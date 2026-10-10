package grpcserver

import (
	"context"
	"errors"

	moviev1 "movie-api/api/movie/v1"
	"movie-api/internal/provider"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	moviev1.UnimplementedMovieServiceServer
	client *provider.Client
}

func New(client *provider.Client) *Server {
	return &Server{client: client}
}

func (s *Server) SearchMovies(
	ctx context.Context,
	query *moviev1.SearchMoviesQuery,
) (*moviev1.SearchMoviesQueryResponse, error) {
	if query.GetQuery() == "" {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}

	results, err := s.client.Search(ctx, query.GetQuery())
	if err != nil {
		if errors.Is(err, provider.ErrInvalidSearchQuery) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	response := &moviev1.SearchMoviesQueryResponse{
		Results: make([]*moviev1.SearchMoviesQueryResponse_SearchResult, 0, len(results)),
	}
	for _, result := range results {
		response.Results = append(response.Results, &moviev1.SearchMoviesQueryResponse_SearchResult{
			MovieId:         result.MovieID,
			MovieTitle:      result.Title,
			MovieYear:       result.Year,
			MoviePosterLink: result.PosterURL,
			MediaType:       mediaType(result.MediaType),
		})
	}
	return response, nil
}

func (s *Server) GetStreamsByMovieID(
	ctx context.Context,
	query *moviev1.GetStreamsByMovieIDQuery,
) (*moviev1.GetStreamsByMovieIDQueryResponse, error) {
	if query.GetMovieId() == "" {
		return nil, status.Error(codes.InvalidArgument, "movie_id is required")
	}

	streams, err := s.client.Streams(ctx, query.GetMovieId())
	if err != nil {
		if errors.Is(err, provider.ErrInvalidMovieID) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if errors.Is(err, provider.ErrNotMovie) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	response := &moviev1.GetStreamsByMovieIDQueryResponse{
		Streams: mediaStreams(streams),
	}
	return response, nil
}

func (s *Server) GetSeries(
	ctx context.Context,
	query *moviev1.GetSeriesQuery,
) (*moviev1.GetSeriesQueryResponse, error) {
	if query.GetMovieId() == "" {
		return nil, status.Error(codes.InvalidArgument, "movie_id is required")
	}

	seasons, err := s.client.Series(ctx, query.GetMovieId())
	if err != nil {
		switch {
		case errors.Is(err, provider.ErrInvalidMovieID):
			return nil, status.Error(codes.InvalidArgument, err.Error())
		case errors.Is(err, provider.ErrNotSeries):
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		default:
			return nil, status.Error(codes.Unavailable, err.Error())
		}
	}

	response := &moviev1.GetSeriesQueryResponse{
		Seasons: make([]*moviev1.GetSeriesQueryResponse_Season, 0, len(seasons)),
	}
	for _, season := range seasons {
		seasonResponse := &moviev1.GetSeriesQueryResponse_Season{
			SeasonNumber: season.Number,
			Episodes: make(
				[]*moviev1.GetSeriesQueryResponse_Season_Episode,
				0,
				len(season.Episodes),
			),
		}
		for _, episode := range season.Episodes {
			seasonResponse.Episodes = append(
				seasonResponse.Episodes,
				&moviev1.GetSeriesQueryResponse_Season_Episode{
					EpisodeNumber:   episode.Number,
					Title:           episode.Title,
					DurationSeconds: episode.DurationSeconds,
				},
			)
		}
		response.Seasons = append(response.Seasons, seasonResponse)
	}
	return response, nil
}

func (s *Server) GetEpisodeStreams(
	ctx context.Context,
	query *moviev1.GetEpisodeStreamsQuery,
) (*moviev1.GetEpisodeStreamsQueryResponse, error) {
	if query.GetMovieId() == "" {
		return nil, status.Error(codes.InvalidArgument, "movie_id is required")
	}
	if query.GetSeasonNumber() < 0 {
		return nil, status.Error(codes.InvalidArgument, "season_number must not be negative")
	}
	if query.GetEpisodeNumber() == "" {
		return nil, status.Error(codes.InvalidArgument, "episode_number is required")
	}

	streams, err := s.client.EpisodeStreams(
		ctx,
		query.GetMovieId(),
		query.GetSeasonNumber(),
		query.GetEpisodeNumber(),
	)
	if err != nil {
		switch {
		case errors.Is(err, provider.ErrInvalidMovieID):
			return nil, status.Error(codes.InvalidArgument, err.Error())
		case errors.Is(err, provider.ErrNotSeries):
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		case errors.Is(err, provider.ErrEpisodeNotFound):
			return nil, status.Error(codes.NotFound, err.Error())
		default:
			return nil, status.Error(codes.Unavailable, err.Error())
		}
	}

	return &moviev1.GetEpisodeStreamsQueryResponse{Streams: mediaStreams(streams)}, nil
}

func mediaStreams(streams []provider.MovieStream) []*moviev1.MediaStream {
	response := make([]*moviev1.MediaStream, 0, len(streams))
	for _, stream := range streams {
		movieStream := &moviev1.MediaStream{
			MasterPlaylistUrl: stream.MasterPlaylistURL,
			AudioTracks: make(
				[]*moviev1.MediaStream_AudioTrack,
				0,
				len(stream.AudioTracks),
			),
			Subtitles: make(
				[]*moviev1.MediaStream_SubtitleTrack,
				0,
				len(stream.Subtitles),
			),
		}
		for _, track := range stream.AudioTracks {
			movieStream.AudioTracks = append(
				movieStream.AudioTracks,
				&moviev1.MediaStream_AudioTrack{
					Id:          track.ID,
					Name:        track.Name,
					Language:    track.Language,
					Default:     track.Default,
					PlaylistUrl: track.PlaylistURL,
				},
			)
		}
		for _, subtitle := range stream.Subtitles {
			movieStream.Subtitles = append(
				movieStream.Subtitles,
				&moviev1.MediaStream_SubtitleTrack{
					Id:          subtitle.ID,
					Name:        subtitle.Name,
					Language:    subtitle.Language,
					Forced:      subtitle.Forced,
					PlaylistUrl: subtitle.PlaylistURL,
				},
			)
		}
		response = append(response, movieStream)
	}
	return response
}

func mediaType(value provider.MediaType) moviev1.MediaType {
	switch value {
	case provider.MediaTypeMovie:
		return moviev1.MediaType_MEDIA_TYPE_MOVIE
	case provider.MediaTypeSeries:
		return moviev1.MediaType_MEDIA_TYPE_SERIES
	default:
		return moviev1.MediaType_MEDIA_TYPE_UNSPECIFIED
	}
}
