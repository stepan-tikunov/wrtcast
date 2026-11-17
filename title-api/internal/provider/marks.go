package provider

import (
	"context"
	"fmt"
	"html"
	"strings"
)

// Marks are the sections title-api marks itself on the episodes the
// primary player marks none of (see internal/marking).
type Marks interface {
	// Sections are those of an episode, and the duration of the video they
	// are marked on; none while it is not marked.
	Sections(title string, season int32, episode string) ([]Section, float64)
	// Unmarked tells of an episode played, or the ones after it, that the
	// primary player marks nothing of, the one played first.
	Unmarked(episodes []UnmarkedEpisode)
}

// UnmarkedEpisode is an episode without the primary player's sections:
// its primary video and its subtitles.
type UnmarkedEpisode struct {
	Title     string
	Season    int32
	Episode   string
	Master    string
	Subtitles []SubtitleTrack
}

// VideoSegment is a segment of the lightest variant of a video: its URL,
// its place and its init segment's URL (none for MPEG-TS).
type VideoSegment struct {
	URL             string
	Start, Duration float64
	Init            string
}

// unmarkedAhead is how many episodes after the one played are told of as
// unmarked.
const unmarkedAhead = 2

// marked are the sections of an episode marked by title-api, if any.
func (c *Client) marked(title string, season int32, episode string) ([]Section, float64) {
	if c.marks == nil {
		return nil, 0
	}
	return c.marks.Sections(title, season, episode)
}

// unmarked are the episode of a series payload and the ones after it,
// ahead at most, that the primary player marks none of.
func unmarked(title string, seasons []seriesSeason, season int32, episode seriesEpisode, marker string, ahead int) []UnmarkedEpisode {
	var result []UnmarkedEpisode
	number := strings.TrimSpace(string(episode.Episode))
	for step := 0; step <= ahead; step++ {
		if step > 0 {
			next, after, ok := nextEpisode(seasons, season, number)
			if !ok {
				break
			}
			season, episode, number = next, after, strings.TrimSpace(string(after.Episode))
		}
		if len(sections(episode.Sections)) > 0 || strings.TrimSpace(episode.HLS) == "" {
			continue
		}
		result = append(result, UnmarkedEpisode{
			Title: title, Season: season, Episode: number,
			Master:    appendDynamicMarker(html.UnescapeString(episode.HLS), marker),
			Subtitles: subtitleTracks(episode.CC),
		})
	}
	return result
}

// EpisodeToMark is the episode of a series, alone, if no primary player
// marks it: as the first primary player that has it plays it.
func (c *Client) EpisodeToMark(ctx context.Context, externalMovieID string, seasonNumber int32, episodeNumber string) ([]UnmarkedEpisode, error) {
	episodeNumber = strings.TrimSpace(episodeNumber)
	if seasonNumber < 0 || episodeNumber == "" {
		return nil, fmt.Errorf("%w: season and episode are required", ErrEpisodeNotFound)
	}
	current, _, embedURLs, err := c.movieDocument(ctx, externalMovieID)
	if err != nil {
		return nil, err
	}
	var result []UnmarkedEpisode
	series, found := false, false
	for _, embedURL := range embedURLs {
		embedHTML, _, err := c.get(ctx, current, embedURL, c.providerURL().String()+"/")
		if err != nil {
			continue
		}
		seasons, err := parseSeriesPayload(embedHTML)
		if err != nil {
			continue
		}
		series = true
		episode, ok := findEpisode(seasons, seasonNumber, episodeNumber)
		if !ok {
			continue
		}
		found = true
		if len(sections(episode.Sections)) > 0 {
			return nil, nil
		}
		marker, err := parseDynamicMarker(embedHTML)
		if result == nil && err == nil {
			result = unmarked(externalMovieID, seasons, seasonNumber, episode, marker, 0)
		}
	}
	if !series {
		return nil, ErrNotSeries
	}
	if !found {
		return nil, fmt.Errorf("%w: season %d episode %s", ErrEpisodeNotFound, seasonNumber, episodeNumber)
	}
	return result, nil
}

// Fetch loads a URL of the provider's players or their CDN, with the
// session's User-Agent the signed URLs are bound to.
func (c *Client) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	current, err := c.currentSession()
	if err != nil {
		return nil, err
	}
	body, _, err := c.get(ctx, current, rawURL, "")
	return body, err
}

// VideoSegments lists the segments of the lightest variant of a master.
func (c *Client) VideoSegments(ctx context.Context, master string) ([]VideoSegment, error) {
	current, err := c.currentSession()
	if err != nil {
		return nil, err
	}
	body, _, err := c.get(ctx, current, master, "")
	if err != nil {
		return nil, fmt.Errorf("master: %w", err)
	}
	variant, err := lightestVariant(master, string(body))
	if err != nil {
		return nil, err
	}
	body, _, err = c.get(ctx, current, variant, "")
	if err != nil {
		return nil, fmt.Errorf("media playlist: %w", err)
	}
	segments, err := parseMediaPlaylist(variant, string(body))
	if err != nil {
		return nil, err
	}
	result := make([]VideoSegment, 0, len(segments))
	for _, segment := range segments {
		result = append(result, VideoSegment{URL: segment.url, Start: segment.start, Duration: segment.duration, Init: segment.init})
	}
	return result, nil
}
