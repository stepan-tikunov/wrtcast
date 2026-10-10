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
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	response := &moviev1.GetStreamsByMovieIDQueryResponse{
		Streams: make([]*moviev1.GetStreamsByMovieIDQueryResponse_MovieStream, 0, len(streams)),
	}
	for _, stream := range streams {
		movieStream := &moviev1.GetStreamsByMovieIDQueryResponse_MovieStream{
			MasterPlaylistUrl: stream.MasterPlaylistURL,
			AudioTracks: make(
				[]*moviev1.GetStreamsByMovieIDQueryResponse_MovieStream_AudioTrack,
				0,
				len(stream.AudioTracks),
			),
			Subtitles: make(
				[]*moviev1.GetStreamsByMovieIDQueryResponse_MovieStream_SubtitleTrack,
				0,
				len(stream.Subtitles),
			),
		}
		for _, track := range stream.AudioTracks {
			movieStream.AudioTracks = append(
				movieStream.AudioTracks,
				&moviev1.GetStreamsByMovieIDQueryResponse_MovieStream_AudioTrack{
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
				&moviev1.GetStreamsByMovieIDQueryResponse_MovieStream_SubtitleTrack{
					Id:          subtitle.ID,
					Name:        subtitle.Name,
					Language:    subtitle.Language,
					Forced:      subtitle.Forced,
					PlaylistUrl: subtitle.PlaylistURL,
				},
			)
		}
		response.Streams = append(response.Streams, movieStream)
	}
	return response, nil
}
