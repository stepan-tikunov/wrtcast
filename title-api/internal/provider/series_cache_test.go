package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientSeriesKept(t *testing.T) {
	t.Parallel()

	var serverURL string
	var pages atomic.Int32
	slow := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = response.Write([]byte("ready"))
		case "/index.php":
			pages.Add(1)
			_, _ = fmt.Fprintf(response, `<iframe src="%s/embed/movie/%s"></iframe>`, serverURL, request.URL.Query().Get("newsid"))
		case "/embed/movie/20973", "/embed/movie/30000":
			if request.URL.Path == "/embed/movie/30000" {
				<-slow
			}
			_, _ = response.Write([]byte(`makePlayer({playlist:{seasons:[{"season":1,"episodes":[{"episode":"1","hls":"x/master.m3u8","title":"Pilot"}]}]}}); var lok=1,x="marker";`))
		case "/embed/movie/42185":
			_, _ = response.Write([]byte(`makePlayer({source:{hls:"x/master.m3u8"}}); var lok=1,x="marker";`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	serverURL = server.URL

	client, err := NewClient(Config{BaseURL: server.URL, RefreshInterval: time.Hour, HTTPTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Asked at once and again: fetched once.
	series := client.ExternalTitleID("20973")
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if seasons, err := client.Series(ctx, series); err != nil || len(seasons) != 1 {
				t.Errorf("Series = %+v, %v", seasons, err)
			}
		})
	}
	wg.Wait()
	if _, err := client.Series(ctx, series); err != nil || pages.Load() != 1 {
		t.Errorf("Series again: %v, %d pages", err, pages.Load())
	}

	// A movie is kept as one.
	movie := client.ExternalTitleID("42185")
	for range 2 {
		if _, err := client.Series(ctx, movie); !errors.Is(err, ErrNotSeries) {
			t.Errorf("movie: %v", err)
		}
	}
	if pages.Load() != 2 {
		t.Errorf("%d pages after the movie", pages.Load())
	}

	// Given up on, the fetch goes on for the next caller.
	late := client.ExternalTitleID("30000")
	wait, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if _, err := client.Series(wait, late); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("given up: %v", err)
	}
	close(slow)
	if seasons, err := client.Series(ctx, late); err != nil || len(seasons) != 1 || pages.Load() != 3 {
		t.Errorf("late Series = %+v, %v, %d pages", seasons, err, pages.Load())
	}
	if _, err := client.Series(ctx, late); err != nil || pages.Load() != 3 {
		t.Errorf("late Series again: %v, %d pages", err, pages.Load())
	}
}
