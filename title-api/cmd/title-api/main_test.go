package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"title-api/internal/provider"
)

func TestAllowAllOrigins(t *testing.T) {
	t.Parallel()

	handler := allowAllOrigins(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	}))

	request := httptest.NewRequest(http.MethodGet, "/v1/titles/title-id/playback-streams", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if origin := response.Header().Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want %q", origin, "*")
	}
}

func TestAllowAllOriginsHandlesPreflight(t *testing.T) {
	t.Parallel()

	called := false
	handler := allowAllOrigins(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	request := httptest.NewRequest(http.MethodOptions, "/v1/titles/title-id/playback-streams", nil)
	request.Header.Set("Origin", "https://example.com")
	request.Header.Set("Access-Control-Request-Method", http.MethodGet)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if called {
		t.Fatal("preflight request reached the wrapped handler")
	}
	if methods := response.Header().Get("Access-Control-Allow-Methods"); methods != "GET, OPTIONS" {
		t.Fatalf("Access-Control-Allow-Methods = %q", methods)
	}
	if headers := response.Header().Get("Access-Control-Allow-Headers"); headers != "*" {
		t.Fatalf("Access-Control-Allow-Headers = %q", headers)
	}
}

type fakePosters map[string][]byte

func (f fakePosters) Poster(_ context.Context, rawURL string) ([]byte, string, error) {
	if rawURL == "bad" {
		return nil, "", provider.ErrInvalidPosterURL
	}
	body, ok := f[rawURL]
	if !ok {
		return nil, "", errors.New("upstream failed")
	}
	return body, "image/jpeg", nil
}

func TestPosterHandler(t *testing.T) {
	t.Parallel()

	handler := posterHandler(fakePosters{"https://site/p.jpg": []byte("jpeg")})
	for _, test := range []struct {
		url    string
		status int
		body   string
	}{
		{"https://site/p.jpg", http.StatusOK, "jpeg"},
		{"bad", http.StatusBadRequest, ""},
		{"https://site/missing.jpg", http.StatusBadGateway, ""},
	} {
		request := httptest.NewRequest(http.MethodGet, "/v1/posters?url="+test.url, nil)
		response := httptest.NewRecorder()
		handler(response, request, nil)
		if response.Code != test.status {
			t.Errorf("%s: status = %d, want %d", test.url, response.Code, test.status)
		}
		if test.status == http.StatusOK {
			if got := response.Header().Get("Content-Type"); got != "image/jpeg" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := response.Body.String(); got != test.body {
				t.Errorf("body = %q, want %q", got, test.body)
			}
		}
	}
}
