package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	ErrNotMovie        = errors.New("requested media is not a movie")
	ErrNotSeries       = errors.New("requested media is not a series")
	ErrEpisodeNotFound = errors.New("episode not found")

	seasonsPattern = regexp.MustCompile(`(?s)["']?seasons["']?\s*:\s*\[`)
)

type MediaType int

const (
	MediaTypeUnspecified MediaType = iota
	MediaTypeMovie
	MediaTypeSeries
)

type Season struct {
	Number   int32
	Episodes []Episode
}

type Episode struct {
	Number          string
	Title           string
	DurationSeconds uint64
	// Marked on a video of DurationSeconds.
	Sections []Section
}

type seriesSeason struct {
	Season   scalarString    `json:"season"`
	Blocked  bool            `json:"blocked"`
	Episodes []seriesEpisode `json:"episodes"`
}

type seriesEpisode struct {
	Episode  scalarString         `json:"episode"`
	HLS      string               `json:"hls"`
	Audio    seriesEpisodeAudio   `json:"audio"`
	CC       []embedSubtitleEntry `json:"cc"`
	Duration uint64               `json:"duration"`
	Title    string               `json:"title"`
	Sections []playerSection      `json:"sections"`
}

type seriesEpisodeAudio struct {
	Names []string `json:"names"`
}

type scalarString string

func (s *scalarString) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*s = ""
		return nil
	}
	if data[0] == '"' {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		*s = scalarString(value)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*s = scalarString(number.String())
	return nil
}

func (c *Client) Series(ctx context.Context, externalMovieID string) ([]Season, error) {
	current, movieHTML, embedURLs, err := c.movieDocument(ctx, externalMovieID)
	if err != nil {
		return nil, err
	}

	merged := make(map[int32]map[string]Episode)
	seriesEmbeds := 0
	parseErrors := make([]error, 0)
	for _, embedURL := range embedURLs {
		embedHTML, _, fetchErr := c.get(ctx, current, embedURL, c.providerURL().String()+"/")
		if fetchErr != nil {
			parseErrors = append(parseErrors, fmt.Errorf("%s: fetch embed: %w", embedURL, fetchErr))
			continue
		}
		seasons, parseErr := parseSeriesPayload(embedHTML)
		if errors.Is(parseErr, ErrNotSeries) {
			continue
		}
		if parseErr != nil {
			parseErrors = append(parseErrors, fmt.Errorf("%s: %w", embedURL, parseErr))
			continue
		}
		seriesEmbeds++
		mergeSeasons(merged, seasons)
	}
	// Alternative players add episodes the primary ones lack, and keep the
	// catalog available while the primary players are down.
	for _, playerURL := range alternativePlayerURLs(movieHTML, c.providerURL().Scheme) {
		player, loadErr := c.loadAlternativePlayer(ctx, playerURL)
		if loadErr != nil {
			parseErrors = append(parseErrors, fmt.Errorf("%s: %w", playerURL, loadErr))
			continue
		}
		if !player.catalog.isSeries() {
			continue
		}
		seriesEmbeds++
		player.catalog.seasons(merged)
	}
	if seriesEmbeds == 0 {
		if len(parseErrors) > 0 {
			return nil, fmt.Errorf("parse series embeds: %w", errors.Join(parseErrors...))
		}
		return nil, ErrNotSeries
	}
	return sortedSeasons(merged), nil
}

func (c *Client) EpisodeStreams(
	ctx context.Context,
	externalMovieID string,
	seasonNumber int32,
	episodeNumber string,
) ([]MovieStream, error) {
	episodeNumber = strings.TrimSpace(episodeNumber)
	if seasonNumber < 0 || episodeNumber == "" {
		return nil, fmt.Errorf("%w: season and episode are required", ErrEpisodeNotFound)
	}

	current, movieHTML, embedURLs, err := c.movieDocument(ctx, externalMovieID)
	if err != nil {
		return nil, err
	}

	streams := make([]MovieStream, 0, len(embedURLs))
	streamErrors := make([]error, 0)
	seriesEmbeds := 0
	episodeFound := false
	// The first primary player that marks the episode's sections; all its
	// streams get them, also when that player's own stream fails.
	var episodeSections []Section
	var sectionsDuration float64
	// The video they are marked on, which the alternative player's is
	// aligned with, and the next episode's.
	var reference sectionsReference
	for index, embedURL := range embedURLs {
		embedHTML, _, fetchErr := c.get(ctx, current, embedURL, c.providerURL().String()+"/")
		if fetchErr != nil {
			streamErrors = append(streamErrors, fmt.Errorf("%s: fetch embed: %w", embedURL, fetchErr))
			continue
		}
		seasons, parseErr := parseSeriesPayload(embedHTML)
		if errors.Is(parseErr, ErrNotSeries) {
			continue
		}
		if parseErr != nil {
			streamErrors = append(streamErrors, fmt.Errorf("%s: %w", embedURL, parseErr))
			continue
		}
		seriesEmbeds++
		episode, ok := findEpisode(seasons, seasonNumber, episodeNumber)
		if !ok {
			continue
		}
		episodeFound = true
		marked := sections(episode.Sections)
		marks := episodeSections == nil && len(marked) > 0
		if marks {
			episodeSections, sectionsDuration = marked, float64(episode.Duration)
		}
		if strings.TrimSpace(episode.HLS) == "" {
			streamErrors = append(streamErrors, fmt.Errorf("%s: episode has no HLS URL", embedURL))
			continue
		}
		marker, markerErr := parseDynamicMarker(embedHTML)
		if markerErr != nil {
			streamErrors = append(streamErrors, fmt.Errorf("%s: %w", embedURL, markerErr))
			continue
		}
		stream, streamErr := c.streamFromHLS(
			ctx,
			current,
			html.UnescapeString(episode.HLS),
			marker,
			episode.Audio.Names,
			subtitleTracks(episode.CC),
		)
		if streamErr != nil {
			streamErrors = append(streamErrors, fmt.Errorf("%s: %w", embedURL, streamErr))
			continue
		}
		stream.Name = primaryStreamName(index, len(embedURLs))
		if len(marked) > 0 {
			stream.Sections, stream.SectionsDuration = marked, float64(episode.Duration)
		}
		if marks {
			reference = sectionsReference{
				key: referenceKey(externalMovieID, seasonNumber, episodeNumber), master: stream.MasterPlaylistURL,
				sections: marked, duration: float64(episode.Duration),
			}
			if season, next, ok := nextEpisode(seasons, seasonNumber, episodeNumber); ok && strings.TrimSpace(next.HLS) != "" {
				number := strings.TrimSpace(string(next.Episode))
				reference.next = &sectionsReference{
					key:      referenceKey(externalMovieID, season, number),
					master:   appendDynamicMarker(html.UnescapeString(next.HLS), marker),
					sections: sections(next.Sections), duration: float64(next.Duration),
				}
			}
		}
		streams = append(streams, stream)
	}
	for _, playerURL := range alternativePlayerURLs(movieHTML, c.providerURL().Scheme) {
		alternative, alternativeErr := c.alternativeStreams(ctx, playerURL, seasonNumber, episodeNumber)
		if errors.Is(alternativeErr, ErrEpisodeNotFound) {
			continue
		}
		if alternativeErr != nil {
			streamErrors = append(streamErrors, fmt.Errorf("%s: %w", playerURL, alternativeErr))
			continue
		}
		episodeFound = true
		seriesEmbeds++
		streams = append(streams, alternative...)
	}
	if len(streams) > 0 {
		shareSections(streams, episodeSections, sectionsDuration)
		c.alignStreams(streams, reference)
		return streams, nil
	}
	if seriesEmbeds == 0 {
		if len(streamErrors) == 0 {
			return nil, ErrNotSeries
		}
		return nil, fmt.Errorf("parse series embeds: %w", errors.Join(streamErrors...))
	}
	if !episodeFound {
		return nil, fmt.Errorf("%w: season %d episode %s", ErrEpisodeNotFound, seasonNumber, episodeNumber)
	}
	return nil, fmt.Errorf("no playable episode embed found: %w", errors.Join(streamErrors...))
}

func containsSeriesPlaylist(embedHTML []byte) bool {
	return seasonsPattern.FindIndex(embedHTML) != nil
}

func parseSeriesSeasons(embedHTML []byte) ([]Season, error) {
	payload, err := parseSeriesPayload(embedHTML)
	if err != nil {
		return nil, err
	}
	merged := make(map[int32]map[string]Episode)
	mergeSeasons(merged, payload)
	return sortedSeasons(merged), nil
}

func parseSeriesPayload(embedHTML []byte) ([]seriesSeason, error) {
	match := seasonsPattern.FindIndex(embedHTML)
	if match == nil {
		return nil, ErrNotSeries
	}
	start := match[1] - 1
	end, err := balancedJSONArrayEnd(embedHTML, start)
	if err != nil {
		return nil, fmt.Errorf("extract seasons: %w", err)
	}
	var seasons []seriesSeason
	if err := json.Unmarshal(embedHTML[start:end], &seasons); err != nil {
		return nil, fmt.Errorf("decode seasons: %w", err)
	}
	return seasons, nil
}

func balancedJSONArrayEnd(data []byte, start int) (int, error) {
	if start < 0 || start >= len(data) || data[start] != '[' {
		return 0, errors.New("seasons array start not found")
	}
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(data); index++ {
		switch character := data[index]; {
		case inString && escaped:
			escaped = false
		case inString && character == '\\':
			escaped = true
		case character == '"':
			inString = !inString
		case !inString && character == '[':
			depth++
		case !inString && character == ']':
			depth--
			if depth == 0 {
				return index + 1, nil
			}
		}
	}
	return 0, errors.New("unterminated seasons array")
}

func findEpisode(seasons []seriesSeason, seasonNumber int32, episodeNumber string) (seriesEpisode, bool) {
	for _, season := range seasons {
		number, err := strconv.ParseInt(string(season.Season), 10, 32)
		if err != nil || int32(number) != seasonNumber || season.Blocked {
			continue
		}
		for _, episode := range season.Episodes {
			if string(episode.Episode) == episodeNumber {
				return episode, true
			}
		}
	}
	return seriesEpisode{}, false
}

// nextEpisode is the episode after one, in its season or first in the
// next, with that season's number.
func nextEpisode(seasons []seriesSeason, seasonNumber int32, episodeNumber string) (int32, seriesEpisode, bool) {
	type numbered struct {
		season  int32
		episode seriesEpisode
	}
	var all []numbered
	for _, season := range seasons {
		number, err := strconv.ParseInt(string(season.Season), 10, 32)
		if err != nil || season.Blocked {
			continue
		}
		for _, episode := range season.Episodes {
			if strings.TrimSpace(string(episode.Episode)) != "" {
				all = append(all, numbered{season: int32(number), episode: episode})
			}
		}
	}
	sort.SliceStable(all, func(left, right int) bool {
		if all[left].season != all[right].season {
			return all[left].season < all[right].season
		}
		return episodeNumberLess(strings.TrimSpace(string(all[left].episode.Episode)), strings.TrimSpace(string(all[right].episode.Episode)))
	})
	for index, item := range all {
		if item.season == seasonNumber && strings.TrimSpace(string(item.episode.Episode)) == episodeNumber && index+1 < len(all) {
			return all[index+1].season, all[index+1].episode, true
		}
	}
	return 0, seriesEpisode{}, false
}

func mergeSeasons(target map[int32]map[string]Episode, source []seriesSeason) {
	for _, season := range source {
		seasonNumber, err := strconv.ParseInt(string(season.Season), 10, 32)
		if err != nil || season.Blocked {
			continue
		}
		key := int32(seasonNumber)
		if target[key] == nil {
			target[key] = make(map[string]Episode)
		}
		for _, episode := range season.Episodes {
			number := strings.TrimSpace(string(episode.Episode))
			if number == "" {
				continue
			}
			marked := sections(episode.Sections)
			if existing, exists := target[key][number]; exists {
				// Another player may mark what the first did not, on a
				// video of its own duration.
				if len(existing.Sections) == 0 && len(marked) > 0 {
					existing.Sections, existing.DurationSeconds = marked, episode.Duration
					target[key][number] = existing
				}
				continue
			}
			target[key][number] = Episode{
				Number:          number,
				Title:           html.UnescapeString(episode.Title),
				DurationSeconds: episode.Duration,
				Sections:        marked,
			}
		}
	}
}

func sortedSeasons(source map[int32]map[string]Episode) []Season {
	seasonNumbers := make([]int, 0, len(source))
	for number := range source {
		seasonNumbers = append(seasonNumbers, int(number))
	}
	sort.Ints(seasonNumbers)

	seasons := make([]Season, 0, len(seasonNumbers))
	for _, number := range seasonNumbers {
		episodes := make([]Episode, 0, len(source[int32(number)]))
		for _, episode := range source[int32(number)] {
			episodes = append(episodes, episode)
		}
		sort.SliceStable(episodes, func(left, right int) bool {
			return episodeNumberLess(episodes[left].Number, episodes[right].Number)
		})
		seasons = append(seasons, Season{Number: int32(number), Episodes: episodes})
	}
	return seasons
}

func episodeNumberLess(left, right string) bool {
	leftNumber, leftErr := strconv.ParseFloat(left, 64)
	rightNumber, rightErr := strconv.ParseFloat(right, 64)
	if leftErr == nil && rightErr == nil && leftNumber != rightNumber {
		return leftNumber < rightNumber
	}
	if leftErr == nil && rightErr != nil {
		return true
	}
	if leftErr != nil && rightErr == nil {
		return false
	}
	return left < right
}
