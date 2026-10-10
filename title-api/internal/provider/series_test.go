package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientSeriesAndEpisodeStreams(t *testing.T) {
	t.Parallel()

	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = response.Write([]byte("ready"))
		case "/index.php":
			switch request.URL.Query().Get("newsid") {
			case "20973":
				_, _ = fmt.Fprintf(response, `<iframe src="%s/embed/movie/378"></iframe>`, serverURL)
			case "42185":
				_, _ = fmt.Fprintf(response, `<iframe src="%s/embed/movie/36036"></iframe>`, serverURL)
			default:
				response.WriteHeader(http.StatusNotFound)
			}
		case "/embed/movie/378":
			_, _ = fmt.Fprintf(response, `
makePlayer({playlist:{seasons:[
  {"season":8,"blocked":false,"episodes":[
    {"episode":"10","hls":"%s/s8e10/master.m3u8?token=base","audio":{"names":["Dub"]},"duration":2600,"title":"Episode 10"},
    {"episode":"1","hls":"%s/s8e1/master.m3u8?token=base","audio":{"names":["LostFilm","Eng.Original"]},"cc":[{"url":"%s/subtitles/ru.vtt","name":"Рус. полные"}],"duration":2594,"title":"House \"]\""},
    {"episode":"2","hls":"%s/s8e2/master.m3u8?token=base","audio":{"names":["Dub"]},"duration":2580,"title":"Episode 2"}
  ]},
  {"season":"1","blocked":false,"episodes":[
    {"episode":2,"hls":"%s/s1e2/master.m3u8?token=base","duration":2500,"title":"Pilot 2"},
    {"episode":1,"hls":"%s/s1e1/master.m3u8?token=base","duration":2400,"title":"Pilot"}
  ]},
  {"season":9,"blocked":true,"episodes":[{"episode":"1","title":"Blocked"}]}
]}});
var lok = 1, challengeMarker = "marker";`, serverURL, serverURL, serverURL, serverURL, serverURL, serverURL)
		case "/embed/movie/36036":
			_, _ = fmt.Fprintf(response, `
makePlayer({source:{hls:"%s/movie/master.m3u8?token=base",audio:{names:["Dub"]}}});
var lok = 1, challengeMarker = "marker";`, serverURL)
		case "/s8e1/master.m3u8":
			if _, ok := request.URL.Query()["marker"]; !ok {
				response.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = response.Write([]byte(`#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio0",NAME="rus0",DEFAULT=YES,LANGUAGE="ru",URI="audio-rus.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio0",NAME="eng1",DEFAULT=NO,LANGUAGE="en",URI="audio-eng.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=1000,AUDIO="audio0"
video.m3u8
`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
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
		t.Fatal(err)
	}

	seriesID := client.ExternalTitleID("20973")
	seasons, err := client.Series(ctx, seriesID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seasons) != 2 || seasons[0].Number != 1 || seasons[1].Number != 8 {
		t.Fatalf("unexpected seasons: %+v", seasons)
	}
	if episodes := seasons[1].Episodes; len(episodes) != 3 ||
		episodes[0].Number != "1" || episodes[1].Number != "2" || episodes[2].Number != "10" ||
		episodes[0].DurationSeconds != 2594 || episodes[0].Title != `House "]"` {
		t.Fatalf("unexpected episodes: %+v", episodes)
	}

	streams, err := client.EpisodeStreams(ctx, seriesID, 8, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 1 || !strings.Contains(streams[0].MasterPlaylistURL, "marker") ||
		len(streams[0].AudioTracks) != 2 || streams[0].AudioTracks[0].Name != "LostFilm" ||
		len(streams[0].Subtitles) != 1 || streams[0].Subtitles[0].Language != "ru" {
		t.Fatalf("unexpected streams: %+v", streams)
	}

	if _, err := client.EpisodeStreams(ctx, seriesID, 8, "404"); !errors.Is(err, ErrEpisodeNotFound) {
		t.Fatalf("missing episode error = %v, want ErrEpisodeNotFound", err)
	}
	if _, err := client.Streams(ctx, seriesID); !errors.Is(err, ErrNotMovie) {
		t.Fatalf("series Streams() error = %v, want ErrNotMovie", err)
	}

	movieID := client.ExternalTitleID("42185")
	if _, err := client.Series(ctx, movieID); !errors.Is(err, ErrNotSeries) {
		t.Fatalf("movie Series() error = %v, want ErrNotSeries", err)
	}
	if _, err := client.EpisodeStreams(ctx, movieID, 1, "1"); !errors.Is(err, ErrNotSeries) {
		t.Fatalf("movie EpisodeStreams() error = %v, want ErrNotSeries", err)
	}
}

func TestParseSeriesPayloadRejectsMalformedArray(t *testing.T) {
	t.Parallel()
	_, err := parseSeriesPayload([]byte(`makePlayer({playlist:{seasons:[{"season":1}`))
	if err == nil || errors.Is(err, ErrNotSeries) {
		t.Fatalf("parseSeriesPayload() error = %v, want malformed array error", err)
	}
}
