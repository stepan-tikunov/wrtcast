package grpcserver

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	titlev1 "title-api/api/title/v1"
	"title-api/internal/provider"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestRealTitleService(t *testing.T) {
	if os.Getenv("MOVIE_PROVIDER_INTEGRATION") == "" {
		t.Skip("set MOVIE_PROVIDER_INTEGRATION=1 to call the live service")
	}
	baseURL := os.Getenv("MOVIE_PROVIDER_BASE_URL")
	if baseURL == "" {
		t.Fatal("MOVIE_PROVIDER_BASE_URL is required for integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	providerClient, err := provider.NewClient(provider.Config{
		BaseURL:         baseURL,
		RefreshInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := providerClient.Start(ctx); err != nil {
		t.Fatal(err)
	}

	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	titlev1.RegisterTitleServiceServer(grpcServer, New(providerClient))
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	defer grpcServer.Stop()

	connection, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := titlev1.NewTitleServiceClient(connection)

	search, err := client.SearchTitles(ctx, &titlev1.SearchTitlesQuery{Query: "Дюна"})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.GetResults()) == 0 || search.GetResults()[0].GetTitleId() == "" {
		t.Fatal("search returned no title IDs")
	}

	movieID := providerClient.ExternalTitleID("42185")
	movie, err := client.GetPlaybackStreams(ctx, &titlev1.GetPlaybackStreamsQuery{
		TitleId: movieID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(movie.GetStreams()) == 0 || movie.GetStreams()[0].GetMasterPlaylistUrl() == "" {
		t.Fatal("movie has no playback streams")
	}
	movieDetails, err := client.GetTitleDetails(ctx, &titlev1.GetTitleDetailsQuery{TitleId: movieID})
	if err != nil {
		t.Fatal(err)
	}
	if movieDetails.GetDurationSeconds() == 0 {
		t.Fatal("movie details have zero duration")
	}

	seriesID := providerClient.ExternalTitleID("20973")
	details, err := client.GetTitleDetails(ctx, &titlev1.GetTitleDetailsQuery{TitleId: seriesID})
	if err != nil {
		t.Fatal(err)
	}
	if details.GetMediaType() != titlev1.MediaType_MEDIA_TYPE_SERIES ||
		len(details.GetSeasons()) == 0 || len(details.GetSeasons()[0].GetEpisodes()) == 0 {
		t.Fatalf("series details have no episode catalog: %+v", details)
	}

	season := int32(8)
	episode := "1"
	episodeStreams, err := client.GetPlaybackStreams(ctx, &titlev1.GetPlaybackStreamsQuery{
		TitleId:       seriesID,
		SeasonNumber:  &season,
		EpisodeNumber: &episode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(episodeStreams.GetStreams()) == 0 ||
		episodeStreams.GetStreams()[0].GetMasterPlaylistUrl() == "" {
		t.Fatal("selected episode has no playback streams")
	}

	for _, test := range []struct {
		siteID string
		title  string
	}{
		{siteID: "43222", title: "Аркейн"},
		{siteID: "54016", title: "Евангелион"},
	} {
		titleDetails, detailsErr := client.GetTitleDetails(
			ctx,
			&titlev1.GetTitleDetailsQuery{TitleId: providerClient.ExternalTitleID(test.siteID)},
		)
		if detailsErr != nil {
			t.Fatalf("GetTitleDetails(%s): %v", test.title, detailsErr)
		}
		if titleDetails.GetTitle() != test.title ||
			titleDetails.GetMediaType() != titlev1.MediaType_MEDIA_TYPE_SERIES ||
			titleDetails.GetDescription() == "" || len(titleDetails.GetRatings()) == 0 ||
			len(titleDetails.GetSeasons()) == 0 {
			t.Fatalf("unexpected %s details: %+v", test.title, titleDetails)
		}
	}

	arcaneSeason := int32(1)
	arcaneEpisode := "1"
	arcane, err := client.GetPlaybackStreams(ctx, &titlev1.GetPlaybackStreamsQuery{
		TitleId:       providerClient.ExternalTitleID("43222"),
		SeasonNumber:  &arcaneSeason,
		EpisodeNumber: &arcaneEpisode,
	})
	if err != nil {
		t.Fatal(err)
	}
	alternativeFound := false
	for _, stream := range arcane.GetStreams() {
		for _, audio := range stream.GetAudioTracks() {
			if strings.Contains(audio.GetName(), "2160p") {
				alternativeFound = true
			}
		}
	}
	if !alternativeFound {
		t.Fatal("Arcane has no 2160p stream from the alternative player")
	}
}
