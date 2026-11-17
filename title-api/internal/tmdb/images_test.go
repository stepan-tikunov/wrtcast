package tmdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestImage(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/w500/missing00000.jpg" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(r.URL.Path))
	}))
	defer server.Close()
	dir := t.TempDir()
	images, err := NewImages(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	images.baseURL = server.URL
	ctx := context.Background()

	for range 2 {
		data, contentType, err := images.Image(ctx, "w500", "8uO0gUM8aNqYLs1OsTBQiXu0fEv.png")
		if err != nil || string(data) != "/w500/8uO0gUM8aNqYLs1OsTBQiXu0fEv.png" || contentType != "image/png" {
			t.Fatalf("image %q %s, %v", data, contentType, err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("%d requests, the image kept", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "w500_8uO0gUM8aNqYLs1OsTBQiXu0fEv.png")); err != nil {
		t.Fatal(err)
	}
	if _, contentType, _ := images.Image(ctx, "w185", "c6OLXfKAk5BKeR6broC8pYiCquX.jpg"); contentType != "image/jpeg" {
		t.Errorf("type %s", contentType)
	}
	if _, _, err := images.Image(ctx, "w500", "missing00000.jpg"); !errors.Is(err, ErrImageNotFound) {
		t.Errorf("missing: %v", err)
	}

	hits.Store(0)
	for _, test := range []struct{ size, file string }{
		{"original", "8uO0gUM8aNqYLs1OsTBQiXu0fEv.jpg"},
		{"w501", "8uO0gUM8aNqYLs1OsTBQiXu0fEv.jpg"},
		{"w500", "8uO0gUM8aNqYLs1OsTBQiXu0fEv.svg"},
		{"w500", "short.jpg"},
		{"w500", "../../../../etc/passwd.jpg"},
		{"w500", "..%2F..%2Fstate.db.jpg"},
		{"w500", "a/8uO0gUM8aNqYLs1OsTBQiXu0fEv.jpg"},
		{"w500", "/8uO0gUM8aNqYLs1OsTBQiXu0fEv.jpg"},
		{"w500/..", "8uO0gUM8aNqYLs1OsTBQiXu0fEv.jpg"},
		{"w500", ""},
	} {
		if _, _, err := images.Image(ctx, test.size, test.file); !errors.Is(err, ErrInvalidImage) {
			t.Errorf("%s/%s: %v", test.size, test.file, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests for invalid images", n)
	}
}

// The pictures least recently served go first, down to nine tenths of the
// limit.
func TestImagesTrim(t *testing.T) {
	dir := t.TempDir()
	images, err := NewImages(dir, 1000)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, name := range []string{"a", "b", "c", "d", ".tmp-1"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, 300), 0o644); err != nil {
			t.Fatal(err)
		}
		used := now.Add(time.Duration(i-10) * time.Hour)
		if err := os.Chtimes(path, used, used); err != nil {
			t.Fatal(err)
		}
	}
	if err := images.trim(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var left []string
	for _, entry := range entries {
		left = append(left, entry.Name())
	}
	if len(left) != 3 || left[0] != "b" || left[2] != "d" {
		t.Fatalf("left %v", left)
	}

	// Under the limit, nothing goes.
	if err := images.trim(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Fatalf("%d left", len(entries))
	}
}

// A picture served is used anew.
func TestImageTouched(t *testing.T) {
	dir := t.TempDir()
	images, err := NewImages(dir, 1000)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "w300_8uO0gUM8aNqYLs1OsTBQiXu0fEv.jpg")
	if err := os.WriteFile(path, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if data, _, err := images.Image(context.Background(), "w300", "8uO0gUM8aNqYLs1OsTBQiXu0fEv.jpg"); err != nil || string(data) != "kept" {
		t.Fatalf("%q, %v", data, err)
	}
	if info, err := os.Stat(path); err != nil || time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("not touched: %v", err)
	}
}
