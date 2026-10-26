package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	playerViewportPattern    = regexp.MustCompile(`<meta\s+name=["']viewporti["']\s+content=["']([^"']+)["']`)
	playerTokenPattern       = regexp.MustCompile(`(?s)const\s+userParam\s*=.*?\btoken\s*:\s*'([^']+)'`)
	playerFilesPattern       = regexp.MustCompile(`(?s)const\s+fileList\s*=\s*JSON\.parse\('((?:\\.|[^'])*)'\)`)
	playerConfigPattern      = regexp.MustCompile(`(?s)const\s+config\s*=\s*JSON\.parse\('((?:\\.|[^'])*)'\)`)
	playerFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	playerUserParamPattern   = regexp.MustCompile(`(?s)const\s+userParam\s*=\s*\{(.*?)\};`)
	playerMoviePattern       = regexp.MustCompile(`(?s)const\s+movie\s*=\s*\{(.*?)\};`)
	playerDomainPattern      = regexp.MustCompile(`\bdomain\s*:\s*'([^']*)'`)
	playerTypePattern        = regexp.MustCompile(`\btype\s*:\s*'([^']*)'`)
	playerMovieIDPattern     = regexp.MustCompile(`\bid\s*:\s*'([^']*)'`)
	stylesheetPattern        = regexp.MustCompile(`(?i)<link\b[^>]*\brel=["']?stylesheet\b[^>]*>`)
	scriptPattern            = regexp.MustCompile(`(?i)<script\b[^>]*\bsrc=[^>]*>`)
	hrefPattern              = regexp.MustCompile(`(?i)\b(?:href|src)=["']([^"']+)["']`)
	deferPattern             = regexp.MustCompile(`(?i)\sdefer\b`)
)

// alternativeGuard is a constant of the player bundle. Its HLS loader sends
// it as "Authorizations: Bearer <guard>" with every CDN request, and
// subtitle links get it as t=.
const alternativeGuard = "pXzvbyDGLYyB6VkwsWZDv3iMKZtsXNzpzRyxZUcsKHXxsSeaYakbo3hw9mBFRc5VQTpqAX6BW8aDEqyLaHYcXSQiV6KHYTVTK6MYRphNAy5sBjtrevqkDzKmLqNdfMZGEU9NELjmtKfZy3RNGzCd767sNh1mXEj4tCcvqndHtzmwAbZNkhm4ghDEasodotMBewypNQ56uotJAQGX11csfeRfBAPk8DcUWWkkqzxca8vbnEw12vUFbBzT6hz8ZB3F3dzUhUXoL2cr1WM1bXQArRCS1MUNMz3X5WDMMQoZKxj2AMTRqp7QQX4dDB9B7VzEZTmyFULhm1AcHHMkoMvSVvKYoBoAKLycYAgMHeD4ECJcGEAGpnkJhrV57zQ7"

// What the bundle loads besides the page's own tags: its icon sprite, an
// SVG <use> of the same origin, and the request it tells an ad blocker by
// (a HEAD to Google's IMA host, whose redirected flag is /stat's ab). Empty
// URLs skip them, as the tests do.
const playerIconPath = "/images/allplay.svg"

var adBlockProbeURL = "https://imasdk.googleapis.com/cekh8i"

// ErrCDNBlocked: the alternative player's CDN refuses this client or its
// session (403 with X-VD client_blocked or session_blocked). More requests
// only keep the block going.
var ErrCDNBlocked = errors.New("the CDN blocked this client")

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
	ID int64 `json:"id"`
	// A number, a string ("1547557") or null, as the catalog has it.
	IDFile      playerID `json:"id_file"`
	Translation string   `json:"translation"`
}

// statID is the id the player registers a view of the file under:
// id_file when it is above zero, else the id.
func (file alternativeFile) statID() int64 {
	if id, err := strconv.ParseInt(string(file.IDFile), 10, 64); err == nil && id > 0 {
		return id
	}
	return file.ID
}

type alternativePlayback struct {
	HLSSource []alternativeHLSSource `json:"hlsSource"`
	Tracks    []struct {
		Kind     string `json:"kind"`
		Label    string `json:"label"`
		Source   string `json:"src"`
		Language string `json:"language"`
		Default  bool   `json:"default"`
	} `json:"tracks"`
	// The playback socket, when the reply names one (see playbackSession).
	Pnr string `json:"pnr"`
	Pnk string `json:"pnk"`
	// When the links expire, in epoch milliseconds, after which the
	// player asks for new ones (see alternativePlayer.reload).
	Time int64 `json:"time"`
}

type alternativeHLSSource struct {
	Label   string            `json:"label"`
	Quality map[string]string `json:"quality"`
	AudioID playerID          `json:"audioId"`
}

// playerID is an id the player sends as a string or as a number.
type playerID string

func (id *playerID) UnmarshalJSON(data []byte) error {
	var text string
	if json.Unmarshal(data, &text) == nil {
		*id = playerID(text)
		return nil
	}
	var number json.Number
	if json.Unmarshal(data, &number) == nil {
		*id = playerID(number.String())
	}
	return nil
}

func alternativePlayerURLs(page []byte, scheme string) []string {
	matches := alternativePlayerPattern.FindAllString(html.UnescapeString(string(page)), -1)
	for index := range matches {
		matches[index] = strings.ReplaceAll(matches[index], `\/`, "/")
	}
	return uniqueEmbedURLs(matches, scheme)
}

// alternativeAds is the page's config.ads: the ad list's fields the
// replace map fills in, and how many lists the preroll may ask for.
type alternativeAds struct {
	Enabled bool   `json:"enabled"`
	Preroll string `json:"preroll"`
	Midroll []struct {
		Link string `json:"link"`
	} `json:"midroll"`
	Replace map[string]json.RawMessage `json:"replace"`
}

// alternativePlayer is the bootstrap state of an alternative player page: its
// session, the values the playback request is signed with, and the catalog
// of files (one movie, or translations per season and episode).
type alternativePlayer struct {
	browser     *browser
	playerURL   *url.URL
	fingerprint string
	token       string
	catalog     alternativeCatalog
	// What the player's telemetry (see viewSession) needs of the page:
	// userParam.domain, %-encoded as the page has it, whether movie.type
	// is a trailer, movie.id, the ads config, and when the page was asked
	// for (performance.now() counts from there) and the player started.
	domain                   string
	trailer                  bool
	movieID                  string
	ads                      alternativeAds
	navigationStart, started time.Time

	// challenge signs the next playback request. It starts as the page's
	// viewporti nonce, which is single-use, and each accepted request
	// answers with the next one in its Borth header. The last accepted
	// request went out and was answered at playbackSent, playbackAnswered;
	// its links expire at expires. The first one waits for ready, when the
	// page's scripts have run.
	mu                             sync.Mutex
	challenge                      string
	playbackSent, playbackAnswered time.Time
	expires                        time.Time
	ready                          time.Time
	reloads                        int // in a row, of which the player tries three
}

// cdnHeaders are what the player's HLS loader sends with every request to
// the CDN, from the master to the segments: the device fingerprint, the
// bundle's guard and the page it plays on. A router that fetches from the
// CDN itself sends them; title-api's own requests get Origin and Referer
// from the browser.
func (player *alternativePlayer) cdnHeaders() []ProxyHeader {
	return player.cdnHeadersWith(player.fingerprint)
}

// cdnHeadersWith has control in Accepts-Controls: the fingerprint, or the
// edge_hash of a playback socket once it has one.
func (player *alternativePlayer) cdnHeadersWith(control string) []ProxyHeader {
	return []ProxyHeader{
		{Name: "Origin", Value: player.playerURL.Scheme + "://" + player.playerURL.Host},
		{Name: "Referer", Value: player.playerURL.String()},
		{Name: "Accepts-Controls", Value: control},
		{Name: "Authorizations", Value: "Bearer " + alternativeGuard},
	}
}

// playerIdentity is what the player's fingerprint is a hash of, as far as
// it is ours to tell: the User-Agent without its version numbers (so a new
// Chrome is the same device, the Chromecast a different one), the time
// zone, the screen and the language. A stored fingerprint of another
// identity is of another device.
func playerIdentity() string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		regexp.MustCompile(`\d+(?:[._]\d+)+`).ReplaceAllString(chromeUserAgent, ""), browserTimeZone().name,
		fmt.Sprintf("%dx%d", screenWidth, screenHeight), navigatorLanguage,
	}, "||")))
	return hex.EncodeToString(digest[:8])
}

// playerFingerprint is the device fingerprint the alternative player knows
// us by: in Borth of playback requests and in Accepts-Controls of CDN
// requests. The real player derives a stable sha256 from the browser; a
// random one is made once per identity and kept in the fingerprint store
// as "<identity>:<fingerprint>", or for the life of the Client without one.
func (c *Client) playerFingerprint(ctx context.Context) (string, error) {
	c.fingerprintMu.Lock()
	defer c.fingerprintMu.Unlock()
	if c.fingerprint != "" {
		return c.fingerprint, nil
	}
	identity := playerIdentity()
	if c.fingerprintStore != nil {
		stored, err := c.fingerprintStore.LoadPlayerFingerprint(ctx)
		if err != nil {
			return "", err
		}
		if owner, fingerprint, ok := strings.Cut(stored, ":"); ok && owner == identity && playerFingerprintPattern.MatchString(fingerprint) {
			c.fingerprint = fingerprint
			return fingerprint, nil
		}
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create player fingerprint: %w", err)
	}
	fingerprint := hex.EncodeToString(random)
	if c.fingerprintStore != nil {
		if err := c.fingerprintStore.SavePlayerFingerprint(ctx, identity+":"+fingerprint); err != nil {
			return "", err
		}
	}
	c.fingerprint = fingerprint
	return fingerprint, nil
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

// pageTiming is the page's, and shorter in tests.
type pageTiming struct {
	// From the deferred scripts' arrival to the playback request: their
	// parse and run, the AV1 check and the player's ready, which the
	// bundle chains without a timer of its own.
	scriptRun [2]time.Duration
	// The player checks every reload whether its links expired.
	reload time.Duration
	// Before the source is set, the player waits this long for its
	// socket to open, and playback_start this long for an edge_hash.
	socketOpen, edgeHash time.Duration
}

var defaultPageTiming = pageTiming{
	scriptRun:  [2]time.Duration{250 * time.Millisecond, 900 * time.Millisecond},
	reload:     120 * time.Second,
	socketOpen: 5 * time.Second,
	edgeHash:   500 * time.Millisecond,
}

// loadAlternativePlayer loads a player page as Chrome loads the provider's
// iframe of it: the navigation, with the provider's origin as the
// cross-site Referer, then, as the preload scanner finds them, its
// stylesheet and scripts (from the HTTP cache while fresh), which the
// player's start waits for. The page's first playback request waits for
// its scripts to have run (ready); its icon sprite and the ad blocker
// probe go meanwhile.
func (c *Client) loadAlternativePlayer(ctx context.Context, rawPlayerURL string) (*alternativePlayer, error) {
	playerURL, err := url.Parse(rawPlayerURL)
	if err != nil || playerURL.Scheme == "" || playerURL.Hostname() == "" {
		return nil, errors.New("invalid alternative player URL")
	}
	fingerprint, err := c.playerFingerprint(ctx)
	if err != nil {
		return nil, err
	}
	provider := c.providerURL()
	navigationStart := time.Now()
	response, err := c.browser.do(ctx, browserRequest{
		kind: fetchNavigate, url: playerURL.String(), initiator: provider, referer: provider.String() + "/",
	})
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
	var config struct {
		Ads alternativeAds `json:"ads"`
	}
	if text, err := decodeJSSingleQuotedString(firstSubmatch(playerConfigPattern, body)); err == nil {
		_ = json.Unmarshal([]byte(text), &config)
	}
	userParam := []byte(firstSubmatch(playerUserParamPattern, body))
	movie := []byte(firstSubmatch(playerMoviePattern, body))
	player := &alternativePlayer{
		browser: c.browser, playerURL: playerURL, fingerprint: fingerprint, token: token, catalog: catalog,
		domain: firstSubmatch(playerDomainPattern, userParam), trailer: firstSubmatch(playerTypePattern, movie) == "trailer",
		movieID: firstSubmatch(playerMovieIDPattern, movie), ads: config.Ads,
		navigationStart: navigationStart,
		challenge:       viewport,
	}
	player.loadSubresources(ctx, body)
	player.started = time.Now()
	timing := c.pageTiming
	player.ready = player.started.Add(timing.scriptRun[0] + time.Duration(mathrand.Int64N(int64(timing.scriptRun[1]-timing.scriptRun[0])+1)))
	go player.startupRequests(player.ready)
	return player, nil
}

// loadSubresources fetches the page's stylesheets and scripts at once, as
// the preload scanner does, and waits for them, as the deferred scripts
// the player starts from are run only then. A failed one changes nothing
// else, as in a browser.
func (player *alternativePlayer) loadSubresources(ctx context.Context, page []byte) {
	type subresource struct {
		kind fetchKind
		url  string
	}
	var resources []subresource
	add := func(kind fetchKind, tag string) {
		reference := html.UnescapeString(firstSubmatch(hrefPattern, []byte(tag)))
		if target, err := player.playerURL.Parse(reference); reference != "" && err == nil &&
			(target.Scheme == "https" || target.Scheme == "http") {
			resources = append(resources, subresource{kind: kind, url: target.String()})
		}
	}
	for _, tag := range stylesheetPattern.FindAllString(string(page), -1) {
		add(fetchStyle, tag)
	}
	for _, tag := range scriptPattern.FindAllString(string(page), -1) {
		if deferPattern.MatchString(tag) {
			add(fetchDeferredScript, tag)
		} else {
			add(fetchScript, tag)
		}
	}
	var wait sync.WaitGroup
	for _, resource := range resources {
		wait.Go(func() {
			response, err := player.browser.do(ctx, browserRequest{
				kind: resource.kind, url: resource.url, initiator: player.playerURL, referer: player.playerURL.String(),
			})
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 16<<20))
				response.Body.Close()
			}
		})
	}
	wait.Wait()
}

// startupRequests are the page's requests that nothing waits for: the ad
// blocker probe while the bundle runs, the icon sprite once the player's
// controls are built.
func (player *alternativePlayer) startupRequests(ready time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	discard := func(request browserRequest) {
		if response, err := player.browser.do(ctx, request); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
		}
	}
	if adBlockProbeURL != "" {
		go discard(browserRequest{
			kind: fetchProbe, method: http.MethodHead, url: adBlockProbeURL,
			initiator: player.playerURL, referer: player.playerURL.String(),
		})
	}
	// The controls are built, and their <use> elements fetch the sprite,
	// just before the player's ready.
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Until(ready.Add(-15 * time.Millisecond))):
	}
	if icon, err := player.playerURL.Parse(playerIconPath); err == nil {
		discard(browserRequest{kind: fetchImage, url: icon.String(), initiator: player.playerURL, referer: player.playerURL.String()})
	}
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

	playback, err := c.requestAlternativePlayback(ctx, player, file.ID, false, false)
	if err != nil {
		return nil, err
	}
	streams := c.alternativePlaybackStreams(player, file, playback)
	if len(streams) == 0 {
		return nil, fmt.Errorf("none of %d sources has a quality", len(playback.HLSSource))
	}
	return streams, nil
}

// requestAlternativePlayback asks the player for the signed links of a file
// the way its jQuery call does, once the page's scripts have run, and keeps
// the challenge the reply hands on. The first request of a page sends the
// page's audio and subtitle (none); a reload of expired links sends only
// whether to play on, which it does when the playback is not paused. The
// receiver decodes no AV1, and the player's check (4K60 AV1 smoothly)
// fails on a PC without an AV1 decoder too.
func (c *Client) requestAlternativePlayback(ctx context.Context, player *alternativePlayer, fileID int64, reload, paused bool) (alternativePlayback, error) {
	player.mu.Lock()
	defer player.mu.Unlock()
	if wait := time.Until(player.ready); wait > 0 {
		select {
		case <-ctx.Done():
			return alternativePlayback{}, ctx.Err()
		case <-time.After(wait):
		}
	}
	playerURL := player.playerURL
	// The fields in the player's order, which url.Values.Encode would sort.
	form := "token=" + url.QueryEscape(player.token) + "&av1=false&autoplay=0&audio=&subtitle="
	if reload {
		autoplay := "1"
		if paused {
			autoplay = "0"
		}
		form = "token=" + url.QueryEscape(player.token) + "&av1=false&autoplay=" + autoplay
	}
	endpoint := playerURL.ResolveReference(&url.URL{Path: "/bnsi/movies/" + strconv.FormatInt(fileID, 10)})
	sent := time.Now()
	response, err := player.browser.do(ctx, browserRequest{
		kind: fetchXHR, method: http.MethodPost, url: endpoint.String(), initiator: playerURL, referer: playerURL.String(),
		// As jQuery sets them: the form's type, Accept, the call's own
		// headers, then X-Requested-With.
		header: [][2]string{
			{"Content-Type", "application/x-www-form-urlencoded; charset=UTF-8"}, {"Accept", "*/*"},
			{"Borth", player.fingerprint + "|" + playerChallenge(player.challenge)}, {"X-Requested-With", "XMLHttpRequest"},
		},
		body: []byte(form),
	})
	if err != nil {
		return alternativePlayback{}, fmt.Errorf("request playback: %w", err)
	}
	body, err := readAlternativeResponse(response)
	if err != nil {
		return alternativePlayback{}, fmt.Errorf("request playback: %w", err)
	}
	player.playbackSent, player.playbackAnswered = sent, time.Now()
	if next := response.Header.Get("Borth"); next != "" {
		player.challenge = next
	}
	var playback alternativePlayback
	if err := json.Unmarshal(body, &playback); err != nil {
		return alternativePlayback{}, fmt.Errorf("decode playback: %w", err)
	}
	if len(playback.HLSSource) == 0 {
		return alternativePlayback{}, fmt.Errorf("playback has no HLS sources: %.200s", body)
	}
	player.expires = time.Time{}
	if playback.Time > 0 {
		player.expires = time.UnixMilli(playback.Time)
	}
	player.reloads = 0
	return playback, nil
}

// alternativeStatusError is a refusal from the player or its CDN, which
// tells why in X-VD.
type alternativeStatusError struct {
	status string
	code   int
	vd     string
}

func (e *alternativeStatusError) Error() string {
	if e.vd != "" {
		return fmt.Sprintf("returned %s (X-VD: %s)", e.status, e.vd)
	}
	return "returned " + e.status
}

func readAlternativeResponse(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &alternativeStatusError{
			status: response.Status,
			code:   response.StatusCode,
			vd:     response.Header.Get("X-VD"),
		}
	}
	return io.ReadAll(io.LimitReader(response.Body, 4<<20))
}

// cdnBlocked tells whether err is the CDN's refusal of the client itself,
// with its X-VD value, as opposed to a rejected link.
func cdnBlocked(err error) (string, bool) {
	var refused *alternativeStatusError
	if !errors.As(err, &refused) || refused.code != http.StatusForbidden {
		return "", false
	}
	return refused.vd, blockedVD(refused.vd)
}

func blockedVD(vd string) bool {
	return vd == "client_blocked" || vd == "session_blocked"
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
func (c *Client) alternativePlaybackStreams(player *alternativePlayer, file alternativeFile, playback alternativePlayback) []MovieStream {
	subtitles := make([]SubtitleTrack, 0, len(playback.Tracks))
	for index, track := range playback.Tracks {
		if track.Source == "" {
			continue
		}
		// As the player does, even when the link has a query already.
		source := track.Source + "?t=" + alternativeGuard + "&b=" + player.fingerprint
		subtitles = append(subtitles, SubtitleTrack{ID: strconv.Itoa(index + 1), Name: track.Label, Language: track.Language, PlaylistURL: source})
	}
	// The sources are the dubs of one file of one page, which the player
	// switches between: one view and one media state for all of them.
	view := newViewSession(c, player, file.statID())
	media := newMediaState()
	for _, subtitle := range subtitles {
		media.allowSubtitle(subtitle.PlaylistURL)
	}
	streams := make([]MovieStream, 0, len(playback.HLSSource))
	for index, source := range playback.HLSSource {
		if len(source.Quality) == 0 {
			continue
		}
		label := strings.TrimSpace(source.Label)
		// The ids look like 1-based indexes, which stand in for a missing one.
		trackID := string(source.AudioID)
		if trackID == "" {
			trackID = strconv.Itoa(index + 1)
		}
		alternative := &alternativeSource{
			client: c, fileID: file.ID, statID: file.statID(), label: source.Label, trackID: trackID,
			player: player, qualities: source.Quality, pnr: playback.Pnr, pnk: playback.Pnk, view: view, media: media,
			levels: map[string]*qualityLevel{}, variants: map[string]variantInfo{},
		}
		id := c.masters.putSource(alternative)
		alternative.id = id
		streams = append(streams, MovieStream{
			Name:              label,
			MasterPlaylistURL: c.mediaBaseURL + "/v1/media/" + id + "/master.m3u8",
			AudioTracks:       []AudioTrack{{ID: "alternative", Name: label, Language: languageFromLabel(source.Label), Default: true}},
			Subtitles:         subtitles,
			ProxyHeaders:      player.cdnHeaders(),
		})
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
