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
)

var (
	playerViewportPattern = regexp.MustCompile(`<meta\s+name=["']viewporti["']\s+content=["']([^"']+)["']`)
	playerTokenPattern    = regexp.MustCompile(`(?s)const\s+userParam\s*=.*?\btoken\s*:\s*'([^']+)'`)
	playerFilesPattern    = regexp.MustCompile(`(?s)const\s+fileList\s*=\s*JSON\.parse\('((?:\\.|[^'])*)'\)`)
)

type alternativeCatalog struct {
	Type   string                                           `json:"type"`
	Active alternativeFile                                  `json:"active"`
	All    map[string]map[string]map[string]alternativeFile `json:"all"`
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

func (c *Client) alternativeStreams(ctx context.Context, rawPlayerURL string, season int32, episode string) ([]MovieStream, error) {
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
	file, err := catalog.selectedFile(season, episode)
	if err != nil {
		return nil, err
	}

	fingerprint := make([]byte, 32)
	if _, err := rand.Read(fingerprint); err != nil {
		return nil, fmt.Errorf("create player fingerprint: %w", err)
	}
	form := url.Values{"token": {token}, "av1": {"true"}, "autoplay": {"0"}, "audio": {""}, "subtitle": {""}}
	endpoint := playerURL.ResolveReference(&url.URL{Path: "/bnsi/movies/" + strconv.FormatInt(file.ID, 10)})
	request, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", c.userAgent)
	request.Header.Set("Accept", "application/json, text/plain, */*")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", playerURL.Scheme+"://"+playerURL.Host)
	request.Header.Set("Referer", playerURL.String())
	request.Header.Set("Borth", hex.EncodeToString(fingerprint)+"|"+playerChallenge(viewport))
	response, err = client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request playback: %w", err)
	}
	body, err = readAlternativeResponse(response)
	if err != nil {
		return nil, fmt.Errorf("request playback: %w", err)
	}
	var playback alternativePlayback
	if err := json.Unmarshal(body, &playback); err != nil {
		return nil, fmt.Errorf("decode playback: %w", err)
	}
	return playback.streams(), nil
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
	if strings.EqualFold(catalog.Type, "serial") {
		translations := catalog.All[strconv.FormatInt(int64(season), 10)][strings.TrimSpace(episode)]
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

func (playback alternativePlayback) streams() []MovieStream {
	subtitles := make([]SubtitleTrack, 0, len(playback.Tracks))
	for index, track := range playback.Tracks {
		if track.Source == "" {
			continue
		}
		subtitles = append(subtitles, SubtitleTrack{ID: strconv.Itoa(index + 1), Name: track.Label, Language: track.Language, PlaylistURL: track.Source})
	}
	streams := make([]MovieStream, 0)
	for _, source := range playback.HLSSource {
		qualities := make([]string, 0, len(source.Quality))
		for quality := range source.Quality {
			qualities = append(qualities, quality)
		}
		sort.Slice(qualities, func(i, j int) bool {
			left, _ := strconv.Atoi(qualities[i])
			right, _ := strconv.Atoi(qualities[j])
			return left > right
		})
		for _, quality := range qualities {
			links := strings.Split(source.Quality[quality], " or ")
			if len(links) == 0 || strings.TrimSpace(links[0]) == "" {
				continue
			}
			name := strings.TrimSpace(source.Label) + " · " + quality + "p"
			streams = append(streams, MovieStream{MasterPlaylistURL: strings.TrimSpace(links[0]), AudioTracks: []AudioTrack{{ID: "alternative", Name: name, Language: languageFromLabel(source.Label), Default: true}}, Subtitles: subtitles})
		}
	}
	return streams
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
