package tmdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// fakeProvider is the provider's site: what its search finds by query and
// the titles' pages, counting the requests.
type fakeProvider struct {
	found  map[string][]Found
	titles map[string]Title
	fail   bool
	// How long a title's page takes to open.
	slow map[string]time.Duration

	mu       sync.Mutex
	searches []string
	opened   []string
}

func (p *fakeProvider) Search(_ context.Context, query string) ([]Found, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.searches = append(p.searches, query)
	if p.fail {
		return nil, errors.New("the provider is down")
	}
	return p.found[query], nil
}

func (p *fakeProvider) Title(ctx context.Context, id string) (Title, error) {
	select {
	case <-time.After(p.slow[id]):
	case <-ctx.Done():
		return Title{}, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opened = append(p.opened, id)
	t, ok := p.titles[id]
	if !ok {
		return Title{}, errors.New("no such title")
	}
	return t, nil
}

func (p *fakeProvider) requests() (int, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.searches), append([]string(nil), p.opened...)
}

// lookupService serves Fight Club's details (movie/550) and finds, of the
// searches TMDB is asked, Fight Club and a movie of the same name a year
// later (movie/551).
func lookupService(t *testing.T, provider *fakeProvider) (*Service, context.Context) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/movie/550":
			data, err := os.ReadFile("testdata/fight-club.json")
			if err != nil {
				t.Error(err)
			}
			_, _ = w.Write(data)
		case "/search/movie":
			results := []item{}
			switch r.URL.Query().Get("query") {
			case "Fight Club", "Бойцовский клуб":
				results = append(results, item{ID: 550, Title: "Бойцовский клуб", OriginalTitle: "Fight Club", ReleaseDate: "1999-10-15"})
			case "Бойцовский клуб 2":
				results = append(results, item{ID: 551, Title: "Бойцовский клуб 2", ReleaseDate: "2000-01-01"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // as title-api's state database
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service, err := New(ctx, db, func(context.Context) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	service.baseURL = server.URL
	if err := service.store.setSetting(ctx, "key", "key"); err != nil {
		t.Fatal(err)
	}
	service.UseProvider(provider)
	go service.Run(ctx)
	return service, ctx
}

func fightClub(id string) Title {
	return Title{ID: id, Title: "Бойцовский клуб", OriginalTitle: "Fight Club", Year: "1999", Kind: KindMovie}
}

func TestLookupFound(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{
		found: map[string][]Found{
			// Both names find the same title; the sequel is found by the
			// Russian one, named otherwise: opened as well, at once.
			"Бойцовский клуб": {{ID: "sequel", Title: "Бойцовский клуб 2", Year: "2000"}, {ID: "fight", Title: "Бойцовский клуб", Year: "1999"}},
			"Fight Club":      {{ID: "fight", Title: "Бойцовский клуб", Year: "1999"}},
		},
		titles: map[string]Title{
			"fight":  fightClub(""),
			"sequel": {Title: "Бойцовский клуб 2", Year: "2000", Kind: KindMovie},
		},
	}
	service, ctx := lookupService(t, provider)
	state, title, err := service.Lookup(ctx, KindMovie, 550)
	if err != nil || state != LookupFound || title != "fight" {
		t.Fatalf("Lookup = %s, %q, %v", state, title, err)
	}
	searches, opened := provider.requests()
	if slices.Sort(opened); searches != 2 || !slices.Equal(opened, []string{"fight", "sequel"}) {
		t.Errorf("%d searches, opened %v", searches, opened)
	}
	if match, ok := service.Match(ctx, "fight"); !ok || match.ID != 550 {
		t.Errorf("Match = %+v, %v", match, ok)
	}
	// Matched, it is found at once.
	if state, title, err := service.Lookup(ctx, KindMovie, 550); state != LookupFound || title != "fight" || err != nil {
		t.Errorf("Lookup again = %s, %q, %v", state, title, err)
	}
	if searches, _ := provider.requests(); searches != 2 {
		t.Errorf("%d searches after", searches)
	}
	if details, err := service.Details(ctx, KindMovie, 550); err != nil || details.TitleID != "fight" {
		t.Errorf("Details = %q, %v", details.TitleID, err)
	}
}

func TestLookupLikeliest(t *testing.T) {
	t.Parallel()
	// Two of the provider's titles are Fight Club: the one the search names
	// as it is found, though the other is matched first.
	provider := &fakeProvider{
		found: map[string][]Found{"Fight Club": {
			{ID: "copy", Title: "Бойцовский клуб (копия)", Year: "1999"},
			{ID: "fight", Title: "Бойцовский клуб", Year: "1999"},
		}},
		titles: map[string]Title{"fight": fightClub(""), "copy": fightClub("")},
		slow:   map[string]time.Duration{"fight": 200 * time.Millisecond},
	}
	service, ctx := lookupService(t, provider)
	if state, title, err := service.Lookup(ctx, KindMovie, 550); state != LookupFound || title != "fight" || err != nil {
		t.Fatalf("Lookup = %s, %q, %v", state, title, err)
	}
}

func TestLookupNotFound(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{
		found: map[string][]Found{
			"Бойцовский клуб": {
				// Matched with another title.
				{ID: "sequel", Title: "Бойцовский клуб 2", Year: "2000"},
				// Of another year.
				{ID: "old", Title: "Бойцовский клуб", Year: "1980"},
				// Not on TMDB.
				{ID: "other", Title: "Бойцовский клуб", Year: "1999"},
			},
		},
		titles: map[string]Title{
			"sequel": {Title: "Бойцовский клуб 2", Year: "2000", Kind: KindMovie},
			"other":  {Title: "Клуб", Year: "1999", Kind: KindMovie},
		},
	}
	service, ctx := lookupService(t, provider)
	now := time.Now()
	service.now = func() time.Time { return now }
	if state, _, err := service.Lookup(ctx, KindMovie, 550); state != LookupNotFound || err != nil {
		t.Fatalf("Lookup = %s, %v", state, err)
	}
	if _, opened := provider.requests(); len(opened) != 2 {
		t.Errorf("opened %v", opened)
	}
	// Not looked for again within a week.
	if state, _, _ := service.Lookup(ctx, KindMovie, 550); state != LookupNotFound {
		t.Errorf("Lookup again = %s", state)
	}
	if searches, _ := provider.requests(); searches != 2 {
		t.Errorf("%d searches", searches)
	}
	now = now.Add(notFoundAgain)
	if state, _, _ := service.Lookup(ctx, KindMovie, 550); state != LookupNotFound {
		t.Errorf("Lookup a week later = %s", state)
	}
	if searches, _ := provider.requests(); searches != 4 {
		t.Errorf("%d searches a week later", searches)
	}
}

func TestLookupLimit(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{found: map[string][]Found{}, titles: map[string]Title{}}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		provider.found["Fight Club"] = append(provider.found["Fight Club"], Found{ID: id, Title: "Бойцовский клуб", Year: "1999"})
		provider.titles[id] = Title{Title: "Клуб", Year: "1999", Kind: KindMovie}
	}
	service, ctx := lookupService(t, provider)
	if state, _, _ := service.Lookup(ctx, KindMovie, 550); state != LookupNotFound {
		t.Fatalf("Lookup = %s", state)
	}
	if _, opened := provider.requests(); len(opened) != lookupCandidates {
		t.Errorf("opened %v", opened)
	}
}

func TestLookupFailed(t *testing.T) {
	t.Parallel()
	provider := &fakeProvider{fail: true}
	service, ctx := lookupService(t, provider)
	now := time.Now()
	service.now = func() time.Time { return now }
	if state, _, _ := service.Lookup(ctx, KindMovie, 550); state != LookupNotFound {
		t.Fatalf("Lookup = %s", state)
	}
	if state, _, _ := service.Lookup(ctx, KindMovie, 550); state != LookupNotFound {
		t.Fatalf("Lookup again = %s", state)
	}
	// Both names asked at once.
	if searches, _ := provider.requests(); searches != 2 {
		t.Errorf("%d searches", searches)
	}
	// Sooner than one not found.
	now = now.Add(failedAgain)
	service.Lookup(ctx, KindMovie, 550)
	if searches, _ := provider.requests(); searches != 4 {
		t.Errorf("%d searches half an hour later", searches)
	}
}

func TestLookupPending(t *testing.T) {
	t.Parallel()
	service, ctx := lookupService(t, &fakeProvider{})
	service.UseProvider(nil)
	if _, _, err := service.Lookup(ctx, KindMovie, 550); !errors.Is(err, ErrNoProvider) {
		t.Errorf("no provider: %v", err)
	}
	// Queued while the worker is busy: pending once the wait ends.
	service.UseProvider(&fakeProvider{})
	service.mu.Lock()
	service.lookingFor = lookup{KindMovie, 550}
	service.mu.Unlock()
	wait, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if state, _, err := service.Lookup(wait, KindMovie, 550); state != LookupPending || err != nil {
		t.Errorf("Lookup = %s, %v", state, err)
	}
}
