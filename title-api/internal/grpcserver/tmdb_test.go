package grpcserver

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	titlev1 "title-api/api/title/v1"
	"title-api/internal/tmdb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	_ "modernc.org/sqlite"
)

func TestGetTmdbDetails(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	service, err := tmdb.New(ctx, db, func(context.Context) (string, error) { return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	server := New(nil).WithTmdb(service)
	for _, test := range []struct {
		query *titlev1.GetTmdbDetailsQuery
		code  codes.Code
	}{
		{&titlev1.GetTmdbDetailsQuery{MediaType: "series", TmdbId: 1405}, codes.InvalidArgument},
		{&titlev1.GetTmdbDetailsQuery{MediaType: "movie"}, codes.InvalidArgument},
		// No TMDB key.
		{&titlev1.GetTmdbDetailsQuery{MediaType: "movie", TmdbId: 550}, codes.FailedPrecondition},
	} {
		if _, err := server.GetTmdbDetails(ctx, test.query); status.Code(err) != test.code {
			t.Errorf("%v: %v, want %s", test.query, err, test.code)
		}
	}
	if _, err := server.GetTmdbSeason(ctx, &titlev1.GetTmdbSeasonQuery{TmdbId: 1405, SeasonNumber: 1}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("season: %v", err)
	}
	if _, err := New(nil).GetTmdbDetails(ctx, &titlev1.GetTmdbDetailsQuery{MediaType: "movie", TmdbId: 550}); status.Code(err) != codes.Unimplemented {
		t.Errorf("TMDB off: %v", err)
	}
}

func TestTmdbStatus(t *testing.T) {
	t.Parallel()
	for err, code := range map[error]codes.Code{
		tmdb.ErrNotFound:                             codes.NotFound,
		fmt.Errorf("x: %w", tmdb.ErrNoKey):           codes.FailedPrecondition,
		fmt.Errorf("TMDB /movie/1: 502 Bad Gateway"): codes.Unavailable,
		context.DeadlineExceeded:                     codes.DeadlineExceeded,
	} {
		if got := status.Code(tmdbStatus(err)); got != code {
			t.Errorf("%v: %s, want %s", err, got, code)
		}
	}
}

func TestTmdbDetailsResponse(t *testing.T) {
	t.Parallel()
	response := tmdbDetails(tmdb.Details{
		ID: 1405, Kind: tmdb.KindTV, Name: "Декстер", Date: "2006-10-01", TitleID: "64aee42620222",
		Series: &tmdb.Series{
			Status: tmdb.StatusReturning, Seasons: 8, Next: &tmdb.EpisodeRef{Season: 9, Episode: 1, AirDate: "2027-01-16"},
			SeasonList: []tmdb.SeasonRef{{Number: 1, Episodes: 12}},
		},
		Collection:      &tmdb.Collection{ID: 1, Name: "c", Parts: []tmdb.Related{{ID: 2, Kind: tmdb.KindMovie, Date: "2027-02-01", TitleID: "x"}}},
		Recommendations: []tmdb.Related{{ID: 3, Kind: tmdb.KindTV, Name: "r"}},
	})
	if response.GetYear() != "2006" || response.GetReleaseDate() != "2006-10-01" || response.GetTitleId() != "64aee42620222" ||
		response.GetTitle().GetMediaType() != titlev1.MediaType_MEDIA_TYPE_SERIES ||
		response.GetStatus() != titlev1.TmdbStatus_TMDB_STATUS_RETURNING || response.GetNumberOfSeasons() != 8 ||
		response.GetNextEpisode().GetAirDate() != "2027-01-16" || response.GetLastEpisode() != nil ||
		len(response.GetSeasons()) != 1 {
		t.Errorf("response %v", response)
	}
	if part := response.GetCollection().GetParts()[0]; part.GetYear() != "2027" || part.GetTitleId() != "x" {
		t.Errorf("part %v", part)
	}
	if r := response.GetRecommendations()[0]; r.GetTitle().GetMediaType() != titlev1.MediaType_MEDIA_TYPE_SERIES {
		t.Errorf("recommendation %v", r)
	}
}
