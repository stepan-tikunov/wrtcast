package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestClientTitleMetadataDeterminesTypeFromPlayer(t *testing.T) {
	t.Parallel()

	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = response.Write([]byte("ready"))
		case "/index.php":
			siteID := request.URL.Query().Get("newsid")
			switch siteID {
			case "42185":
				_, _ = response.Write([]byte(titlePage(serverURL, "42185", "Дюна", "Dune", "2021", "США, Канада")))
			case "43222":
				_, _ = response.Write([]byte(titlePage(serverURL, "43222", "Аркейн", "Arcane", "2021", "США")))
			case "54016":
				_, _ = response.Write([]byte(titlePage(serverURL, "54016", "Евангелион", "Shin Seiki Evangerion", "1995", "Япония")))
			case "999":
				_, _ = response.Write([]byte(titlePage(serverURL, "999", "Неизвестный", "Unknown", "2026", "США")))
			default:
				response.WriteHeader(http.StatusNotFound)
			}
		case "/embed/movie/42185":
			_, _ = response.Write([]byte(`makePlayer({source:{hls:"https://cdn.example/movie/master.m3u8",duration:9360}})`))
		case "/embed/movie/43222":
			_, _ = response.Write([]byte(`makePlayer({playlist:{seasons:[{"season":1,"episodes":[]},{"season":2,"episodes":[]}]}})`))
		case "/embed/movie/54016":
			_, _ = response.Write([]byte(`makePlayer({playlist:{seasons:[{"season":1,"episodes":[{"episode":1}]}]}})`))
		case "/embed/movie/999":
			_, _ = response.Write([]byte(`makePlayer({source:{file:"not-hls"}})`))
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

	tests := []struct {
		siteID    string
		mediaType MediaType
		title     string
	}{
		{siteID: "42185", mediaType: MediaTypeMovie, title: "Дюна"},
		{siteID: "43222", mediaType: MediaTypeSeries, title: "Аркейн"},
		{siteID: "54016", mediaType: MediaTypeSeries, title: "Евангелион"},
	}
	for _, test := range tests {
		metadata, err := client.TitleMetadata(ctx, client.ExternalTitleID(test.siteID))
		if err != nil {
			t.Fatalf("TitleMetadata(%s): %v", test.siteID, err)
		}
		if metadata.MediaType != test.mediaType || metadata.Title != test.title {
			t.Fatalf("TitleMetadata(%s) = %+v", test.siteID, metadata)
		}
		if test.mediaType == MediaTypeMovie && metadata.DurationSeconds != 9360 {
			t.Fatalf("movie duration = %d, want 9360", metadata.DurationSeconds)
		}
		if metadata.Description != "Описание тайтла" || metadata.PosterURL != serverURL+"/poster.jpg" ||
			metadata.Quality != "HD 720, 1080" || len(metadata.Genres) != 2 ||
			len(metadata.Directors) != 2 || len(metadata.Actors) != 2 || len(metadata.Ratings) != 2 ||
			metadata.Ratings[0].Source != "kinopoisk" || metadata.Ratings[0].Value != 8.108 {
			t.Fatalf("incomplete metadata for %s: %+v", test.siteID, metadata)
		}
	}
	if _, err := client.TitleMetadata(ctx, client.ExternalTitleID("999")); !errors.Is(err, ErrMediaTypeUnknown) {
		t.Fatalf("TitleMetadata(unknown) error = %v, want ErrMediaTypeUnknown", err)
	}
}

func titlePage(baseURL, embedID, title, originalTitle, year, country string) string {
	return fmt.Sprintf(`
<div class="fdesc"><span id="descr" itemprop="description"><p>Описание тайтла</p></span></div>
<div class="fposter"><img src="/poster.jpg" alt="poster"></div>
<ul>
  <li><span>Название:</span> <span itemprop="name">%s</span></li>
  <li><span>Год выхода:</span> <span itemprop="dateCreated">%s</span></li>
  <li><span>Страна:</span> <span itemprop="countryOfOrigin">%s</span></li>
  <li><span>Оригинальное название:</span> <span itemprop="alternativeHeadline">%s</span></li>
  <li><span>Качество:</span> <strong>HD 720, 1080</strong></li>
  <li><span>Категории:</span> <span itemprop="genre"><a>Драма</a> / <a>Фантастика</a></span></li>
  <li><span>Режиссер:</span> <span itemprop="director">Первый, Второй</span></li>
  <li><span>Актеры:</span> <span itemprop="actors">Актёр Один, Актёр Два</span></li>
</ul>
<div class="frate frate-kp"><span>8.108</span></div>
<div class="frate frate-imdb"><span>8.5</span></div>
<iframe src="%s/embed/movie/%s"></iframe>`, title, year, country, originalTitle, baseURL, embedID)
}

func TestParseTitleMetadataRequiresTitle(t *testing.T) {
	t.Parallel()
	baseURL, _ := url.Parse("https://example.com")
	if _, err := parseTitleMetadata([]byte(`<div itemprop="description">missing title</div>`), baseURL); err == nil {
		t.Fatal("parseTitleMetadata() error = nil, want missing title error")
	}
}

func TestClientTitleMetadataDurationFallbacks(t *testing.T) {
	t.Parallel()

	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = response.Write([]byte("ready"))
		case "/index.php":
			siteID := request.URL.Query().Get("newsid")
			_, _ = response.Write([]byte(titlePage(serverURL, siteID, "Duration", "Duration", "2026", "США")))
		case "/embed/movie/100":
			_, _ = response.Write([]byte(`makePlayer({source:{hls:"https://cdn.example/quoted.m3u8",duration:"5400"}})`))
		case "/embed/movie/101":
			_, _ = fmt.Fprintf(response, `makePlayer({source:{hls:"%s/master.m3u8?token=base"}}); var lok=1,x="marker";`, serverURL)
		case "/embed/movie/102":
			_, _ = fmt.Fprintf(response, `makePlayer({source:{hls:"%s/direct.m3u8?token=base"}}); var lok=1,x="marker";`, serverURL)
		case "/embed/movie/103":
			_, _ = fmt.Fprintf(response, `makePlayer({source:{hls:"%s/missing.m3u8"}}); var lok=1,x="marker";`, serverURL)
		case "/master.m3u8":
			_, _ = response.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nvideo.m3u8\n"))
		case "/video.m3u8":
			if request.URL.Query().Get("token") != "base" {
				t.Errorf("variant lost master query: %s", request.URL.RawQuery)
			}
			_, _ = response.Write([]byte("#EXTM3U\n#EXTINF:5.25,\na.ts\n#EXTINF:4.75,\nb.ts\n"))
		case "/direct.m3u8":
			_, _ = response.Write([]byte("#EXTM3U\n#EXTINF:3.5,\na.ts\n#EXTINF:3.5,\nb.ts\n"))
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

	for _, test := range []struct {
		siteID   string
		duration uint64
	}{
		{siteID: "100", duration: 5400},
		{siteID: "101", duration: 10},
		{siteID: "102", duration: 7},
		{siteID: "103", duration: 0},
	} {
		metadata, metadataErr := client.TitleMetadata(ctx, client.ExternalTitleID(test.siteID))
		if metadataErr != nil {
			t.Fatalf("TitleMetadata(%s): %v", test.siteID, metadataErr)
		}
		if metadata.DurationSeconds != test.duration {
			t.Fatalf("TitleMetadata(%s) duration = %d, want %d", test.siteID, metadata.DurationSeconds, test.duration)
		}
	}
}
