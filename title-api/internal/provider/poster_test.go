package provider

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

func TestClientPoster(t *testing.T) {
	t.Parallel()

	// The first poster request hits an expired session and gets the challenge
	// page; the client must refresh the session and retry.
	var challenged atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = response.Write([]byte("ready"))
		case "/uploads/dune.jpg":
			if request.Header.Get("Referer") == "" {
				t.Error("poster request has no Referer")
			}
			if !challenged.Swap(true) {
				_, _ = response.Write([]byte(`<script>function get_jhash(){}; var cookie = "__js_p_";</script>`))
				return
			}
			_, _ = response.Write(testPNG)
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	body, contentType, err := client.Poster(ctx, server.URL+"/uploads/dune.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "image/png" || !bytes.Equal(body, testPNG) {
		t.Fatalf("Poster() = %q, %q", contentType, body)
	}

	_, _, err = client.Poster(ctx, server.URL+"/missing.jpg")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("Poster(missing) error = %v, want 404", err)
	}
}

func TestClientPosterUsesCurrentHost(t *testing.T) {
	t.Parallel()

	// Saved poster links point at hosts the provider has since moved away
	// from; only their path and query are fetched, from the current host.
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = response.Write([]byte("ready"))
		case "/uploads/posts/2019-11/dune.jpg":
			if request.URL.RawQuery != "v=2" {
				t.Errorf("poster query = %q, want v=2", request.URL.RawQuery)
			}
			_, _ = response.Write(testPNG)
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{
		"https://gx.lordfilm5.pro/uploads/posts/2019-11/dune.jpg?v=2",
		"http://evil.example/uploads/posts/2019-11/dune.jpg?v=2",
	} {
		body, contentType, err := client.Poster(ctx, target)
		if err != nil {
			t.Fatalf("Poster(%q) error = %v", target, err)
		}
		if contentType != "image/png" || !bytes.Equal(body, testPNG) {
			t.Fatalf("Poster(%q) = %q, %q", target, contentType, body)
		}
	}
}

func TestClientPosterRejectsInvalidURL(t *testing.T) {
	t.Parallel()
	client, err := NewClient(Config{BaseURL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"",
		"::",
		"/uploads/poster.jpg",
		"ftp://example.com/poster.jpg",
		"file:///etc/passwd",
		"https://example.com",
		"https://example.com/",
	} {
		_, _, err := client.Poster(context.Background(), target)
		if !errors.Is(err, ErrInvalidPosterURL) {
			t.Errorf("Poster(%q) error = %v, want ErrInvalidPosterURL", target, err)
		}
	}
}
