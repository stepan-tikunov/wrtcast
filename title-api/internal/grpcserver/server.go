package grpcserver

import (
	"context"
	"errors"
	"strings"

	titlev1 "title-api/api/title/v1"
	"title-api/internal/provider"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	titlev1.UnimplementedTitleServiceServer
	client *provider.Client
}

func New(client *provider.Client) *Server {
	return &Server{client: client}
}

func (s *Server) SearchTitles(
	ctx context.Context,
	query *titlev1.SearchTitlesQuery,
) (*titlev1.SearchTitlesQueryResponse, error) {
	if strings.TrimSpace(query.GetQuery()) == "" {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}

	results, err := s.client.Search(ctx, query.GetQuery())
	if err != nil {
		if errors.Is(err, provider.ErrInvalidSearchQuery) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	response := &titlev1.SearchTitlesQueryResponse{
		Results: make([]*titlev1.SearchTitlesQueryResponse_SearchResult, 0, len(results)),
	}
	for _, result := range results {
		response.Results = append(response.Results, &titlev1.SearchTitlesQueryResponse_SearchResult{
			TitleId:    result.TitleID,
			Title:      result.Title,
			Year:       result.Year,
			PosterLink: result.PosterURL,
		})
	}
	return response, nil
}

func (s *Server) GetPlaybackStreams(
	ctx context.Context,
	query *titlev1.GetPlaybackStreamsQuery,
) (*titlev1.GetPlaybackStreamsQueryResponse, error) {
	if query.GetTitleId() == "" {
		return nil, status.Error(codes.InvalidArgument, "title_id is required")
	}

	metadata, err := s.client.TitleMetadata(ctx, query.GetTitleId())
	if err != nil {
		return nil, providerStatus(err)
	}

	var streams []provider.MovieStream
	switch metadata.MediaType {
	case provider.MediaTypeMovie:
		// Episode selectors intentionally have no effect for movies.
		streams, err = s.client.Streams(ctx, query.GetTitleId())
	case provider.MediaTypeSeries:
		if query.SeasonNumber == nil {
			return nil, status.Error(codes.InvalidArgument, "season_number is required for a series")
		}
		if query.GetSeasonNumber() < 0 {
			return nil, status.Error(codes.InvalidArgument, "season_number must not be negative")
		}
		if query.EpisodeNumber == nil || strings.TrimSpace(query.GetEpisodeNumber()) == "" {
			return nil, status.Error(codes.InvalidArgument, "episode_number is required for a series")
		}
		streams, err = s.client.EpisodeStreams(
			ctx,
			query.GetTitleId(),
			query.GetSeasonNumber(),
			query.GetEpisodeNumber(),
		)
	default:
		return nil, status.Error(codes.FailedPrecondition, "unsupported media type")
	}
	if err != nil {
		return nil, providerStatus(err)
	}

	return &titlev1.GetPlaybackStreamsQueryResponse{Streams: mediaStreams(streams)}, nil
}

func (s *Server) GetTitleDetails(
	ctx context.Context,
	query *titlev1.GetTitleDetailsQuery,
) (*titlev1.GetTitleDetailsQueryResponse, error) {
	if query.GetTitleId() == "" {
		return nil, status.Error(codes.InvalidArgument, "title_id is required")
	}

	metadata, err := s.client.TitleMetadata(ctx, query.GetTitleId())
	if err != nil {
		return nil, providerStatus(err)
	}

	response := &titlev1.GetTitleDetailsQueryResponse{
		TitleId:         metadata.TitleID,
		Title:           metadata.Title,
		OriginalTitle:   metadata.OriginalTitle,
		Year:            metadata.Year,
		Description:     metadata.Description,
		PosterLink:      metadata.PosterURL,
		MediaType:       mediaType(metadata.MediaType),
		Quality:         metadata.Quality,
		Countries:       metadata.Countries,
		Genres:          metadata.Genres,
		Directors:       metadata.Directors,
		Actors:          metadata.Actors,
		DurationSeconds: metadata.DurationSeconds,
		Ratings: make(
			[]*titlev1.GetTitleDetailsQueryResponse_Rating,
			0,
			len(metadata.Ratings),
		),
	}
	for _, rating := range metadata.Ratings {
		response.Ratings = append(response.Ratings, &titlev1.GetTitleDetailsQueryResponse_Rating{
			Source: rating.Source,
			Value:  rating.Value,
		})
	}

	if metadata.MediaType == provider.MediaTypeSeries {
		seasons, seriesErr := s.client.Series(ctx, query.GetTitleId())
		if seriesErr != nil {
			return nil, providerStatus(seriesErr)
		}
		response.Seasons = titleDetailsSeasons(seasons)
	}
	return response, nil
}

func providerStatus(err error) error {
	switch {
	case errors.Is(err, provider.ErrInvalidTitleID):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, provider.ErrEpisodeNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, provider.ErrNotMovie), errors.Is(err, provider.ErrNotSeries),
		errors.Is(err, provider.ErrMediaTypeUnknown):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, provider.ErrRegistrationRequired):
		return status.Error(codes.PermissionDenied, err.Error())
	default:
		return status.Error(codes.Unavailable, err.Error())
	}
}

func titleDetailsSeasons(seasons []provider.Season) []*titlev1.GetTitleDetailsQueryResponse_Season {
	response := make([]*titlev1.GetTitleDetailsQueryResponse_Season, 0, len(seasons))
	for _, season := range seasons {
		seasonResponse := &titlev1.GetTitleDetailsQueryResponse_Season{
			SeasonNumber: season.Number,
			Episodes: make(
				[]*titlev1.GetTitleDetailsQueryResponse_Season_Episode,
				0,
				len(season.Episodes),
			),
		}
		for _, episode := range season.Episodes {
			seasonResponse.Episodes = append(
				seasonResponse.Episodes,
				&titlev1.GetTitleDetailsQueryResponse_Season_Episode{
					EpisodeNumber:   episode.Number,
					Title:           episode.Title,
					DurationSeconds: episode.DurationSeconds,
				},
			)
		}
		response = append(response, seasonResponse)
	}
	return response
}

func mediaStreams(streams []provider.MovieStream) []*titlev1.MediaStream {
	response := make([]*titlev1.MediaStream, 0, len(streams))
	for _, stream := range streams {
		mediaStream := &titlev1.MediaStream{
			Name:              stream.Name,
			MasterPlaylistUrl: stream.MasterPlaylistURL,
			ProxyHeaders: make(
				[]*titlev1.MediaStream_ProxyHeader,
				0,
				len(stream.ProxyHeaders),
			),
			AudioTracks: make(
				[]*titlev1.MediaStream_AudioTrack,
				0,
				len(stream.AudioTracks),
			),
			Subtitles: make(
				[]*titlev1.MediaStream_SubtitleTrack,
				0,
				len(stream.Subtitles),
			),
		}
		for _, header := range stream.ProxyHeaders {
			mediaStream.ProxyHeaders = append(
				mediaStream.ProxyHeaders,
				&titlev1.MediaStream_ProxyHeader{Name: header.Name, Value: header.Value},
			)
		}
		for _, track := range stream.AudioTracks {
			mediaStream.AudioTracks = append(
				mediaStream.AudioTracks,
				&titlev1.MediaStream_AudioTrack{
					Id:          track.ID,
					Name:        track.Name,
					Language:    track.Language,
					Default:     track.Default,
					PlaylistUrl: track.PlaylistURL,
				},
			)
		}
		for _, subtitle := range stream.Subtitles {
			mediaStream.Subtitles = append(
				mediaStream.Subtitles,
				&titlev1.MediaStream_SubtitleTrack{
					Id:          subtitle.ID,
					Name:        subtitle.Name,
					Language:    subtitle.Language,
					Forced:      subtitle.Forced,
					PlaylistUrl: subtitle.PlaylistURL,
				},
			)
		}
		response = append(response, mediaStream)
	}
	return response
}

func mediaType(value provider.MediaType) titlev1.MediaType {
	switch value {
	case provider.MediaTypeMovie:
		return titlev1.MediaType_MEDIA_TYPE_MOVIE
	case provider.MediaTypeSeries:
		return titlev1.MediaType_MEDIA_TYPE_SERIES
	default:
		return titlev1.MediaType_MEDIA_TYPE_UNSPECIFIED
	}
}
