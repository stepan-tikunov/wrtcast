package grpcserver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	moviev1 "movie-api/api/movie/v1"
	"movie-api/internal/provider"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMediaTypeMismatchStatuses(t *testing.T) {
	t.Parallel()

	var serverURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = response.Write([]byte("ready"))
		case "/index.php":
			embedID := "1"
			if request.URL.Query().Get("newsid") == "20973" {
				embedID = "2"
			}
			_, _ = fmt.Fprintf(response, `<iframe src="%s/embed/movie/%s"></iframe>`, serverURL, embedID)
		case "/embed/movie/1":
			_, _ = fmt.Fprintf(response, `makePlayer({source:{hls:"%s/movie/master.m3u8"}}); var lok=1,x="marker";`, serverURL)
		case "/embed/movie/2":
			_, _ = fmt.Fprintf(response, `makePlayer({playlist:{seasons:[{"season":1,"episodes":[{"episode":"1","hls":"%s/series/master.m3u8","title":"Pilot"}]}]}}); var lok=1,x="marker";`, serverURL)
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

	_, err = service.GetSeries(ctx, &moviev1.GetSeriesQuery{
		MovieId: client.ExternalMovieID("42185"),
	})
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Fatalf("GetSeries(movie) code = %s, want %s: %v", code, codes.FailedPrecondition, err)
	}

	_, err = service.GetStreamsByMovieID(ctx, &moviev1.GetStreamsByMovieIDQuery{
		MovieId: client.ExternalMovieID("20973"),
	})
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Fatalf("GetStreamsByMovieID(series) code = %s, want %s: %v", code, codes.FailedPrecondition, err)
	}

	_, err = service.GetEpisodeStreams(ctx, &moviev1.GetEpisodeStreamsQuery{
		MovieId:       client.ExternalMovieID("20973"),
		SeasonNumber:  1,
		EpisodeNumber: "404",
	})
	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("GetEpisodeStreams(missing) code = %s, want %s: %v", code, codes.NotFound, err)
	}

	_, err = service.GetTitleMetadata(ctx, &moviev1.GetTitleMetadataQuery{MovieId: "wrong-prefix-1"})
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Fatalf("GetTitleMetadata(invalid ID) code = %s, want %s: %v", code, codes.InvalidArgument, err)
	}
}
