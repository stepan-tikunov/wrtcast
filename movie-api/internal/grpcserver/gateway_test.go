package grpcserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	moviev1 "movie-api/api/movie/v1"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
)

type gatewayTestService struct {
	moviev1.UnimplementedMovieServiceServer
}

func (gatewayTestService) SearchMovies(
	_ context.Context,
	query *moviev1.SearchMoviesQuery,
) (*moviev1.SearchMoviesQueryResponse, error) {
	return &moviev1.SearchMoviesQueryResponse{
		Results: []*moviev1.SearchMoviesQueryResponse_SearchResult{{
			MovieId:         "67c18f3642185",
			MovieTitle:      query.GetQuery(),
			MovieYear:       "2021",
			MoviePosterLink: "https://cdn.example/poster.jpg",
		}},
	}, nil
}

func (gatewayTestService) GetStreamsByMovieID(
	_ context.Context,
	query *moviev1.GetStreamsByMovieIDQuery,
) (*moviev1.GetStreamsByMovieIDQueryResponse, error) {
	return &moviev1.GetStreamsByMovieIDQueryResponse{
		Streams: []*moviev1.GetStreamsByMovieIDQueryResponse_MovieStream{{
			MasterPlaylistUrl: "https://cdn.example/" + query.GetMovieId() + "/master.m3u8",
			AudioTracks: []*moviev1.GetStreamsByMovieIDQueryResponse_MovieStream_AudioTrack{{
				Id:          "rus0",
				Name:        "Dub",
				Language:    "ru",
				Default:     true,
				PlaylistUrl: "https://cdn.example/audio.m3u8",
			}},
			Subtitles: []*moviev1.GetStreamsByMovieIDQueryResponse_MovieStream_SubtitleTrack{{
				Id:          "1",
				Name:        "Рус. полные",
				Language:    "ru",
				PlaylistUrl: "https://cdn.example/subtitles.vtt",
			}},
		}},
	}, nil
}

func TestHTTPAnnotation(t *testing.T) {
	t.Parallel()
	mux := runtime.NewServeMux()
	if err := moviev1.RegisterMovieServiceHandlerServer(
		context.Background(),
		mux,
		gatewayTestService{},
	); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/movies/67c18f3642185/streams", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	var body struct {
		Streams []struct {
			MasterPlaylistURL string `json:"masterPlaylistUrl"`
			AudioTracks       []struct {
				Language string `json:"language"`
			} `json:"audioTracks"`
			Subtitles []struct {
				Language string `json:"language"`
			} `json:"subtitles"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Streams) != 1 || body.Streams[0].MasterPlaylistURL == "" ||
		len(body.Streams[0].AudioTracks) != 1 || body.Streams[0].AudioTracks[0].Language != "ru" ||
		len(body.Streams[0].Subtitles) != 1 || body.Streams[0].Subtitles[0].Language != "ru" {
		t.Fatalf("unexpected response: %s", response.Body.String())
	}
}

func TestSearchMoviesHTTPAnnotation(t *testing.T) {
	t.Parallel()
	mux := runtime.NewServeMux()
	if err := moviev1.RegisterMovieServiceHandlerServer(
		context.Background(),
		mux,
		gatewayTestService{},
	); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/movies:search?query=%D0%94%D1%8E%D0%BD%D0%B0", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	var body struct {
		Results []struct {
			MovieID    string `json:"movieId"`
			MovieTitle string `json:"movieTitle"`
		} `json:"results"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Results) != 1 || body.Results[0].MovieID != "67c18f3642185" || body.Results[0].MovieTitle != "Дюна" {
		t.Fatalf("unexpected response: %s", response.Body.String())
	}
}
