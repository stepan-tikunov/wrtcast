package grpcserver

import (
	"context"
	"errors"
	"strings"
	"time"

	titlev1 "title-api/api/title/v1"
	"title-api/internal/marking"
	"title-api/internal/provider"
	"title-api/internal/tmdb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	titlev1.UnimplementedTitleServiceServer
	client  *provider.Client
	marking *marking.Service
	tmdb    *tmdb.Service
}

func New(client *provider.Client) *Server {
	return &Server{client: client}
}

// WithMarking has the server tell marking of the titles played first and
// serve its settings.
func (s *Server) WithMarking(service *marking.Service) *Server {
	s.marking = service
	return s
}

// WithTmdb has the server match the titles whose details are asked for
// with TMDB, tell their TMDB titles, look for TMDB's titles on the
// provider, and serve its settings.
func (s *Server) WithTmdb(service *tmdb.Service) *Server {
	s.tmdb = service
	if s.client != nil {
		service.UseProvider(tmdbProvider{s.client})
	}
	return s
}

// tmdbProvider is the provider, as TMDB's titles are looked for on it.
type tmdbProvider struct {
	client *provider.Client
}

func (p tmdbProvider) Search(ctx context.Context, query string) ([]tmdb.Found, error) {
	results, err := p.client.Search(ctx, query)
	found := make([]tmdb.Found, 0, len(results))
	for _, result := range results {
		found = append(found, tmdb.Found{ID: result.TitleID, Title: result.Title, Year: result.Year})
	}
	return found, err
}

func (p tmdbProvider) Title(ctx context.Context, id string) (tmdb.Title, error) {
	metadata, err := p.client.TitleMetadata(ctx, id)
	return tmdbTitle(metadata), err
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
		if query.GetFirstPlay() && s.marking != nil {
			s.marking.Opened(ctx, query.GetTitleId())
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
		Sections:        sections(metadata.Sections),
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
	if s.tmdb != nil {
		match, ok := s.tmdb.Match(ctx, metadata.TitleID)
		if !ok {
			match, ok = s.tmdb.Opened(ctx, tmdbTitle(metadata))
		}
		if ok {
			response.Tmdb = tmdbRef(match.Kind, match.ID)
		}
	}
	return response, nil
}

// tmdbMatchWait bounds a wait of WaitTmdbMatch, below the router's 20 s
// for a request of title-api.
const tmdbMatchWait = 15 * time.Second

func (s *Server) WaitTmdbMatch(
	ctx context.Context,
	query *titlev1.WaitTmdbMatchQuery,
) (*titlev1.TmdbMatch, error) {
	if query.GetTitleId() == "" {
		return nil, status.Error(codes.InvalidArgument, "title_id is required")
	}
	if s.tmdb == nil {
		return nil, status.Error(codes.Unimplemented, "TMDB is off")
	}
	ctx, cancel := context.WithTimeout(ctx, tmdbMatchWait)
	defer cancel()
	match, ok, pending := s.tmdb.WaitMatch(ctx, query.GetTitleId())
	response := &titlev1.TmdbMatch{Pending: pending}
	if ok {
		response.Tmdb = tmdbRef(match.Kind, match.ID)
	}
	return response, nil
}

// tmdbLookupWait bounds a wait of LookupTmdbTitle, as tmdbMatchWait.
const tmdbLookupWait = 15 * time.Second

var tmdbLookupStates = map[string]titlev1.TmdbLookup_State{
	tmdb.LookupPending:  titlev1.TmdbLookup_STATE_PENDING,
	tmdb.LookupFound:    titlev1.TmdbLookup_STATE_FOUND,
	tmdb.LookupNotFound: titlev1.TmdbLookup_STATE_NOT_FOUND,
}

func (s *Server) LookupTmdbTitle(
	ctx context.Context,
	command *titlev1.LookupTmdbTitleCommand,
) (*titlev1.TmdbLookup, error) {
	if s.tmdb == nil {
		return nil, status.Error(codes.Unimplemented, "TMDB is off")
	}
	kind, err := tmdbQuery(&titlev1.GetTmdbDetailsQuery{MediaType: command.GetMediaType(), TmdbId: command.GetTmdbId()})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, tmdbLookupWait)
	defer cancel()
	state, title, err := s.tmdb.Lookup(ctx, kind, command.GetTmdbId())
	if errors.Is(err, tmdb.ErrNoProvider) {
		return nil, status.Error(codes.Unimplemented, err.Error())
	}
	if err != nil {
		return nil, tmdbStatus(err)
	}
	return &titlev1.TmdbLookup{State: tmdbLookupStates[state], TitleId: title}, nil
}

// seriesUpdatesWait bounds the provider's part of GetSeriesUpdates, below
// the router's 20 s for a request of title-api.
const seriesUpdatesWait = 15 * time.Second

// seriesUpdatesMax is the most titles of a list of GetSeriesUpdates.
const seriesUpdatesMax = 30

func (s *Server) GetSeriesUpdates(
	ctx context.Context,
	query *titlev1.GetSeriesUpdatesQuery,
) (*titlev1.GetSeriesUpdatesQueryResponse, error) {
	if len(query.GetTitleIds()) > seriesUpdatesMax || len(query.GetUpcomingTitleIds()) > seriesUpdatesMax {
		return nil, status.Errorf(codes.InvalidArgument, "at most %d titles a list", seriesUpdatesMax)
	}
	// The provider is asked one title after another, as long as there is
	// time; those it takes too long for are fetched on and kept (see
	// provider.Client.Series).
	wait, cancel := context.WithTimeout(ctx, seriesUpdatesWait)
	defer cancel()
	response := &titlev1.GetSeriesUpdatesQueryResponse{}
	update := func(id string, last bool) {
		update := &titlev1.SeriesUpdate{TitleId: id}
		if s.tmdb != nil {
			match, series, ok := s.tmdb.Upcoming(ctx, id)
			if ok && match.Kind != tmdb.KindTV {
				return
			}
			if series != nil {
				update.Status = tmdbStatuses[series.Status]
				update.NextEpisode = tmdbEpisodeRef(series.Next)
			}
		}
		if last && s.client != nil && wait.Err() == nil {
			seasons, err := s.client.Series(wait, id)
			if errors.Is(err, provider.ErrNotSeries) {
				return
			}
			if err == nil {
				update.LastSeason, update.LastEpisode = lastEpisode(seasons)
				update.Seasons = seriesSeasons(seasons)
			}
		}
		response.Updates = append(response.Updates, update)
	}
	for _, id := range query.GetTitleIds() {
		update(id, true)
	}
	for _, id := range query.GetUpcomingTitleIds() {
		update(id, false)
	}
	return response, nil
}

// seriesSeasons are a series' episode numbers by season.
func seriesSeasons(seasons []provider.Season) []*titlev1.SeriesSeason {
	response := make([]*titlev1.SeriesSeason, 0, len(seasons))
	for _, season := range seasons {
		numbers := make([]string, 0, len(season.Episodes))
		for _, episode := range season.Episodes {
			numbers = append(numbers, episode.Number)
		}
		response = append(response, &titlev1.SeriesSeason{SeasonNumber: season.Number, EpisodeNumbers: numbers})
	}
	return response
}

// lastEpisode is a series' last episode on the provider: that of its last
// season with any.
func lastEpisode(seasons []provider.Season) (int32, string) {
	for i := len(seasons) - 1; i >= 0; i-- {
		if episodes := seasons[i].Episodes; len(episodes) > 0 {
			return seasons[i].Number, episodes[len(episodes)-1].Number
		}
	}
	return 0, ""
}

// tmdbTitle is a title as matching with TMDB knows it.
func tmdbTitle(metadata provider.TitleMetadata) tmdb.Title {
	title := tmdb.Title{
		ID: metadata.TitleID, Title: metadata.Title, OriginalTitle: metadata.OriginalTitle,
		Year: metadata.Year, Countries: metadata.Countries, Directors: metadata.Directors,
		Actors: metadata.Actors[:min(len(metadata.Actors), 5)], Description: metadata.Description,
	}
	switch metadata.MediaType {
	case provider.MediaTypeMovie:
		title.Kind = tmdb.KindMovie
	case provider.MediaTypeSeries:
		title.Kind = tmdb.KindTV
	}
	return title
}

func (s *Server) GetTmdbSettings(
	ctx context.Context,
	_ *titlev1.GetTmdbSettingsQuery,
) (*titlev1.TmdbSettings, error) {
	if s.tmdb == nil {
		return nil, status.Error(codes.Unimplemented, "TMDB is off")
	}
	settings, err := s.tmdb.Settings(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return tmdbSettings(settings), nil
}

func (s *Server) UpdateTmdbSettings(
	ctx context.Context,
	command *titlev1.UpdateTmdbSettingsCommand,
) (*titlev1.TmdbSettings, error) {
	if s.tmdb == nil {
		return nil, status.Error(codes.Unimplemented, "TMDB is off")
	}
	settings, err := s.tmdb.Update(ctx, command.GetApiKey())
	switch {
	case errors.Is(err, tmdb.ErrInvalidKey):
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return tmdbSettings(settings), nil
}

// GetTmdbDetails tells of a title on TMDB by its ID, whether title-api has
// it or not.
func (s *Server) GetTmdbDetails(
	ctx context.Context,
	query *titlev1.GetTmdbDetailsQuery,
) (*titlev1.TmdbDetails, error) {
	if s.tmdb == nil {
		return nil, status.Error(codes.Unimplemented, "TMDB is off")
	}
	kind, err := tmdbQuery(query)
	if err != nil {
		return nil, err
	}
	details, err := s.tmdb.Details(ctx, kind, query.GetTmdbId())
	if err != nil {
		return nil, tmdbStatus(err)
	}
	return tmdbDetails(details), nil
}

// GetTmdbCast tells who is in a title on TMDB, all of them.
func (s *Server) GetTmdbCast(
	ctx context.Context,
	query *titlev1.GetTmdbDetailsQuery,
) (*titlev1.TmdbCast, error) {
	if s.tmdb == nil {
		return nil, status.Error(codes.Unimplemented, "TMDB is off")
	}
	kind, err := tmdbQuery(query)
	if err != nil {
		return nil, err
	}
	cast, err := s.tmdb.Cast(ctx, kind, query.GetTmdbId())
	if err != nil {
		return nil, tmdbStatus(err)
	}
	return &titlev1.TmdbCast{Cast: tmdbPeople(cast)}, nil
}

// GetTmdbPerson tells of someone on TMDB by their ID, and of the titles
// they are in.
func (s *Server) GetTmdbPerson(
	ctx context.Context,
	query *titlev1.GetTmdbPersonQuery,
) (*titlev1.TmdbPersonDetails, error) {
	if s.tmdb == nil {
		return nil, status.Error(codes.Unimplemented, "TMDB is off")
	}
	if query.GetTmdbId() == 0 {
		return nil, status.Error(codes.InvalidArgument, "tmdb_id is required")
	}
	person, err := s.tmdb.Person(ctx, query.GetTmdbId())
	if err != nil {
		return nil, tmdbStatus(err)
	}
	return tmdbPerson(person), nil
}

var tmdbGenders = map[int]titlev1.TmdbGender{
	tmdb.GenderFemale:    titlev1.TmdbGender_TMDB_GENDER_FEMALE,
	tmdb.GenderMale:      titlev1.TmdbGender_TMDB_GENDER_MALE,
	tmdb.GenderNonBinary: titlev1.TmdbGender_TMDB_GENDER_NON_BINARY,
}

func tmdbPerson(person tmdb.PersonDetails) *titlev1.TmdbPersonDetails {
	response := &titlev1.TmdbPersonDetails{
		Id: person.ID, Name: person.Name, OriginalName: person.OriginalName, ProfilePath: person.Profile,
		Profession: person.Profession, Birthday: person.Birthday, Deathday: person.Deathday,
		PlaceOfBirth: person.PlaceOfBirth, Biography: person.Biography, Gender: tmdbGenders[person.Gender],
		Credits: make([]*titlev1.TmdbCredit, 0, len(person.Credits)),
	}
	for _, link := range person.Links() {
		response.Links = append(response.Links, &titlev1.TmdbLink{Site: link.Site, Url: link.URL})
	}
	for _, credit := range person.Credits {
		response.Credits = append(response.Credits, &titlev1.TmdbCredit{
			Title: tmdbRelated([]tmdb.Related{credit.Related})[0], Character: credit.Character,
			Jobs: credit.Jobs, Episodes: credit.Episodes, Acting: credit.Acting,
		})
	}
	return response
}

// tmdbQuery is the kind of the title a query is of, checked.
func tmdbQuery(query *titlev1.GetTmdbDetailsQuery) (string, error) {
	kind := query.GetMediaType()
	if kind != tmdb.KindMovie && kind != tmdb.KindTV {
		return "", status.Error(codes.InvalidArgument, `media_type must be "movie" or "tv"`)
	}
	if query.GetTmdbId() == 0 {
		return "", status.Error(codes.InvalidArgument, "tmdb_id is required")
	}
	return kind, nil
}

// GetTmdbSeason tells of a season of a series on TMDB.
func (s *Server) GetTmdbSeason(
	ctx context.Context,
	query *titlev1.GetTmdbSeasonQuery,
) (*titlev1.TmdbSeason, error) {
	if s.tmdb == nil {
		return nil, status.Error(codes.Unimplemented, "TMDB is off")
	}
	if query.GetTmdbId() == 0 || query.GetSeasonNumber() < 0 {
		return nil, status.Error(codes.InvalidArgument, "tmdb_id and season_number are required")
	}
	season, err := s.tmdb.Season(ctx, query.GetTmdbId(), query.GetSeasonNumber())
	if err != nil {
		return nil, tmdbStatus(err)
	}
	response := &titlev1.TmdbSeason{Episodes: make([]*titlev1.TmdbEpisode, 0, len(season.Episodes))}
	for _, episode := range season.Episodes {
		response.Episodes = append(response.Episodes, &titlev1.TmdbEpisode{
			EpisodeNumber: episode.Number, Name: episode.Name, Overview: episode.Overview,
			StillPath: episode.Still, AirDate: episode.AirDate, RuntimeMinutes: episode.Runtime,
			Upcoming: episode.Upcoming,
		})
	}
	return response, nil
}

func tmdbStatus(err error) error {
	switch {
	case errors.Is(err, tmdb.ErrNoKey):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, tmdb.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	default:
		return status.Error(codes.Unavailable, err.Error())
	}
}

func tmdbPeople(people []tmdb.Person) []*titlev1.TmdbPerson {
	result := make([]*titlev1.TmdbPerson, 0, len(people))
	for _, person := range people {
		result = append(result, &titlev1.TmdbPerson{
			Id: person.ID, Name: person.Name, Character: person.Character, ProfilePath: person.Profile,
		})
	}
	return result
}

func tmdbDetails(details tmdb.Details) *titlev1.TmdbDetails {
	response := &titlev1.TmdbDetails{
		Title: tmdbRef(details.Kind, details.ID), Name: details.Name, OriginalName: details.OriginalName,
		Year: details.Year(), ReleaseDate: details.Date, Tagline: details.Tagline, Overview: details.Overview,
		BackdropPath: details.Backdrop, LogoPath: details.Logo, LogoAspect: details.LogoAspect,
		PosterPath: details.Poster, Age: details.Age, VoteAverage: details.VoteAverage,
		RuntimeMinutes: details.Runtime, Genres: details.Genres, Countries: details.Countries,
		Directors: tmdbPeople(details.Directors), TitleId: details.TitleID,
		Recommendations: tmdbRelated(details.Recommendations),
	}
	response.Cast, response.CastTotal = tmdbPeople(details.Cast[:min(len(details.Cast), tmdb.CastShown)]), uint32(len(details.Cast))
	for _, video := range details.Videos {
		response.Videos = append(response.Videos, &titlev1.TmdbVideo{
			YoutubeKey: video.Key, Name: video.Name, Language: video.Language, Kind: video.Kind,
		})
	}
	if collection := details.Collection; collection != nil {
		response.Collection = &titlev1.TmdbCollection{
			Id: collection.ID, Name: collection.Name, Parts: tmdbRelated(collection.Parts),
		}
	}
	if series := details.Series; series != nil {
		response.Status = tmdbStatuses[series.Status]
		response.NextEpisode, response.LastEpisode = tmdbEpisodeRef(series.Next), tmdbEpisodeRef(series.Last)
		response.NumberOfSeasons = uint32(max(series.Seasons, 0))
		response.Creators = tmdbPeople(series.Creators)
		for _, season := range series.SeasonList {
			response.Seasons = append(response.Seasons, &titlev1.TmdbSeasonRef{
				SeasonNumber: season.Number, EpisodeCount: season.Episodes, AirDate: season.AirDate, Name: season.Name,
			})
		}
	}
	return response
}

var tmdbStatuses = map[string]titlev1.TmdbStatus{
	tmdb.StatusReturning: titlev1.TmdbStatus_TMDB_STATUS_RETURNING,
	tmdb.StatusEnded:     titlev1.TmdbStatus_TMDB_STATUS_ENDED,
	tmdb.StatusCanceled:  titlev1.TmdbStatus_TMDB_STATUS_CANCELED,
	tmdb.StatusOther:     titlev1.TmdbStatus_TMDB_STATUS_OTHER,
}

func tmdbRef(kind string, id uint64) *titlev1.TmdbTitle {
	ref := &titlev1.TmdbTitle{Id: id, MediaType: titlev1.MediaType_MEDIA_TYPE_MOVIE}
	if kind == tmdb.KindTV {
		ref.MediaType = titlev1.MediaType_MEDIA_TYPE_SERIES
	}
	return ref
}

func tmdbEpisodeRef(ref *tmdb.EpisodeRef) *titlev1.TmdbEpisodeRef {
	if ref == nil {
		return nil
	}
	return &titlev1.TmdbEpisodeRef{
		SeasonNumber: ref.Season, EpisodeNumber: ref.Episode, AirDate: ref.AirDate, Name: ref.Name,
	}
}

func tmdbRelated(list []tmdb.Related) []*titlev1.TmdbRelated {
	response := make([]*titlev1.TmdbRelated, 0, len(list))
	for _, r := range list {
		response = append(response, &titlev1.TmdbRelated{
			Title: tmdbRef(r.Kind, r.ID), Name: r.Name, Year: r.Year(), ReleaseDate: r.Date,
			PosterPath: r.Poster, BackdropPath: r.Backdrop, TitleId: r.TitleID,
		})
	}
	return response
}

func tmdbSettings(settings tmdb.Settings) *titlev1.TmdbSettings {
	return &titlev1.TmdbSettings{KeySet: settings.KeySet, KeyHint: settings.KeyHint, Error: settings.Error}
}

func (s *Server) GetMarkingSettings(
	ctx context.Context,
	_ *titlev1.GetMarkingSettingsQuery,
) (*titlev1.MarkingSettings, error) {
	if s.marking == nil {
		return nil, status.Error(codes.Unimplemented, "marking is off")
	}
	settings, err := s.marking.Settings(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return markingSettings(settings), nil
}

func (s *Server) UpdateMarkingSettings(
	ctx context.Context,
	command *titlev1.UpdateMarkingSettingsCommand,
) (*titlev1.MarkingSettings, error) {
	if s.marking == nil {
		return nil, status.Error(codes.Unimplemented, "marking is off")
	}
	settings, err := s.marking.Update(ctx, command.Enabled, command.ApiKey)
	switch {
	case errors.Is(err, marking.ErrInvalidKey):
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case err != nil:
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	return markingSettings(settings), nil
}

// MarkEpisode tells marking of a series saved to watch, and has the episode
// it would start with marked, while marking is on.
func (s *Server) MarkEpisode(
	ctx context.Context,
	command *titlev1.MarkEpisodeCommand,
) (*titlev1.MarkEpisodeCommandResponse, error) {
	if command.GetTitleId() == "" {
		return nil, status.Error(codes.InvalidArgument, "title_id is required")
	}
	if command.GetSeasonNumber() < 0 || strings.TrimSpace(command.GetEpisodeNumber()) == "" {
		return nil, status.Error(codes.InvalidArgument, "season_number and episode_number are required")
	}
	if s.marking == nil {
		return nil, status.Error(codes.Unimplemented, "marking is off")
	}
	s.marking.Opened(ctx, command.GetTitleId())
	settings, err := s.marking.Settings(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !settings.Enabled || !settings.KeySet {
		return &titlev1.MarkEpisodeCommandResponse{}, nil
	}
	episodes, err := s.client.EpisodeToMark(ctx, command.GetTitleId(), command.GetSeasonNumber(), command.GetEpisodeNumber())
	if err != nil {
		return nil, providerStatus(err)
	}
	s.marking.Unmarked(episodes)
	return &titlev1.MarkEpisodeCommandResponse{}, nil
}

func markingSettings(settings marking.Settings) *titlev1.MarkingSettings {
	return &titlev1.MarkingSettings{
		Enabled: settings.Enabled,
		KeySet:  settings.KeySet,
		KeyHint: settings.KeyHint,
		Error:   settings.Error,
	}
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
					Sections:        sections(episode.Sections),
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
			Name:                    stream.Name,
			MasterPlaylistUrl:       stream.MasterPlaylistURL,
			Sections:                sections(stream.Sections),
			SectionsDurationSeconds: stream.SectionsDuration,
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

func sections(list []provider.Section) []*titlev1.Section {
	response := make([]*titlev1.Section, 0, len(list))
	for _, section := range list {
		response = append(response, &titlev1.Section{
			Kind:         sectionKind(section.Kind),
			StartSeconds: section.Start,
			EndSeconds:   section.End,
			Skippable:    section.Skippable,
		})
	}
	return response
}

func sectionKind(value provider.SectionKind) titlev1.Section_Kind {
	switch value {
	case provider.SectionRecap:
		return titlev1.Section_KIND_RECAP
	case provider.SectionIntro:
		return titlev1.Section_KIND_INTRO
	case provider.SectionCredits:
		return titlev1.Section_KIND_CREDITS
	default:
		return titlev1.Section_KIND_UNSPECIFIED
	}
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
