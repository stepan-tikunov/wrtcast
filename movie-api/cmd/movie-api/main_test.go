package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAllowAllOrigins(t *testing.T) {
	t.Parallel()

	handler := allowAllOrigins(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	}))

	request := httptest.NewRequest(http.MethodGet, "/v1/movies/movie-id/streams", nil)
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

	request := httptest.NewRequest(http.MethodOptions, "/v1/movies/movie-id/streams", nil)
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
