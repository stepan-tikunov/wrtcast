package grpcserver

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	moviev1 "movie-api/api/movie/v1"
	"movie-api/internal/provider"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestRealGetStreamsByMovieID(t *testing.T) {
	if os.Getenv("MOVIE_PROVIDER_INTEGRATION") == "" {
		t.Skip("set MOVIE_PROVIDER_INTEGRATION=1 to call the live service")
	}
	baseURL := os.Getenv("MOVIE_PROVIDER_BASE_URL")
	if baseURL == "" {
		t.Fatal("MOVIE_PROVIDER_BASE_URL is required for integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, err := provider.NewClient(provider.Config{
		BaseURL:         baseURL,
		RefreshInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	moviev1.RegisterMovieServiceServer(grpcServer, New(client))
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

	response, err := moviev1.NewMovieServiceClient(connection).GetStreamsByMovieID(
		ctx,
		&moviev1.GetStreamsByMovieIDQuery{MovieId: client.ExternalMovieID("42185")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.GetStreams()) == 0 {
		t.Fatal("got no movie streams")
	}
	if response.GetStreams()[0].GetMasterPlaylistUrl() == "" {
		t.Fatal("first movie stream has no master playlist URL")
	}
	var hasAudioMetadata bool
	for _, stream := range response.GetStreams() {
		for _, track := range stream.GetAudioTracks() {
			hasAudioMetadata = hasAudioMetadata || track.GetId() != "" &&
				track.GetName() != "" && track.GetLanguage() != ""
		}
	}
	if !hasAudioMetadata {
		t.Fatal("missing parsed audio metadata")
	}
}
