package grpcserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	titlev1 "title-api/api/title/v1"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
)

type gatewayTestService struct {
	titlev1.UnimplementedTitleServiceServer
}

func (gatewayTestService) SearchTitles(
	_ context.Context,
	query *titlev1.SearchTitlesQuery,
) (*titlev1.SearchTitlesQueryResponse, error) {
	return &titlev1.SearchTitlesQueryResponse{
		Results: []*titlev1.SearchTitlesQueryResponse_SearchResult{{
			TitleId:         "67c18f3642185",
			Title:           query.GetQuery(),
			Year:            "2021",
			PosterLink:      "https://cdn.example/poster.jpg",
			DurationSeconds: 9360,
		}},
	}, nil
}

func (gatewayTestService) GetPlaybackStreams(
	_ context.Context,
	query *titlev1.GetPlaybackStreamsQuery,
) (*titlev1.GetPlaybackStreamsQueryResponse, error) {
	return &titlev1.GetPlaybackStreamsQueryResponse{
		Streams: []*titlev1.MediaStream{{
			MasterPlaylistUrl: "https://cdn.example/" + query.GetTitleId() + "/master.m3u8",
			AudioTracks: []*titlev1.MediaStream_AudioTrack{{
				Id:          "rus0",
				Name:        "Dub",
				Language:    "ru",
				Default:     true,
				PlaylistUrl: "https://cdn.example/audio.m3u8",
			}},
			Subtitles: []*titlev1.MediaStream_SubtitleTrack{{
				Id:          "1",
				Name:        "Рус. полные",
				Language:    "ru",
				PlaylistUrl: "https://cdn.example/subtitles.vtt",
			}},
		}},
	}, nil
}

func (gatewayTestService) GetTitleDetails(
	_ context.Context,
	query *titlev1.GetTitleDetailsQuery,
) (*titlev1.GetTitleDetailsQueryResponse, error) {
	return &titlev1.GetTitleDetailsQueryResponse{
		TitleId:       query.GetTitleId(),
		Title:         "Аркейн",
		OriginalTitle: "Arcane: League of Legends",
		Year:          "2021",
		Description:   "Описание",
		PosterLink:    "https://cdn.example/arcane.jpg",
		MediaType:     titlev1.MediaType_MEDIA_TYPE_SERIES,
		Quality:       "HD 720, 1080",
		Countries:     []string{"США"},
		Genres:        []string{"Мультфильмы", "Netflix"},
		Directors:     []string{"Pascal Charrue"},
		Actors:        []string{"Хейли Стайнфелд"},
		Ratings: []*titlev1.GetTitleDetailsQueryResponse_Rating{
			{Source: "kinopoisk", Value: 9.2},
			{Source: "imdb", Value: 9.0},
		},
		Seasons: []*titlev1.GetTitleDetailsQueryResponse_Season{{
			SeasonNumber: 1,
			Episodes: []*titlev1.GetTitleDetailsQueryResponse_Season_Episode{{
				EpisodeNumber:   "1",
				Title:           "Аркейн (1 сезон) - 1 серия",
				DurationSeconds: 2400,
			}},
		}},
	}, nil
}

func testGateway(t *testing.T) *runtime.ServeMux {
	t.Helper()
	mux := runtime.NewServeMux()
	if err := titlev1.RegisterTitleServiceHandlerServer(
		context.Background(),
		mux,
		gatewayTestService{},
	); err != nil {
		t.Fatal(err)
	}
	return mux
}

func TestSearchTitlesHTTPAnnotation(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/v1/search?query=%D0%94%D1%8E%D0%BD%D0%B0", nil)
	response := httptest.NewRecorder()
	testGateway(t).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	var body struct {
		Results []struct {
			TitleID         string `json:"titleId"`
			Title           string `json:"title"`
			DurationSeconds string `json:"durationSeconds"`
		} `json:"results"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Results) != 1 || body.Results[0].TitleID != "67c18f3642185" ||
		body.Results[0].Title != "Дюна" || body.Results[0].DurationSeconds != "9360" {
		t.Fatalf("unexpected response: %s", response.Body.String())
	}
}

func TestPlaybackStreamsHTTPAnnotation(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/titles/67c18f3620973/playback-streams?season_number=8&episode_number=1",
		nil,
	)
	response := httptest.NewRecorder()
	testGateway(t).ServeHTTP(response, request)
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

func TestTitleDetailsHTTPAnnotation(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "/v1/titles/67c18f3643222", nil)
	response := httptest.NewRecorder()
	testGateway(t).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	var body struct {
		TitleID   string `json:"titleId"`
		Title     string `json:"title"`
		MediaType string `json:"mediaType"`
		Ratings   []struct {
			Source string  `json:"source"`
			Value  float64 `json:"value"`
		} `json:"ratings"`
		Seasons []struct {
			SeasonNumber int32 `json:"seasonNumber"`
			Episodes     []struct {
				EpisodeNumber string `json:"episodeNumber"`
			} `json:"episodes"`
		} `json:"seasons"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.TitleID != "67c18f3643222" || body.Title != "Аркейн" ||
		body.MediaType != "MEDIA_TYPE_SERIES" || len(body.Ratings) != 2 ||
		len(body.Seasons) != 1 || body.Seasons[0].Episodes[0].EpisodeNumber != "1" {
		t.Fatalf("unexpected details response: %s", response.Body.String())
	}
}
