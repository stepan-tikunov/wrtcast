package tmdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTMDB answers with the fixtures and counts the requests of each path.
type fakeTMDB struct {
	server *httptest.Server
	mu     sync.Mutex
	hits   map[string]int
	// While set, a request waits for it to close.
	hold chan struct{}
	fail atomic.Bool
}

func newFakeTMDB(t *testing.T) *fakeTMDB {
	fake := &fakeTMDB{hits: map[string]int{}}
	// By path, or by path and language; the rest are not on TMDB.
	files := map[string]string{
		"/movie/550":                          "fight-club",
		"/movie/693134":                       "dune-part-two",
		"/collection/726871":                  "dune-collection",
		"/tv/1405":                            "dexter",
		"/person/819":                         "person-norton",
		"/tv/1405/season/1":                   "dexter-s1",
		"/person/819/images":                  "person-norton-images",
		"/person/819/tagged_images":           "person-norton-tagged",
		"/tv/1405/season/1/episode/2/credits": "dexter-s1e2-credits",
		"/tv/95603/season/1 ru-RU":            "osman-s1",
		"/tv/95603/season/1 en-US":            "osman-s1-en",
	}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.hits[r.URL.Path]++
		hold := fake.hold
		fake.mu.Unlock()
		if hold != nil {
			<-hold
		}
		if r.URL.Query().Get("api_key") != "key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if fake.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/configuration/countries" {
			_, _ = w.Write([]byte(`[{"iso_3166_1":"US","native_name":"Соединенные Штаты"},{"iso_3166_1":"DE","native_name":"Германия"}]`))
			return
		}
		name := files[r.URL.Path+" "+r.URL.Query().Get("language")]
		if name == "" {
			name = files[r.URL.Path]
		}
		if name == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data, err := os.ReadFile("testdata/" + name + ".json")
		if err != nil {
			t.Error(err)
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeTMDB) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fakeTMDB) holding(hold chan struct{}) {
	f.mu.Lock()
	f.hold = hold
	f.mu.Unlock()
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newServingService(t *testing.T, fake *fakeTMDB, key string) (*Service, *clock) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// As title-api opens it: the background fetches write one at a time.
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	service, err := New(ctx, db, func(context.Context) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	service.baseURL = fake.server.URL
	if err := service.store.setSetting(ctx, "key", key); err != nil {
		t.Fatal(err)
	}
	now := &clock{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	service.cache.now = now.Now
	return service, now
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !done(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestDetailsCached(t *testing.T) {
	fake := newFakeTMDB(t)
	service, now := newServingService(t, fake, "key")
	ctx := context.Background()

	details, err := service.Details(ctx, KindMovie, 550)
	if err != nil {
		t.Fatal(err)
	}
	if details.Name != "Бойцовский клуб" || len(details.Countries) != 2 || details.Countries[0] != "Германия" || details.Countries[1] != "США" {
		t.Fatalf("details %q, countries %v", details.Name, details.Countries)
	}
	// The whole cast, from what is kept.
	if cast, err := service.Cast(ctx, KindMovie, 550); err != nil || len(cast) != len(details.Cast) || len(cast) <= CastShown {
		t.Fatalf("cast %d, %v", len(cast), err)
	}
	// Fresh for 30 days, the movie being old.
	now.advance(29 * 24 * time.Hour)
	if _, err := service.Details(ctx, KindMovie, 550); err != nil {
		t.Fatal(err)
	}
	if n := fake.count("/movie/550"); n != 1 {
		t.Fatalf("%d requests while fresh", n)
	}

	// Stale: served at once, fetched again in the background.
	now.advance(2 * 24 * time.Hour)
	hold := make(chan struct{})
	fake.holding(hold)
	if details, err := service.Details(ctx, KindMovie, 550); err != nil || details.Name != "Бойцовский клуб" {
		t.Fatalf("stale: %q, %v", details.Name, err)
	}
	// One refresh at a time.
	if _, err := service.Details(ctx, KindMovie, 550); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the refresh", func() bool { return fake.count("/movie/550") == 2 })
	fake.holding(nil)
	close(hold)
	waitFor(t, "the refresh to be kept", func() bool {
		_, fetched, _, _ := service.cache.read(ctx, detailsKey(KindMovie, 550))
		return fetched.Equal(now.Now().Truncate(time.Second))
	})
	if n := fake.count("/movie/550"); n != 2 {
		t.Fatalf("%d requests", n)
	}

	// Stale while TMDB fails: still served.
	now.advance(31 * 24 * time.Hour)
	fake.fail.Store(true)
	if details, err := service.Details(ctx, KindMovie, 550); err != nil || details.Name != "Бойцовский клуб" {
		t.Fatalf("stale while failing: %q, %v", details.Name, err)
	}
	waitFor(t, "the failed refresh", func() bool { return fake.count("/movie/550") == 3 })
	// A title not kept while TMDB fails is an error.
	if _, err := service.Details(ctx, KindTV, 1405); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("failing: %v", err)
	}
}

func TestDetailsOnce(t *testing.T) {
	fake := newFakeTMDB(t)
	service, _ := newServingService(t, fake, "key")
	hold := make(chan struct{})
	fake.holding(hold)
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for range 5 {
		wg.Go(func() {
			_, err := service.Details(context.Background(), KindTV, 1405)
			errs <- err
		})
	}
	waitFor(t, "the request", func() bool { return fake.count("/tv/1405") == 1 })
	fake.holding(nil)
	close(hold)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := fake.count("/tv/1405"); n != 1 {
		t.Fatalf("%d requests", n)
	}
}

func TestDetailsErrors(t *testing.T) {
	fake := newFakeTMDB(t)
	service, _ := newServingService(t, fake, "key")
	if _, err := service.Details(context.Background(), KindMovie, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("not on TMDB: %v", err)
	}
	service, _ = newServingService(t, fake, "")
	if _, err := service.Details(context.Background(), KindMovie, 550); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
	if _, err := service.Season(context.Background(), 1405, 1); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key: %v", err)
	}
	if n := fake.count("/movie/550"); n != 0 {
		t.Fatalf("%d requests with no key", n)
	}
}

// The collection's parts and the recommendations title-api has are told
// by their titles.
func TestDetailsTitleIDs(t *testing.T) {
	fake := newFakeTMDB(t)
	service, _ := newServingService(t, fake, "key")
	ctx := context.Background()
	for title, id := range map[string]uint64{"dune": 438631, "dune2": 693134} {
		if err := service.store.matched(ctx, title, id, KindMovie, byName, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	// Dune of 1984 as a series is another title.
	if err := service.store.matched(ctx, "dune-tv", 841, KindTV, byName, "", 0); err != nil {
		t.Fatal(err)
	}
	details, err := service.Details(ctx, KindMovie, 693134)
	if err != nil {
		t.Fatal(err)
	}
	if details.TitleID != "dune2" {
		t.Errorf("title %q", details.TitleID)
	}
	if details.Collection == nil || len(details.Collection.Parts) != 3 {
		t.Fatalf("collection %+v", details.Collection)
	}
	for _, part := range details.Collection.Parts {
		want := map[uint64]string{438631: "dune", 693134: "dune2"}[part.ID]
		if part.TitleID != want {
			t.Errorf("part %d: %q, want %q", part.ID, part.TitleID, want)
		}
	}
	for _, r := range details.Recommendations {
		want := map[uint64]string{438631: "dune"}[r.ID]
		if r.TitleID != want {
			t.Errorf("recommendation %d %s: %q, want %q", r.ID, r.Kind, r.TitleID, want)
		}
	}
}

// fakeWikidata answers of Q177311 (Edward Norton), failing while fail is
// set.
func fakeWikidata(t *testing.T, fail *atomic.Bool, hits *atomic.Int32) string {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		query := r.URL.Query()
		if r.Header.Get("User-Agent") == "" || query.Get("format") != "json" {
			t.Errorf("request %s, User-Agent %q", r.URL, r.Header.Get("User-Agent"))
		}
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch query.Get("action") + " " + query.Get("ids") + query.Get("entity") + " " + query.Get("property") {
		case "wbgetentities Q1297|Q30 ":
			if query.Get("props") != "labels" || query.Get("languages") != "ru" {
				t.Errorf("labels %s", r.URL)
			}
			_, _ = w.Write([]byte(`{"entities":{"Q1297":{"labels":{"ru":{"language":"ru","value":"Чикаго"}}},"Q30":{"labels":{"ru":{"language":"ru","value":"США"}}}}}`))
		case "wbgetclaims Q177311 P19":
			_, _ = w.Write([]byte(`{"claims":{"P19":[{"mainsnak":{"datavalue":{"value":{"entity-type":"item","numeric-id":1297,"id":"Q1297"},"type":"wikibase-entityid"}}}]}}`))
		case "wbgetclaims Q1297 P17":
			_, _ = w.Write([]byte(`{"claims":{"P17":[{"mainsnak":{"datavalue":{"value":{"entity-type":"item","numeric-id":30,"id":"Q30"},"type":"wikibase-entityid"}}}]}}`))
		case "wbgetentities Q177311 ":
			if query.Get("sitefilter") != "ruwiki|enwiki" {
				t.Errorf("entities %s", r.URL)
			}
			_, _ = w.Write([]byte(`{"entities":{"Q177311":{"sitelinks":{
				"enwiki":{"site":"enwiki","title":"Edward Norton","url":"https://en.wikipedia.org/wiki/Edward_Norton"},
				"ruwiki":{"site":"ruwiki","title":"Нортон, Эдвард","url":"https://ru.wikipedia.org/wiki/%D0%9D%D0%BE%D1%80%D1%82%D0%BE%D0%BD"}}}}}`))
		case "wbgetclaims Q177311 P2604":
			_, _ = w.Write([]byte(`{"claims":{"P2604":[{"mainsnak":{"snaktype":"value","property":"P2604","datavalue":{"value":"4434","type":"string"}}}]}}`))
		default:
			t.Errorf("action %s", r.URL)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestPerson(t *testing.T) {
	fake := newFakeTMDB(t)
	service, now := newServingService(t, fake, "key")
	var wikidataFails atomic.Bool
	var wikidataHits atomic.Int32
	service.wikidataURL = fakeWikidata(t, &wikidataFails, &wikidataHits)
	ctx := context.Background()
	if err := service.store.matched(ctx, "fight-club", 550, KindMovie, byName, "", 0); err != nil {
		t.Fatal(err)
	}

	// Wikidata failing: kept for a day.
	wikidataFails.Store(true)
	person, err := service.Person(ctx, 819)
	if err != nil {
		t.Fatal(err)
	}
	if person.Name != "Эдвард Нортон" || person.Kinopoisk != "" || person.Wikipedia != "" || !person.Partial ||
		person.PlaceOfBirth != "Boston, Massachusetts, USA" {
		t.Fatalf("person %q, kinopoisk %q, wikipedia %q, partial %t", person.Name, person.Kinopoisk, person.Wikipedia, person.Partial)
	}
	if person.Credits[0].TitleID != "fight-club" || person.Credits[1].TitleID != "" {
		t.Errorf("titles %q, %q", person.Credits[0].TitleID, person.Credits[1].TitleID)
	}
	wikidataFails.Store(false)
	now.advance(25 * time.Hour)
	if _, err := service.Person(ctx, 819); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the refresh to be kept", func() bool {
		data, _, _, _ := service.cache.read(ctx, fmt.Sprintf("person:819:v%d", personVersion))
		return strings.Contains(string(data), `"kinopoisk":"4434"`)
	})
	person, err = service.Person(ctx, 819)
	if err != nil || person.Partial || person.Kinopoisk != "4434" || !strings.HasPrefix(person.Wikipedia, "https://ru.wikipedia.org/") ||
		person.PlaceOfBirth != "Чикаго, США" {
		t.Fatalf("person %+v, %v", person, err)
	}
	// Then for a month.
	now.advance(29 * 24 * time.Hour)
	if _, err := service.Person(ctx, 819); err != nil {
		t.Fatal(err)
	}
	if n := fake.count("/person/819"); n != 2 {
		t.Errorf("%d requests of TMDB", n)
	}
	if n := wikidataHits.Load(); n != 6 {
		t.Errorf("%d requests of Wikidata", n)
	}

	if _, err := service.Person(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such person: %v", err)
	}
}

func TestSeason(t *testing.T) {
	fake := newFakeTMDB(t)
	service, _ := newServingService(t, fake, "key")
	ctx := context.Background()

	dexter, err := service.Season(ctx, 1405, 1)
	if err != nil || len(dexter.Episodes) != 12 {
		t.Fatalf("dexter: %d episodes, %v", len(dexter.Episodes), err)
	}
	if n := fake.count("/tv/1405/season/1"); n != 1 {
		t.Errorf("%d requests, the overviews in Russian", n)
	}
	// No overviews in Russian: the English ones.
	osman, err := service.Season(ctx, 95603, 1)
	if err != nil || len(osman.Episodes) != 27 {
		t.Fatalf("osman: %d episodes, %v", len(osman.Episodes), err)
	}
	if osman.Episodes[0].Overview == "" || osman.Episodes[0].Name != "" || fake.count("/tv/95603/season/1") != 2 {
		t.Errorf("osman %+v", osman.Episodes[0])
	}
	// Dexter's 9th season of the provider is a series of its own on TMDB.
	none, err := service.Season(ctx, 1405, 9)
	if err != nil || len(none.Episodes) != 0 {
		t.Fatalf("season 9: %+v, %v", none, err)
	}
}

func TestUpcoming(t *testing.T) {
	fake := newFakeTMDB(t)
	service, now := newServingService(t, fake, "key")
	ctx := context.Background()
	if err := service.store.matched(ctx, "dexter", 1405, KindTV, byName, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := service.store.matched(ctx, "fight", 550, KindMovie, byName, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := service.Upcoming(ctx, "other"); ok {
		t.Error("a title not matched")
	}
	if match, series, ok := service.Upcoming(ctx, "fight"); !ok || match.Kind != KindMovie || series != nil {
		t.Errorf("a movie: %+v, %+v, %v", match, series, ok)
	}
	// Not kept: nothing yet, fetched in the background.
	if match, series, ok := service.Upcoming(ctx, "dexter"); !ok || match.ID != 1405 || series != nil {
		t.Fatalf("not kept: %+v, %+v, %v", match, series, ok)
	}
	waitFor(t, "the details to be kept", func() bool {
		_, _, ok, _ := service.cache.read(ctx, detailsKey(KindTV, 1405))
		return ok
	})
	if _, series, _ := service.Upcoming(ctx, "dexter"); series == nil || series.Status != StatusEnded || series.Next != nil {
		t.Fatalf("kept: %+v", series)
	}
	// Past its time: as kept, fetched again in the background.
	now.advance(31 * 24 * time.Hour)
	if _, series, _ := service.Upcoming(ctx, "dexter"); series == nil {
		t.Fatal("stale: nothing")
	}
	waitFor(t, "the refresh", func() bool { return fake.count("/tv/1405") == 2 })
}
