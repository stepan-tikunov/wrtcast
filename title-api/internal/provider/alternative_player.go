package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	playerViewportPattern = regexp.MustCompile(`<meta\s+name=["']viewporti["']\s+content=["']([^"']+)["']`)
	playerTokenPattern    = regexp.MustCompile(`(?s)const\s+userParam\s*=.*?\btoken\s*:\s*'([^']+)'`)
	playerFilesPattern    = regexp.MustCompile(`(?s)const\s+fileList\s*=\s*JSON\.parse\('((?:\\.|[^'])*)'\)`)
)

type alternativeCatalog struct {
	Type   string          `json:"type"`
	Active alternativeFile `json:"active"`
	// For a series: season -> episode -> translation -> file. Movies put
	// unrelated data here, so it is only decoded for series.
	All json.RawMessage `json:"all"`
}

type alternativeEpisodes map[string]map[string]map[string]alternativeFile

func (catalog alternativeCatalog) episodes() alternativeEpisodes {
	var episodes alternativeEpisodes
	if catalog.isSeries() {
		_ = json.Unmarshal(catalog.All, &episodes)
	}
	return episodes
}

type alternativeFile struct {
	ID          int64  `json:"id"`
	Translation string `json:"translation"`
}

type alternativePlayback struct {
	HLSSource []struct {
		Label   string            `json:"label"`
		Quality map[string]string `json:"quality"`
	} `json:"hlsSource"`
	Tracks []struct {
		Kind     string `json:"kind"`
		Label    string `json:"label"`
		Source   string `json:"src"`
		Language string `json:"language"`
		Default  bool   `json:"default"`
	} `json:"tracks"`
}

func alternativePlayerURLs(page []byte, scheme string) []string {
	matches := alternativePlayerPattern.FindAllString(html.UnescapeString(string(page)), -1)
	for index := range matches {
		matches[index] = strings.ReplaceAll(matches[index], `\/`, "/")
	}
	return uniqueEmbedURLs(matches, scheme)
}

// alternativePlayer is the bootstrap state of an alternative player page: its
// session, the values the playback request is signed with, and the catalog
// of files (one movie, or translations per season and episode).
type alternativePlayer struct {
	client    *http.Client
	playerURL *url.URL
	viewport  string
	token     string
	catalog   alternativeCatalog
}

func (catalog alternativeCatalog) isSeries() bool {
	return strings.EqualFold(catalog.Type, "serial")
}

// seasons adds the catalog's episodes to target without overwriting entries
// from the primary players, which carry titles and durations.
func (catalog alternativeCatalog) seasons(target map[int32]map[string]Episode) {
	for seasonKey, episodes := range catalog.episodes() {
		seasonNumber, err := strconv.ParseInt(strings.TrimSpace(seasonKey), 10, 32)
		if err != nil {
			continue
		}
		key := int32(seasonNumber)
		for episodeKey, translations := range episodes {
			number := strings.TrimSpace(episodeKey)
			if number == "" || len(translations) == 0 {
				continue
			}
			if target[key] == nil {
				target[key] = make(map[string]Episode)
			}
			if _, exists := target[key][number]; !exists {
				target[key][number] = Episode{Number: number}
			}
		}
	}
}

func (c *Client) loadAlternativePlayer(ctx context.Context, rawPlayerURL string) (*alternativePlayer, error) {
	playerURL, err := url.Parse(rawPlayerURL)
	if err != nil || playerURL.Scheme == "" || playerURL.Hostname() == "" {
		return nil, errors.New("invalid alternative player URL")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: c.httpTimeout, Jar: jar}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, playerURL.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", c.userAgent)
	request.Header.Set("Referer", c.baseURL.String()+"/")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch player: %w", err)
	}
	body, err := readAlternativeResponse(response)
	if err != nil {
		return nil, fmt.Errorf("fetch player: %w", err)
	}

	viewport := firstSubmatch(playerViewportPattern, body)
	token := firstSubmatch(playerTokenPattern, body)
	catalogText := firstSubmatch(playerFilesPattern, body)
	if viewport == "" || token == "" || catalogText == "" {
		return nil, errors.New("player bootstrap data not found")
	}
	catalogText, err = decodeJSSingleQuotedString(catalogText)
	if err != nil {
		return nil, fmt.Errorf("decode player catalog: %w", err)
	}
	var catalog alternativeCatalog
	if err := json.Unmarshal([]byte(catalogText), &catalog); err != nil {
		preview := catalogText
		if len(preview) > 160 {
			preview = preview[:160]
		}
		return nil, fmt.Errorf("decode player catalog %q: %w", preview, err)
	}
	return &alternativePlayer{
		client: client, playerURL: playerURL, viewport: viewport, token: token, catalog: catalog,
	}, nil
}

func (c *Client) alternativeStreams(ctx context.Context, rawPlayerURL string, season int32, episode string) ([]MovieStream, error) {
	player, err := c.loadAlternativePlayer(ctx, rawPlayerURL)
	if err != nil {
		return nil, err
	}
	file, err := player.catalog.selectedFile(season, episode)
	if err != nil {
		return nil, err
	}

	playback, err := c.requestAlternativePlayback(ctx, player, file.ID)
	if err != nil {
		return nil, err
	}
	streams := c.alternativePlaybackStreams(player, file.ID, playback)
	if len(streams) == 0 {
		return nil, fmt.Errorf("none of %d sources has a quality", len(playback.HLSSource))
	}
	return streams, nil
}

func (c *Client) requestAlternativePlayback(ctx context.Context, player *alternativePlayer, fileID int64) (alternativePlayback, error) {
	client, playerURL, viewport, token := player.client, player.playerURL, player.viewport, player.token
	fingerprint := make([]byte, 32)
	if _, err := rand.Read(fingerprint); err != nil {
		return alternativePlayback{}, fmt.Errorf("create player fingerprint: %w", err)
	}
	form := url.Values{"token": {token}, "av1": {"true"}, "autoplay": {"0"}, "audio": {""}, "subtitle": {""}}
	endpoint := playerURL.ResolveReference(&url.URL{Path: "/bnsi/movies/" + strconv.FormatInt(fileID, 10)})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return alternativePlayback{}, err
	}
	request.Header.Set("User-Agent", c.userAgent)
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", playerURL.Scheme+"://"+playerURL.Host)
	request.Header.Set("Referer", playerURL.String())
	request.Header.Set("Borth", hex.EncodeToString(fingerprint)+"|"+playerChallenge(viewport))
	response, err := client.Do(request)
	if err != nil {
		return alternativePlayback{}, fmt.Errorf("request playback: %w", err)
	}
	body, err := readAlternativeResponse(response)
	if err != nil {
		return alternativePlayback{}, fmt.Errorf("request playback: %w", err)
	}
	var playback alternativePlayback
	if err := json.Unmarshal(body, &playback); err != nil {
		return alternativePlayback{}, fmt.Errorf("decode playback: %w", err)
	}
	if len(playback.HLSSource) == 0 {
		return alternativePlayback{}, fmt.Errorf("playback has no HLS sources: %.200s", body)
	}
	return playback, nil
}

func readAlternativeResponse(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("returned %s", response.Status)
	}
	return body, nil
}

func firstSubmatch(pattern *regexp.Regexp, body []byte) string {
	match := pattern.FindSubmatch(body)
	if len(match) != 2 {
		return ""
	}
	return string(match[1])
}

func decodeJSSingleQuotedString(value string) (string, error) {
	var quoted strings.Builder
	quoted.Grow(len(value) + 2)
	quoted.WriteByte('"')
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '"':
			quoted.WriteString(`\"`)
		case '\\':
			if index+1 < len(value) && value[index+1] == '\'' {
				quoted.WriteByte('\'')
				index++
			} else {
				quoted.WriteByte('\\')
			}
		default:
			quoted.WriteByte(value[index])
		}
	}
	quoted.WriteByte('"')
	var decoded string
	if err := json.Unmarshal([]byte(quoted.String()), &decoded); err != nil {
		return "", err
	}
	return decoded, nil
}

func (catalog alternativeCatalog) selectedFile(season int32, episode string) (alternativeFile, error) {
	if catalog.isSeries() {
		translations := catalog.episodes()[strconv.FormatInt(int64(season), 10)][strings.TrimSpace(episode)]
		keys := make([]string, 0, len(translations))
		for key := range translations {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if translations[key].ID > 0 {
				return translations[key], nil
			}
		}
		return alternativeFile{}, fmt.Errorf("%w: season %d episode %s", ErrEpisodeNotFound, season, episode)
	}
	if catalog.Active.ID <= 0 {
		return alternativeFile{}, errors.New("active movie file not found")
	}
	return catalog.Active, nil
}

// alternativePlaybackStreams lists every source without touching the CDN:
// each master is assembled only when a player first asks for it, so starting
// a title costs requests for the chosen source alone.
func (c *Client) alternativePlaybackStreams(player *alternativePlayer, fileID int64, playback alternativePlayback) []MovieStream {
	subtitles := make([]SubtitleTrack, 0, len(playback.Tracks))
	for index, track := range playback.Tracks {
		if track.Source == "" {
			continue
		}
		subtitles = append(subtitles, SubtitleTrack{ID: strconv.Itoa(index + 1), Name: track.Label, Language: track.Language, PlaylistURL: track.Source})
	}
	streams := make([]MovieStream, 0, len(playback.HLSSource))
	for _, source := range playback.HLSSource {
		if len(source.Quality) == 0 {
			continue
		}
		label := strings.TrimSpace(source.Label)
		id := c.masters.putLazy(c.alternativeMaster(player, fileID, source.Label, source.Quality))
		streams = append(streams, MovieStream{
			Name:              label,
			MasterPlaylistURL: c.mediaBaseURL + "/v1/media/" + id + "/master.m3u8",
			AudioTracks:       []AudioTrack{{ID: "alternative", Name: label, Language: languageFromLabel(source.Label), Default: true}},
			Subtitles:         subtitles,
			ProxyHeaders: []ProxyHeader{{
				Name:  "Origin",
				Value: player.playerURL.Scheme + "://" + player.playerURL.Host,
			}},
		})
	}
	return streams
}

// alternativeMaster resolves one source's qualities into a master. The CDN
// now and then rejects every freshly signed link at once (403, X-VD:
// token_decrypt) and accepts new ones later, so a failed attempt asks the
// player to sign the source again. Its token is single-use, which takes a
// fresh copy of the player page.
func (c *Client) alternativeMaster(
	player *alternativePlayer,
	fileID int64,
	label string,
	qualities map[string]string,
) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		for attempt, delay := range []time.Duration{0, time.Second, 3 * time.Second} {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(delay):
				}
				fresh, err := c.loadAlternativePlayer(ctx, player.playerURL.String())
				if err != nil {
					return nil, err
				}
				playback, err := c.requestAlternativePlayback(ctx, fresh, fileID)
				if err != nil {
					return nil, err
				}
				player = fresh
				for _, source := range playback.HLSSource {
					if source.Label == label {
						qualities = source.Quality
					}
				}
			}
			if master := c.syntheticMaster(ctx, player.client, player.playerURL, qualities); len(master) > 0 {
				return master, nil
			}
		}
		return nil, fmt.Errorf("no quality of %q resolved", strings.TrimSpace(label))
	}
}

func (c *Client) syntheticMaster(ctx context.Context, client *http.Client, playerURL *url.URL, qualities map[string]string) []byte {
	type variant struct {
		quality              int
		attributes, mediaURL string
	}
	variants := make(chan variant, len(qualities))
	var wait sync.WaitGroup
	for qualityText, alternatives := range qualities {
		quality, err := strconv.Atoi(qualityText)
		if err != nil {
			continue
		}
		link := strings.TrimSpace(strings.Split(alternatives, " or ")[0])
		if link == "" {
			continue
		}
		wait.Add(1)
		go func(quality int, link string) {
			defer wait.Done()
			attributes, mediaURL, err := c.resolveAlternativeVariant(ctx, client, playerURL, link)
			if err == nil {
				variants <- variant{quality: quality, attributes: attributes, mediaURL: mediaURL}
			}
		}(quality, link)
	}
	wait.Wait()
	close(variants)
	resolved := make([]variant, 0, len(qualities))
	for item := range variants {
		resolved = append(resolved, item)
	}
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].quality < resolved[j].quality })
	if len(resolved) == 0 {
		return nil
	}
	var master strings.Builder
	master.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	for _, item := range resolved {
		master.WriteString("#EXT-X-STREAM-INF:")
		master.WriteString(item.attributes)
		master.WriteByte('\n')
		master.WriteString(item.mediaURL)
		master.WriteByte('\n')
	}
	return []byte(master.String())
}

func (c *Client) resolveAlternativeVariant(ctx context.Context, client *http.Client, playerURL *url.URL, rawURL string) (string, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", err
	}
	request.Header.Set("User-Agent", c.userAgent)
	request.Header.Set("Origin", playerURL.Scheme+"://"+playerURL.Host)
	request.Header.Set("Referer", playerURL.String())
	response, err := client.Do(request)
	if err != nil {
		return "", "", err
	}
	body, err := readAlternativeResponse(response)
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	for index, line := range lines {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		for next := index + 1; next < len(lines); next++ {
			candidate := strings.TrimSpace(lines[next])
			if candidate == "" || strings.HasPrefix(candidate, "#") {
				continue
			}
			base, err := url.Parse(rawURL)
			if err != nil {
				return "", "", err
			}
			reference, err := url.Parse(candidate)
			if err != nil {
				return "", "", err
			}
			return strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"), base.ResolveReference(reference).String(), nil
		}
	}
	return "", "", errors.New("variant playlist not found in upstream master")
}

func languageFromLabel(label string) string {
	lower := strings.ToLower(label)
	switch {
	case strings.Contains(lower, "russian"):
		return "ru"
	case strings.Contains(lower, "ukrainian"):
		return "uk"
	case strings.Contains(lower, "english"):
		return "en"
	default:
		return ""
	}
}

func playerChallenge(input string) string {
	return playerScatter(playerTrailingZeros(playerBitLength(input)))
}

func playerBitLength(input string) string {
	n := len(input)
	max := bitLength(n - 1)
	groups := make([][]int, max+1)
	for i := 0; i < n; i++ {
		class := 0
		if i > 0 {
			class = bitLength(i)
		}
		groups[class] = append(groups[class], i)
	}
	return permuteGroups(input, groups, true)
}

func playerTrailingZeros(input string) string {
	n := len(input)
	max := bitLength(n - 1)
	groups := make([][]int, max+1)
	for i := 0; i < n; i++ {
		class := max
		if i > 0 {
			class = 0
			for value := i; value&1 == 0; value >>= 1 {
				class++
			}
		}
		groups[class] = append(groups[class], i)
	}
	return permuteGroups(input, groups, false)
}

func permuteGroups(input string, groups [][]int, reverse bool) string {
	out := make([]byte, len(input))
	offset := 0
	for step := 0; step < len(groups); step++ {
		group := step
		if reverse {
			group = len(groups) - 1 - step
		}
		for _, index := range groups[group] {
			out[index] = input[offset]
			offset++
		}
	}
	return string(out)
}

func playerScatter(input string) string {
	n := len(input)
	prime := nextPrime(n + 1)
	positions := make([]int, 0, n)
	seen := make(map[int]bool, n)
	value := 0
	for len(positions) < n {
		value = (value + 2) % prime
		if value < n && !seen[value] {
			seen[value] = true
			positions = append(positions, value)
		}
	}
	out := make([]byte, n)
	for i, position := range positions {
		out[position] = input[i]
	}
	return string(out)
}

func bitLength(value int) int {
	result := 0
	for value > 0 {
		result++
		value >>= 1
	}
	return result
}
func nextPrime(value int) int {
	for ; ; value++ {
		prime := true
		for divisor := 2; divisor*divisor <= value; divisor++ {
			if value%divisor == 0 {
				prime = false
				break
			}
		}
		if prime {
			return value
		}
	}
}
