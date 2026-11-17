package tmdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestExact(t *testing.T) {
	office := []Candidate{
		{ID: 2316, Kind: KindTV, Title: "Офис", OriginalTitle: "The Office", Date: "2005-03-24"},
		{ID: 2996, Kind: KindTV, Title: "Офис", OriginalTitle: "The Office", Date: "2001-07-09"},
	}
	hateful := []Candidate{
		{ID: 273248, Kind: KindMovie, Title: "Омерзительная восьмёрка", OriginalTitle: "The Hateful Eight", Date: "2015-12-25"},
	}
	for _, test := range []struct {
		name       string
		title      Title
		candidates []Candidate
		want       uint64
	}{
		{"by year", Title{Title: "Офис", OriginalTitle: "The Office", Year: "2005", Kind: KindTV}, office, 2316},
		{"no year", Title{Title: "Офис", Kind: KindTV}, office, 0},
		{"ё as е", Title{Title: "Омерзительная восьмерка", Year: "2015", Kind: KindMovie}, hateful, 273248},
		{"original name", Title{Title: "Мерзкая восьмёрка", OriginalTitle: "The Hateful Eight", Year: "2015"}, hateful, 273248},
		{"another year", Title{Title: "Омерзительная восьмерка", Year: "2016"}, hateful, 0},
		{"another kind", Title{Title: "Омерзительная восьмерка", Year: "2015", Kind: KindTV}, hateful, 0},
		{"years", Title{Title: "Офис", Year: "2001-2003", Kind: KindTV}, office, 2996},
	} {
		t.Run(test.name, func(t *testing.T) {
			found, ok := exact(test.title, test.candidates)
			if ok != (test.want != 0) || found.ID != test.want {
				t.Fatalf("exact = %d, %v; want %d", found.ID, ok, test.want)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	if got := normalize("Декстер: Воскрешение!"); got != "декстервоскрешение" {
		t.Fatalf("normalize = %q", got)
	}
	if got := normalize("Ёлки 2"); got != "елки2" {
		t.Fatalf("normalize = %q", got)
	}
}

// A title the search settles is kept without asking Claude, and told by
// Match.
func TestMatchByName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer a.b.c" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/authentication":
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		case "/search/movie":
			results := []item{}
			if r.URL.Query().Get("query") == "Fight Club" {
				results = append(results,
					item{ID: 550, Title: "Бойцовский клуб", OriginalTitle: "Fight Club", ReleaseDate: "1999-10-15"},
					item{ID: 440777, Title: "Женский бойцовский клуб", OriginalTitle: "Female Fight Squad", ReleaseDate: "2017-03-16"})
			}
			json.NewEncoder(w).Encode(map[string]any{"results": results})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	ctx := context.Background()
	service, err := New(ctx, db, func(context.Context) (string, error) {
		t.Fatal("Claude asked")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	service.baseURL = server.URL

	if _, err := service.Update(ctx, "bad"); err != ErrInvalidKey {
		t.Fatalf("Update(bad) = %v", err)
	}
	settings, err := service.Update(ctx, "a.b.c")
	if err != nil || !settings.KeySet || settings.KeyHint != ".b.c" {
		t.Fatalf("Update = %+v, %v", settings, err)
	}
	title := Title{ID: "64aee42615562", Title: "Бойцовский клуб", OriginalTitle: "Fight Club", Year: "1999", Kind: KindMovie}
	service.matchTitle(ctx, title)
	match, ok := service.Match(ctx, title.ID)
	if !ok || match.ID != 550 || match.Kind != KindMovie {
		t.Fatalf("Match = %+v, %v", match, ok)
	}
	if service.due(ctx, title.ID) {
		t.Fatal("a matched title is due")
	}
}

// A title opened that TMDB's search settles is matched at once; one it
// doesn't goes to the background, where WaitMatch waits for it.
func TestOpened(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		results := []item{}
		switch r.URL.Query().Get("query") {
		case "Fight Club":
			results = append(results, item{ID: 550, Title: "Бойцовский клуб", OriginalTitle: "Fight Club", ReleaseDate: "1999-10-15"})
		case "Офис":
			results = append(results,
				item{ID: 2316, Name: "Офис", OriginalName: "The Office", FirstAirDate: "2005-03-24"},
				item{ID: 2996, Name: "Офис", OriginalName: "The Office", FirstAirDate: "2001-07-09"})
		}
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer server.Close()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// No Claude key: what the search can't settle stays unmatched.
	service, err := New(ctx, db, func(context.Context) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	service.baseURL = server.URL
	if _, ok := service.Opened(ctx, Title{ID: "fight", Title: "Бойцовский клуб", OriginalTitle: "Fight Club", Year: "1999", Kind: KindMovie}); ok {
		t.Fatal("matched with no key")
	}
	if err := service.store.setSetting(ctx, "key", "key"); err != nil {
		t.Fatal(err)
	}

	match, ok := service.Opened(ctx, Title{ID: "fight", Title: "Бойцовский клуб", OriginalTitle: "Fight Club", Year: "1999", Kind: KindMovie})
	if !ok || match.ID != 550 || match.Kind != KindMovie {
		t.Fatalf("Opened = %+v, %v", match, ok)
	}
	if match, ok := service.Match(ctx, "fight"); !ok || match.ID != 550 {
		t.Fatalf("Match = %+v, %v", match, ok)
	}

	office := Title{ID: "office", Title: "Офис", Kind: KindTV}
	if _, ok := service.Opened(ctx, office); ok {
		t.Fatal("two Офис matched by name")
	}
	wait, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if _, ok, pending := service.WaitMatch(wait, "office"); ok || !pending {
		t.Fatalf("WaitMatch before Run = %v, %v", ok, pending)
	}
	go service.Run(ctx)
	wait, stop = context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if _, ok, pending := service.WaitMatch(wait, "office"); ok || pending {
		t.Fatalf("WaitMatch = %v, %v", ok, pending)
	}
	if _, ok, pending := service.WaitMatch(ctx, "never opened"); ok || pending {
		t.Fatalf("WaitMatch of a title not opened = %v, %v", ok, pending)
	}
}
