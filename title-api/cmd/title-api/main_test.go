package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
		{"known", "?q=1080&t=30&p=1&w=0&d=2640.04&b=123456789&e=8000000", http.StatusOK},
		{"known", "?q=720p", http.StatusBadRequest}, {"known", "?t=-1", http.StatusBadRequest},
		{"known", "?p=yes", http.StatusBadRequest}, {"known", "?b=-5", http.StatusBadRequest},
		{"known", "?d=1e3", http.StatusBadRequest}, {"known", "?e=1.5", http.StatusBadRequest},
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
	if len(source.reports) != 3 ||
		source.reports[0] != (provider.PlaybackReport{Resolution: "720", Position: 12.5, HasPosition: true}) ||
		source.reports[1] != (provider.PlaybackReport{}) ||
		source.reports[2] != (provider.PlaybackReport{
			Resolution: "1080", Position: 30, HasPosition: true, Paused: true, HasPaused: true, HasBuffering: true,
			Duration: 2640.04, Bytes: 123456789, HasBytes: true, Bandwidth: 8000000,
		}) {
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

type fakeRelay struct{}

func (fakeRelay) FetchMedia(_ context.Context, id, rawURL string, _ bool) (*http.Response, error) {
	switch {
	case id != "known":
		return nil, provider.ErrMasterNotFound
	case rawURL == "https://cdn.example/seg.ts":
		return &http.Response{
			StatusCode: http.StatusOK, ContentLength: 5,
			Header: http.Header{"Content-Type": {"video/mp2t"}, "Set-Cookie": {"a=b"}},
			Body:   io.NopCloser(strings.NewReader("bytes")),
		}, nil
	case rawURL == "https://cdn.example/blocked.ts":
		return &http.Response{
			StatusCode: http.StatusForbidden, ContentLength: -1,
			Header: http.Header{"X-Vd": {"client_blocked"}}, Body: io.NopCloser(strings.NewReader("")),
		}, nil
	}
	return nil, provider.ErrMediaURL
}

func (fakeRelay) QualityPlaylist(_ context.Context, id, quality string) ([]byte, error) {
	if id != "known" || quality != "720" {
		return nil, provider.ErrMasterNotFound
	}
	return []byte("#EXTM3U\n"), nil
}

// The relay passes the CDN's status, type and X-VD on and streams the
// body; URLs not of the media are refused.
func TestMediaRelayHandler(t *testing.T) {
	t.Parallel()
	handler := mediaRelayHandler(fakeRelay{})
	for _, test := range []struct {
		id, target, body, vd string
		status               int
	}{
		{"known", "https://cdn.example/seg.ts", "bytes", "", http.StatusOK},
		{"known", "https://cdn.example/blocked.ts", "", "client_blocked", http.StatusForbidden},
		{"known", "https://elsewhere.example/seg.ts", "", "", http.StatusBadRequest},
		{"missing", "https://cdn.example/seg.ts", "", "", http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, "/v1/media/x/cdn?u="+url.QueryEscape(test.target), nil), map[string]string{"id": test.id})
		if response.Code != test.status || response.Header().Get("X-VD") != test.vd ||
			test.status == http.StatusOK && (response.Body.String() != test.body || response.Header().Get("Content-Type") != "video/mp2t" ||
				response.Header().Get("Content-Length") != "5" || response.Header().Get("Set-Cookie") != "") {
			t.Errorf("%s %s: %d %v %q", test.id, test.target, response.Code, response.Header(), response.Body.String())
		}
	}
	playlist := qualityPlaylistHandler(fakeRelay{})
	for quality, status := range map[string]int{"720": http.StatusOK, "1080": http.StatusNotFound, "x": http.StatusBadRequest} {
		response := httptest.NewRecorder()
		playlist(response, httptest.NewRequest(http.MethodGet, "/", nil), map[string]string{"id": "known", "quality": quality})
		if response.Code != status {
			t.Errorf("quality %s: %d", quality, response.Code)
		}
	}
}
