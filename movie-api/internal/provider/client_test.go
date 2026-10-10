package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientSolvesAndRefreshesChallenge(t *testing.T) {
	t.Parallel()

	var code atomic.Int64
	code.Store(127)
	var serverURL string
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/", "/index.php":
			currentCode := code.Load()
			hash, err := challengeHash(request.Context(), currentCode)
			if err != nil {
				t.Error(err)
				response.WriteHeader(http.StatusInternalServerError)
				return
			}
			cookie := request.Header.Get("Cookie")
			valid := strings.Contains(cookie, fmt.Sprintf("__js_p_=%d,1800,0,0,0", currentCode)) &&
				strings.Contains(cookie, "__jhash_="+strconv.Itoa(hash)) &&
				strings.Contains(cookie, "__jua_=")
			if !valid {
				response.Header().Add("Set-Cookie", fmt.Sprintf("__js_p_=%d,1800,0,0,0; Path=/", currentCode))
				_, _ = response.Write([]byte(`<script>function get_jhash(){}; var cookie = "__js_p_";</script>`))
				return
			}
			if request.URL.Path == "/" {
				_, _ = response.Write([]byte("ready"))
				return
			}
			if request.URL.Query().Get("newsid") != "42185" {
				response.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprintf(response, `<iframe src="%s/embed/movie/36036"></iframe><iframe src="%s/embed/movie/36037"></iframe>`, serverURL, serverURL)
		case "/embed/movie/36036":
			_, _ = fmt.Fprintf(response, `
makePlayer({source:{hls:"%s/master.m3u8?token=base",audio:{"names":["Dub"]},cc:[{"url":"%s/subtitles-ru.vtt","name":"Рус. полные"}]}});
var lok = 1, challengeMarker = "marker";`, serverURL, serverURL)
		case "/embed/movie/36037":
			_, _ = fmt.Fprintf(response, `
makePlayer({source:{hls:"%s/master-2.m3u8?token=base",audio:{names:["Original"]},cc:[]}});
var lok = 1, challengeMarker = "marker";`, serverURL)
		case "/master.m3u8":
			if _, ok := request.URL.Query()["marker"]; !ok {
				response.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = response.Write([]byte(`#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio0",NAME="rus0",DEFAULT=YES,LANGUAGE="ru",URI="audio.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=1000,CODECS="avc1,mp4a",RESOLUTION=1280x720,AUDIO="audio0"
video.m3u8
`))
		case "/master-2.m3u8":
			_, _ = response.Write([]byte(`#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio0",NAME="eng0",DEFAULT=YES,LANGUAGE="en",URI="audio-2.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=2000,CODECS="avc1,mp4a",RESOLUTION=1920x1080,AUDIO="audio0"
video-2.m3u8
`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	serverURL = server.URL

	client, err := NewClient(Config{
		BaseURL:         server.URL,
		RefreshInterval: time.Hour,
		HTTPTimeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("Start(): %v", err)
	}

	movieID := client.ExternalMovieID("42185")
	streams, err := client.Streams(ctx, movieID)
	if err != nil {
		t.Fatalf("Streams() before refresh: %v", err)
	}
	if len(streams) != 2 || streams[0].AudioTracks[0].Name != "Dub" ||
		len(streams[0].Subtitles) != 1 || streams[0].Subtitles[0].Language != "ru" ||
		streams[1].AudioTracks[0].Language != "en" {
		t.Fatalf("unexpected streams: %+v", streams)
	}

	code.Store(161)
	streams, err = client.Streams(ctx, movieID)
	if err != nil {
		t.Fatalf("Streams() after forced challenge: %v", err)
	}
	if len(streams) != 2 {
		t.Fatalf("got %d streams after refresh, want 2", len(streams))
	}
}

func TestClientSearch(t *testing.T) {
	t.Parallel()

	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = response.Write([]byte("ready"))
		case "/search-result/":
			if request.Method != http.MethodPost {
				response.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if err := request.ParseForm(); err != nil {
				t.Error(err)
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			if request.Form.Get("do") != "search" || request.Form.Get("story") != "Дюна" {
				t.Errorf("unexpected search form: %v", request.Form)
			}
			_, _ = fmt.Fprintf(response, `
<div class="th-item">
  <a class="th-in with-mask" href="%s/filmy/42185-djuna-2021.html">
    <div class="th-img"><img data-src="/uploads/dune.jpg" alt="Дюна"></div>
    <div class="th-title">Дюна</div><div class="th-year">2021</div>
  </a>
</div>
<div class="th-item">
  <a href="/serialy/51890-djuna-prorochestvo-2024.html">
    <img src="https://cdn.example/prophecy.jpg" alt="Дюна: Пророчество">
    <div class="th-title">Дюна: Пророчество</div><span class="year">2024</span>
  </a>
</div>`, serverURL)
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	serverURL = server.URL

	client, err := NewClient(Config{BaseURL: server.URL, RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	results, err := client.Search(ctx, "  Дюна  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(results), results)
	}
	if results[0].MovieID != client.ExternalMovieID("42185") || results[0].Title != "Дюна" ||
		results[0].Year != "2021" || results[0].PosterURL != serverURL+"/uploads/dune.jpg" {
		t.Fatalf("unexpected first result: %+v", results[0])
	}
	if results[1].PosterURL != "https://cdn.example/prophecy.jpg" {
		t.Fatalf("unexpected second result: %+v", results[1])
	}
}

func TestClientSearchRejectsShortQuery(t *testing.T) {
	t.Parallel()
	client, err := NewClient(Config{BaseURL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Search(context.Background(), "Дю")
	if !errors.Is(err, ErrInvalidSearchQuery) {
		t.Fatalf("Search() error = %v, want ErrInvalidSearchQuery", err)
	}
}
