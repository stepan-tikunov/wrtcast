package grpcserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	titlev1 "title-api/api/title/v1"
	"title-api/internal/provider"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTitleServicePlaybackAndDetails(t *testing.T) {
	t.Parallel()

	var serverURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = response.Write([]byte("ready"))
		case "/search-result/":
			_, _ = response.Write([]byte(`<div class="th-item"><a href="/filmy/42185-movie.html"><img src="/poster.jpg"><div class="th-title">Movie</div><div class="th-year">2021</div></a></div>`))
		case "/index.php":
			siteID := request.URL.Query().Get("newsid")
			if siteID == "25975" {
				response.WriteHeader(http.StatusForbidden)
				_, _ = response.Write([]byte(`<div class="berrors">Этот материал доступен только зарегистрированным пользователям! Зарегистрируйтесь на сайте.</div>`))
				return
			}
			embedID := "1"
			title := "Movie"
			if siteID == "20973" {
				embedID = "2"
				title = "Series"
			}
			_, _ = fmt.Fprintf(response, `<ul><li><span>Название:</span><span itemprop="name">%s</span></li></ul><iframe src="%s/embed/movie/%s"></iframe>`, title, serverURL, embedID)
		case "/embed/movie/1":
			_, _ = fmt.Fprintf(response, `makePlayer({source:{hls:"%s/movie/master.m3u8",duration:7200}}); var lok=1,x="marker";`, serverURL)
		case "/embed/movie/2":
			_, _ = fmt.Fprintf(response, `makePlayer({playlist:{seasons:[{"season":1,"episodes":[{"episode":"1","hls":"%s/series/master.m3u8","duration":2400,"title":"Pilot"}]}]}}); var lok=1,x="marker";`, serverURL)
		case "/movie/master.m3u8", "/series/master.m3u8":
			_, _ = response.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nvideo.m3u8\n"))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	serverURL = upstream.URL

	client, err := provider.NewClient(provider.Config{
		BaseURL:         upstream.URL,
		RefreshInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	service := New(client)
	search, err := service.SearchTitles(ctx, &titlev1.SearchTitlesQuery{Query: "Movie"})
	if err != nil || len(search.GetResults()) != 1 ||
		search.GetResults()[0].GetTitleId() != client.ExternalTitleID("42185") {
		t.Fatalf("SearchTitles() = %+v, %v", search, err)
	}

	season := int32(99)
	episode := "ignored"
	movie, err := service.GetPlaybackStreams(ctx, &titlev1.GetPlaybackStreamsQuery{
		TitleId:       client.ExternalTitleID("42185"),
		SeasonNumber:  &season,
		EpisodeNumber: &episode,
	})
	if err != nil || len(movie.GetStreams()) != 1 {
		t.Fatalf("movie playback with ignored selectors = %+v, %v", movie, err)
	}

	seriesID := client.ExternalTitleID("20973")
	_, err = service.GetPlaybackStreams(ctx, &titlev1.GetPlaybackStreamsQuery{TitleId: seriesID})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("series playback without selectors code = %s, want %s: %v", code, codes.InvalidArgument, err)
	}

	episode = "1"
	_, err = service.GetPlaybackStreams(ctx, &titlev1.GetPlaybackStreamsQuery{
		TitleId:       seriesID,
		EpisodeNumber: &episode,
	})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("series playback with partial selectors code = %s, want %s: %v", code, codes.InvalidArgument, err)
	}

	season = 1
	series, err := service.GetPlaybackStreams(ctx, &titlev1.GetPlaybackStreamsQuery{
		TitleId:       seriesID,
		SeasonNumber:  &season,
		EpisodeNumber: &episode,
	})
	if err != nil || len(series.GetStreams()) != 1 {
		t.Fatalf("series playback = %+v, %v", series, err)
	}

	episode = "404"
	_, err = service.GetPlaybackStreams(ctx, &titlev1.GetPlaybackStreamsQuery{
		TitleId:       seriesID,
		SeasonNumber:  &season,
		EpisodeNumber: &episode,
	})
	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("missing episode code = %s, want %s: %v", code, codes.NotFound, err)
	}

	details, err := service.GetTitleDetails(ctx, &titlev1.GetTitleDetailsQuery{TitleId: seriesID})
	if err != nil {
		t.Fatal(err)
	}
	if details.GetMediaType() != titlev1.MediaType_MEDIA_TYPE_SERIES ||
		len(details.GetSeasons()) != 1 || len(details.GetSeasons()[0].GetEpisodes()) != 1 {
		t.Fatalf("unexpected series details: %+v", details)
	}

	_, err = service.GetTitleDetails(ctx, &titlev1.GetTitleDetailsQuery{TitleId: "wrong-prefix-1"})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("invalid title ID code = %s, want %s: %v", code, codes.InvalidArgument, err)
	}

	registeredOnlyID := client.ExternalTitleID("25975")
	_, err = service.GetTitleDetails(ctx, &titlev1.GetTitleDetailsQuery{TitleId: registeredOnlyID})
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Fatalf("registered-only details code = %s, want %s: %v", code, codes.PermissionDenied, err)
	}
	_, err = service.GetPlaybackStreams(ctx, &titlev1.GetPlaybackStreamsQuery{TitleId: registeredOnlyID})
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Fatalf("registered-only playback code = %s, want %s: %v", code, codes.PermissionDenied, err)
	}
}

func TestMediaStreamsMapsProxyHeaders(t *testing.T) {
	t.Parallel()
	streams := mediaStreams([]provider.MovieStream{{
		Name:              "Русский дубляж",
		MasterPlaylistURL: "https://api.example/master.m3u8",
		ProxyHeaders:      []provider.ProxyHeader{{Name: "Origin", Value: "https://player.example"}},
	}})
	if len(streams) != 1 || streams[0].GetName() != "Русский дубляж" ||
		len(streams[0].GetProxyHeaders()) != 1 ||
		streams[0].GetProxyHeaders()[0].GetName() != "Origin" ||
		streams[0].GetProxyHeaders()[0].GetValue() != "https://player.example" {
		t.Fatalf("unexpected streams: %+v", streams)
	}
}

func TestGetSeriesUpdates(t *testing.T) {
	t.Parallel()

	var serverURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = response.Write([]byte("ready"))
		case "/index.php":
			_, _ = fmt.Fprintf(response, `<iframe src="%s/embed/movie/%s"></iframe>`, serverURL, request.URL.Query().Get("newsid"))
		case "/embed/movie/20973":
			_, _ = response.Write([]byte(`makePlayer({playlist:{seasons:[` +
				`{"season":2,"episodes":[{"episode":"1","hls":"x/master.m3u8"}]},` +
				`{"season":1,"episodes":[{"episode":"10","hls":"x/master.m3u8"},{"episode":"2","hls":"x/master.m3u8"}]}` +
				`]}}); var lok=1,x="marker";`))
		case "/embed/movie/42185":
			_, _ = response.Write([]byte(`makePlayer({source:{hls:"x/master.m3u8"}}); var lok=1,x="marker";`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	serverURL = upstream.URL

	client, err := provider.NewClient(provider.Config{BaseURL: upstream.URL, RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	service := New(client)
	series, movie := client.ExternalTitleID("20973"), client.ExternalTitleID("42185")
	response, err := service.GetSeriesUpdates(ctx, &titlev1.GetSeriesUpdatesQuery{
		TitleIds: []string{series, movie}, UpcomingTitleIds: []string{movie},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The movie asked for its episodes is left out; asked for what TMDB
	// has coming alone, it isn't known to be one.
	updates := response.GetUpdates()
	if len(updates) != 2 || updates[0].GetTitleId() != series || updates[1].GetTitleId() != movie ||
		updates[0].GetLastSeason() != 2 || updates[0].GetLastEpisode() != "1" || updates[1].GetLastEpisode() != "" {
		t.Fatalf("updates %v", updates)
	}

	ids := make([]string, seriesUpdatesMax+1)
	for i := range ids {
		ids[i] = series
	}
	if _, err := service.GetSeriesUpdates(ctx, &titlev1.GetSeriesUpdatesQuery{UpcomingTitleIds: ids}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("too many: %v", err)
	}
}

func TestLastEpisode(t *testing.T) {
	t.Parallel()
	seasons := []provider.Season{{Number: 1, Episodes: []provider.Episode{{Number: "1"}, {Number: "2"}}}, {Number: 2}}
	if season, episode := lastEpisode(seasons); season != 1 || episode != "2" {
		t.Errorf("lastEpisode = %d, %q", season, episode)
	}
	if season, episode := lastEpisode(nil); season != 0 || episode != "" {
		t.Errorf("none = %d, %q", season, episode)
	}
}
