package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	embedURLPattern          = regexp.MustCompile(`(?i)(?:https?:)?//[a-z0-9.-]+(?::[0-9]+)?/embed/movie/[0-9]+`)
	alternativePlayerPattern = regexp.MustCompile(`(?i)(?:https?:)?(?:\\?/){2}[a-z0-9.-]+(?::[0-9]+)?(?:\\?/)\?(?:token_movie=[a-f0-9]+(?:&amp;|&)token=[a-f0-9]+|token=[a-f0-9]+(?:&amp;|&)token_movie=[a-f0-9]+)`)
	hlsPattern               = regexp.MustCompile(`(?s)["']?hls["']?\s*:\s*"((?:\\.|[^"])*)"`)
	markerPattern            = regexp.MustCompile(`(?s)var\s+lok\s*=\s*1\s*,\s*[a-zA-Z0-9_$]+\s*=\s*"((?:\\.|[^"])*)"`)
	audioNames               = regexp.MustCompile(`(?s)["']?names["']?\s*:\s*(\[[^]]*\])`)
	subtitlesPattern         = regexp.MustCompile(`(?s)["']?cc["']?\s*:\s*(\[[^]]*\])`)
)

type MovieStream struct {
	MasterPlaylistURL string
	AudioTracks       []AudioTrack
	Subtitles         []SubtitleTrack
	ProxyHeaders      []ProxyHeader
}

type ProxyHeader struct {
	Name  string
	Value string
}

type AudioTrack struct {
	ID          string
	Name        string
	Language    string
	Default     bool
	PlaylistURL string
}

type SubtitleTrack struct {
	ID          string
	Name        string
	Language    string
	Forced      bool
	PlaylistURL string
}

type embedSubtitleEntry struct {
	URL  string `json:"url"`
	Name string `json:"name"`
}

func (c *Client) Streams(ctx context.Context, externalMovieID string) ([]MovieStream, error) {
	current, movieHTML, embedURLs, err := c.movieDocument(ctx, externalMovieID)
	if err != nil {
		return nil, err
	}

	streams := make([]MovieStream, 0, len(embedURLs))
	streamErrors := make([]error, 0)
	seriesEmbeds := 0
	for _, embedURL := range embedURLs {
		stream, streamErr := c.streamFromEmbed(ctx, current, embedURL)
		if errors.Is(streamErr, ErrNotMovie) {
			seriesEmbeds++
			continue
		}
		if streamErr != nil {
			streamErrors = append(streamErrors, fmt.Errorf("%s: %w", embedURL, streamErr))
			continue
		}
		streams = append(streams, stream)
	}
	for _, playerURL := range alternativePlayerURLs(movieHTML, c.baseURL.Scheme) {
		alternative, alternativeErr := c.alternativeStreams(ctx, playerURL, 0, "")
		if alternativeErr != nil {
			streamErrors = append(streamErrors, fmt.Errorf("%s: %w", playerURL, alternativeErr))
			continue
		}
		streams = append(streams, alternative...)
	}
	if len(streams) == 0 {
		if len(embedURLs) > 0 && seriesEmbeds == len(embedURLs) {
			return nil, ErrNotMovie
		}
		return nil, fmt.Errorf("no playable embed found: %w", errors.Join(streamErrors...))
	}
	return streams, nil
}

func (c *Client) movieEmbeds(ctx context.Context, externalMovieID string) (*session, []string, error) {
	current, _, embedURLs, err := c.movieDocument(ctx, externalMovieID)
	if err != nil {
		return nil, nil, err
	}
	return current, embedURLs, nil
}

func (c *Client) movieDocument(ctx context.Context, externalMovieID string) (*session, []byte, []string, error) {
	siteMovieID, err := c.parseExternalTitleID(externalMovieID)
	if err != nil {
		return nil, nil, nil, err
	}

	current, err := c.currentSession()
	if err != nil {
		return nil, nil, nil, err
	}
	movieHTML, err := c.moviePage(ctx, current, siteMovieID)
	if errors.Is(err, errAntiBotChallenge) {
		if refreshErr := c.refreshIfStale(ctx, current.generation); refreshErr != nil {
			return nil, nil, nil, fmt.Errorf("refresh rejected anti-bot session: %w", refreshErr)
		}
		current, err = c.currentSession()
		if err != nil {
			return nil, nil, nil, err
		}
		movieHTML, err = c.moviePage(ctx, current, siteMovieID)
	}
	if err != nil {
		return nil, nil, nil, err
	}

	embedURLs := uniqueEmbedURLs(embedURLPattern.FindAllString(string(movieHTML), -1), c.baseURL.Scheme)
	if len(embedURLs) == 0 && len(alternativePlayerURLs(movieHTML, c.baseURL.Scheme)) == 0 {
		return nil, nil, nil, errors.New("embed URL not found in movie page")
	}
	return current, movieHTML, embedURLs, nil
}

func (c *Client) streamFromEmbed(ctx context.Context, current *session, embedURL string) (MovieStream, error) {
	embedHTML, _, err := c.get(ctx, current, embedURL, c.baseURL.String()+"/")
	if err != nil {
		return MovieStream{}, fmt.Errorf("fetch embed: %w", err)
	}
	if containsSeriesPlaylist(embedHTML) {
		return MovieStream{}, ErrNotMovie
	}
	hlsMatch := hlsPattern.FindSubmatch(embedHTML)
	if len(hlsMatch) != 2 {
		return MovieStream{}, errors.New("HLS URL or dynamic marker not found in embed page")
	}
	hlsURL, err := decodeJSString(string(hlsMatch[1]))
	if err != nil {
		return MovieStream{}, fmt.Errorf("decode HLS URL: %w", err)
	}
	marker, err := parseDynamicMarker(embedHTML)
	if err != nil {
		return MovieStream{}, err
	}
	return c.streamFromHLS(
		ctx,
		current,
		html.UnescapeString(hlsURL),
		marker,
		parseAudioNames(embedHTML),
		parseSubtitles(embedHTML),
	)
}

func (c *Client) streamFromHLS(
	ctx context.Context,
	current *session,
	hlsURL string,
	marker string,
	audioNames []string,
	subtitles []SubtitleTrack,
) (MovieStream, error) {
	masterURL := appendDynamicMarker(hlsURL, marker)

	masterBody, _, err := c.get(ctx, current, masterURL, "")
	if err != nil {
		return MovieStream{}, fmt.Errorf("fetch HLS master: %w", err)
	}
	if !strings.Contains(string(masterBody), "#EXTM3U") {
		return MovieStream{}, errors.New("HLS master response is not an m3u8 playlist")
	}
	return parseMaster(masterURL, string(masterBody), audioNames, subtitles)
}

func appendDynamicMarker(hlsURL, marker string) string {
	separator := "?"
	if strings.Contains(hlsURL, "?") {
		separator = "&"
	}
	return hlsURL + separator + marker
}

func parseDynamicMarker(embedHTML []byte) (string, error) {
	markerMatch := markerPattern.FindSubmatch(embedHTML)
	if len(markerMatch) != 2 {
		return "", errors.New("dynamic marker not found in embed page")
	}
	marker, err := decodeJSString(string(markerMatch[1]))
	if err != nil {
		return "", fmt.Errorf("decode HLS marker: %w", err)
	}
	return strings.TrimLeft(html.UnescapeString(marker), "?&"), nil
}

func uniqueEmbedURLs(matches []string, scheme string) []string {
	result := make([]string, 0, len(matches))
	seen := make(map[string]bool)
	for _, match := range matches {
		if strings.HasPrefix(match, "//") {
			match = scheme + ":" + match
		}
		if !seen[match] {
			seen[match] = true
			result = append(result, match)
		}
	}
	return result
}

var errAntiBotChallenge = errors.New("anti-bot challenge")

func (c *Client) moviePage(ctx context.Context, current *session, siteMovieID string) ([]byte, error) {
	target := c.baseURL.ResolveReference(&url.URL{Path: "/index.php", RawQuery: "newsid=" + siteMovieID})
	body, _, err := c.get(ctx, current, target.String(), "")
	if err != nil {
		return nil, fmt.Errorf("fetch movie page: %w", err)
	}
	if isChallenge(body) {
		return nil, errAntiBotChallenge
	}
	return body, nil
}

func (c *Client) parseExternalTitleID(movieID string) (string, error) {
	if !strings.HasPrefix(movieID, c.domainPrefix) {
		return "", fmt.Errorf("%w: expected domain prefix %s", ErrInvalidTitleID, c.domainPrefix)
	}
	siteMovieID := strings.TrimPrefix(movieID, c.domainPrefix)
	if siteMovieID == "" {
		return "", fmt.Errorf("%w: site movie ID is empty", ErrInvalidTitleID)
	}
	if _, err := strconv.ParseUint(siteMovieID, 10, 64); err != nil {
		return "", fmt.Errorf("%w: site movie ID must be decimal", ErrInvalidTitleID)
	}
	return siteMovieID, nil
}

func decodeJSString(value string) (string, error) {
	var decoded string
	if err := json.Unmarshal([]byte(`"`+value+`"`), &decoded); err != nil {
		return "", err
	}
	return decoded, nil
}

func parseAudioNames(embedHTML []byte) []string {
	match := audioNames.FindSubmatch(embedHTML)
	if len(match) != 2 {
		return nil
	}
	var names []string
	if err := json.Unmarshal(match[1], &names); err != nil {
		return nil
	}
	return names
}

func parseSubtitles(embedHTML []byte) []SubtitleTrack {
	match := subtitlesPattern.FindSubmatch(embedHTML)
	if len(match) != 2 {
		return nil
	}
	var entries []embedSubtitleEntry
	if err := json.Unmarshal(match[1], &entries); err != nil {
		return nil
	}
	return subtitleTracks(entries)
}

func subtitleTracks(entries []embedSubtitleEntry) []SubtitleTrack {
	tracks := make([]SubtitleTrack, 0, len(entries))
	for index, entry := range entries {
		name := html.UnescapeString(entry.Name)
		tracks = append(tracks, SubtitleTrack{
			ID:          strconv.Itoa(index + 1),
			Name:        name,
			Language:    inferLanguage(name),
			Forced:      isForcedSubtitle(name),
			PlaylistURL: html.UnescapeString(entry.URL),
		})
	}
	return tracks
}

func inferLanguage(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "рус"), strings.Contains(lower, "rus"):
		return "ru"
	case strings.Contains(lower, "укр"), strings.Contains(lower, "ukr"):
		return "uk"
	case strings.Contains(lower, "eng"), strings.Contains(lower, "англ"):
		return "en"
	default:
		return ""
	}
}

func isForcedSubtitle(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "форс") || strings.Contains(lower, "forced")
}
