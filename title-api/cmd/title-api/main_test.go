package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
	if methods := response.Header().Get("Access-Control-Allow-Methods"); methods != "GET, DELETE, OPTIONS" {
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

type fakeMasters map[string][]byte

func (masters fakeMasters) MasterPlaylist(_ context.Context, id string) ([]byte, error) {
	switch body, ok := masters[id]; {
	case id == "blocked":
		return nil, provider.ErrCDNBlocked
	case !ok:
		return nil, provider.ErrMasterNotFound
	case body == nil:
		return nil, errors.New("upstream rejected the source")
	default:
		return body, nil
	}
}

func TestMasterPlaylistHandler(t *testing.T) {
	t.Parallel()
	handler := masterPlaylistHandler(fakeMasters{"known": []byte("#EXTM3U\n"), "rejected": nil})
	for _, test := range []struct {
		id     string
		status int
	}{
		{"known", http.StatusOK}, {"missing", http.StatusNotFound},
		{"rejected", http.StatusBadGateway}, {"blocked", http.StatusForbidden},
	} {
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, "/", nil), map[string]string{"id": test.id})
		if response.Code != test.status {
			t.Errorf("%s: status = %d, want %d", test.id, response.Code, test.status)
		}
		if test.status == http.StatusOK && response.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
			t.Errorf("Content-Type = %q", response.Header().Get("Content-Type"))
		}
	}
}

type fakeMediaHeaders struct {
	reports []provider.PlaybackReport
	ended   []string
}

func (f *fakeMediaHeaders) MediaHeaders(_ context.Context, id string, report provider.PlaybackReport) (provider.MediaHeaders, error) {
	switch id {
	case "blocked":
		return provider.MediaHeaders{}, provider.ErrCDNBlocked
	case "known":
		f.reports = append(f.reports, report)
		return provider.MediaHeaders{
			Headers: []provider.ProxyHeader{{Name: "Accepts-Controls", Value: "edge"}, {Name: "Origin", Value: "https://player"}},
			TTL:     time.Minute,
		}, nil
	}
	return provider.MediaHeaders{}, provider.ErrMasterNotFound
}

func (f *fakeMediaHeaders) EndMediaSession(id string) {
	f.ended = append(f.ended, id)
}

func TestMediaHeadersHandler(t *testing.T) {
	t.Parallel()
	source := &fakeMediaHeaders{}
	handler := mediaHeadersHandler(source)
	for _, test := range []struct {
		id, query string
		status    int
	}{
		{"known", "?q=720&t=12.5", http.StatusOK}, {"known", "", http.StatusOK},
		{"known", "?q=720p", http.StatusBadRequest}, {"known", "?t=-1", http.StatusBadRequest},
		{"missing", "", http.StatusNotFound}, {"blocked", "", http.StatusForbidden},
	} {
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, "/v1/media/x/headers"+test.query, nil), map[string]string{"id": test.id})
		if response.Code != test.status {
			t.Errorf("%s%s: status = %d, want %d", test.id, test.query, response.Code, test.status)
		}
		if test.status == http.StatusOK && (response.Header().Get("Content-Type") != "application/json" ||
			response.Body.String() != `{"headers":[{"name":"Accepts-Controls","value":"edge"},{"name":"Origin","value":"https://player"}],"ttl":60}`+"\n") {
			t.Errorf("%s%s: %s %q", test.id, test.query, response.Header().Get("Content-Type"), response.Body.String())
		}
	}
	if len(source.reports) != 2 ||
		source.reports[0] != (provider.PlaybackReport{Resolution: "720", Position: 12.5, HasPosition: true}) ||
		source.reports[1] != (provider.PlaybackReport{}) {
		t.Fatalf("reports = %+v", source.reports)
	}
}

func TestMediaSessionHandler(t *testing.T) {
	t.Parallel()
	source := &fakeMediaHeaders{}
	for range 2 {
		response := httptest.NewRecorder()
		mediaSessionHandler(source)(response, httptest.NewRequest(http.MethodDelete, "/v1/media/known/session", nil), map[string]string{"id": "known"})
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d", response.Code)
		}
	}
	if len(source.ended) != 2 || source.ended[0] != "known" {
		t.Fatalf("ended = %v", source.ended)
	}
}
