package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The alternative player plays one quality at a time: hls.js loads the
// master of the quality the page starts with (the last one played, which
// the page keeps in its storage, else 480, else 360) as soon as the
// playback reply is in, and a quality change loads another one. title-api
// starts the same way, but the receiver needs every quality's attributes
// up front (a codec string copied from another quality names a profile
// and level its stream doesn't have, and the receiver fails to load it),
// so the other qualities' masters follow, one at a time. A source's
// master lists the qualities loaded, each pointing at title-api, which
// serves that quality's media playlist when the receiver asks for it
// (QualityPlaylist). The router
// fetches everything else of the session through title-api too
// (FetchMedia), so one Chrome plays the whole session to the CDN, and
// title-api sees every fragment, as hls.js does for its bandwidth
// estimate.

// How long a media playlist served is served again without asking the CDN:
// the router's look at it (is_fmp4) and the receiver's fetch cost one
// request.
const levelPlaylistTTL = 5 * time.Second

// After the CDN blocks the session, the relay refuses at once for this
// long instead of asking it again, as the player stops on a fatal error.
const blockedFor = 10 * time.Minute

// ErrMediaURL: the URL is not one of the media's CDN hosts.
var ErrMediaURL = errors.New("not a CDN URL of this media")

// alternativeSource is one source (a dub) of a file of the alternative
// player. Its master is assembled on first request, and when the playback
// reply named a socket (pnr, pnk) the source gets a playbackSession then.
// Its view reports the playback to the player's site; its media state,
// shared with the other dubs of the page, is what the page's hls.js knows.
type alternativeSource struct {
	client  *Client
	id      string // its media id
	fileID  int64
	statID  int64
	label   string
	trackID string
	media   *mediaState

	// Replaced by the retries of resolve, which the master store runs one
	// at a time, and by a reload of expired links; read under mu.
	mu        sync.Mutex
	player    *alternativePlayer
	qualities map[string]string
	levels    map[string]*qualityLevel // by quality
	variants  map[string]variantInfo   // of the loaded qualities
	pnr, pnk  string
	session   *playbackSession
	view      *viewSession
}

// qualityLevel is one quality of a source as hls.js has it once loaded:
// its master's variant and media playlist.
type qualityLevel struct {
	mu         sync.Mutex
	loaded     bool
	attributes string
	link, url  string // the master link that loaded, its media playlist
	// The media playlist as last served (see levelPlaylistTTL) and its
	// fragments.
	playlist  []byte
	servedAt  time.Time
	fragments []string
}

// cdnHeaders are the player's, with the edge_hash once the socket has one.
func (source *alternativeSource) cdnHeaders() []ProxyHeader {
	return source.playerOf().cdnHeadersWith(source.control())
}

func (source *alternativeSource) playerOf() *alternativePlayer {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.player
}

// control is Accepts-Controls of the moment: the socket's edge_hash, else
// the fingerprint.
func (source *alternativeSource) control() string {
	source.mu.Lock()
	player, session := source.player, source.session
	source.mu.Unlock()
	if session != nil {
		return session.control()
	}
	return player.fingerprint
}

// headers answers a headers request of the router: those of the socket,
// which the request keeps open, or the player's own without one. The
// request also keeps the view going and tells it what the router plays;
// while the view runs, the router asks again soon. Proxy names the relay
// the router's CDN requests go through.
func (source *alternativeSource) headers(report PlaybackReport) MediaHeaders {
	source.mu.Lock()
	player, session, view := source.player, source.session, source.view
	source.mu.Unlock()
	if report.Resolution != "" {
		source.client.preferQuality(report.Resolution)
	}
	change, active := view.touch(time.Now(), source, report)
	headers := MediaHeaders{Headers: player.cdnHeaders(), TTL: mediaHeadersTTL}
	if session != nil {
		headers = session.touch(report, change)
	}
	if active && headers.TTL > activeHeadersTTL {
		headers.TTL = activeHeadersTTL
	}
	headers.Proxy = "/v1/media/" + source.id + "/cdn"
	return headers
}

// end ends the view and closes the socket, if any, saying the playback
// ended.
func (source *alternativeSource) end() {
	source.mu.Lock()
	session, view := source.session, source.view
	source.mu.Unlock()
	view.end(source)
	if session != nil {
		session.end()
	}
}

// initialQuality is the quality the page starts with.
func (source *alternativeSource) initialQuality() string {
	source.mu.Lock()
	qualities := source.qualities
	source.mu.Unlock()
	if preferred := source.client.preferredQuality(); preferred != "" && qualities[preferred] != "" {
		return preferred
	}
	for _, quality := range []string{"480", "360"} {
		if qualities[quality] != "" {
			return quality
		}
	}
	return pickResolution(qualities, "0")
}

// resolve assembles the source's master. It opens the playback socket
// first, if the reply named one, and waits for it as the player does
// before it sets the source, then loads the quality the page starts with.
// The CDN now and then rejects every freshly signed link at once (403,
// X-VD: token_decrypt) and accepts new ones later, so a failed attempt
// asks the player to sign the source again: with the challenge its last
// reply handed on, or, should that be refused, with a fresh copy of the
// page and its viewporti nonce. A CDN that blocks the client itself ends
// the attempts.
func (source *alternativeSource) resolve(ctx context.Context) ([]byte, error) {
	var lastErr error
	for attempt, delay := range []time.Duration{0, time.Second, 3 * time.Second} {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			if err := source.resign(ctx); err != nil {
				return nil, err
			}
		}
		source.startSession(ctx)
		_, err := source.loadQuality(ctx, source.initialQuality())
		if err == nil {
			source.loadOtherQualities(ctx)
			return source.master(), nil
		}
		if vd, blocked := cdnBlocked(err); blocked {
			return nil, fmt.Errorf("%w (X-VD: %s)", ErrCDNBlocked, vd)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
	}
	return nil, fmt.Errorf("no quality of %q resolved: %w", strings.TrimSpace(source.label), lastErr)
}

// resign asks the player for new links of the source, from a fresh page
// should the challenge be refused (see resolve).
func (source *alternativeSource) resign(ctx context.Context) error {
	c := source.client
	player := source.playerOf()
	playback, err := c.requestAlternativePlayback(ctx, player, source.fileID, false, false)
	if _, blocked := cdnBlocked(err); err != nil && !blocked && ctx.Err() == nil {
		fresh, loadErr := c.loadAlternativePlayer(ctx, player.playerURL.String())
		if loadErr != nil {
			return loadErr
		}
		player = fresh
		playback, err = c.requestAlternativePlayback(ctx, player, source.fileID, false, false)
		if err == nil {
			// A page of its own, with a player of its own.
			source.mu.Lock()
			source.view = newViewSession(c, player, source.statID)
			source.mu.Unlock()
		}
	}
	if vd, blocked := cdnBlocked(err); blocked {
		return fmt.Errorf("%w (X-VD: %s)", ErrCDNBlocked, vd)
	}
	if err != nil {
		return err
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	for _, item := range playback.HLSSource {
		if item.Label == source.label {
			source.qualities = item.Quality
		}
	}
	source.player, source.pnr, source.pnk = player, playback.Pnr, playback.Pnk
	source.levels, source.variants = map[string]*qualityLevel{}, map[string]variantInfo{}
	return nil
}

// startSession opens the playback socket of a source whose playback reply
// named one, and waits, as the player does, for it to open and then
// briefly for the edge_hash of its playback_start; other sources stay as
// they are.
func (source *alternativeSource) startSession(ctx context.Context) {
	source.mu.Lock()
	if source.session == nil && source.pnk != "" &&
		(strings.HasPrefix(source.pnr, "wss://") || strings.HasPrefix(source.pnr, "ws://")) {
		source.session = newPlaybackSession(source.client, source.player, source.view, source.pnr, source.pnk, source.trackID, source.qualities)
	}
	session, view := source.session, source.view
	source.mu.Unlock()
	if session == nil {
		return
	}
	session.touch(PlaybackReport{}, view.current())
	timing := source.client.pageTiming
	if session.wait(ctx, timing.socketOpen, func() bool { return !session.openedAt.IsZero() || !session.running }) {
		session.wait(ctx, timing.edgeHash, func() bool { return session.edgeHash != "" || !session.running })
	}
}

// loadQuality loads a quality's master as hls.js does on a quality change:
// its first link, or the next of its " or " alternatives should one fail
// other than with a 403, which the others would only repeat.
func (source *alternativeSource) loadQuality(ctx context.Context, quality string) (*qualityLevel, error) {
	source.mu.Lock()
	level := source.levels[quality]
	if level == nil {
		level = &qualityLevel{}
		source.levels[quality] = level
	}
	links := source.qualities[quality]
	source.mu.Unlock()
	level.mu.Lock()
	defer level.mu.Unlock()
	if level.loaded {
		return level, nil
	}
	if links == "" {
		return nil, fmt.Errorf("%w: no quality %q", ErrMasterNotFound, quality)
	}
	lastErr := errors.New("quality has no link")
	for _, link := range strings.Split(links, " or ") {
		if link = strings.TrimSpace(link); link == "" {
			continue
		}
		source.media.allow(link)
		requested := time.Now()
		body, err := source.cdnGet(ctx, link, "manifest")
		if err == nil {
			level.attributes, level.url, err = firstVariant(link, body)
		}
		if err != nil {
			lastErr = err
			var refused *alternativeStatusError
			if errors.As(err, &refused) && refused.code == http.StatusForbidden || ctx.Err() != nil {
				break
			}
			continue
		}
		source.media.allow(level.url)
		level.link, level.loaded = link, true
		info := parseVariantInfo(level.attributes)
		info.requested, info.loaded = requested, time.Now()
		source.mu.Lock()
		source.variants[quality] = info
		source.mu.Unlock()
		return level, nil
	}
	return nil, lastErr
}

// loadOtherQualities loads the masters of the qualities not loaded yet,
// highest first; one that fails is left out of the master, and a block
// stops the rest.
func (source *alternativeSource) loadOtherQualities(ctx context.Context) {
	source.mu.Lock()
	keys := make([]int, 0, len(source.qualities))
	for key := range source.qualities {
		if value, err := strconv.Atoi(key); err == nil && value > 0 {
			keys = append(keys, value)
		}
	}
	source.mu.Unlock()
	sort.Sort(sort.Reverse(sort.IntSlice(keys)))
	for _, key := range keys {
		if _, err := source.loadQuality(ctx, strconv.Itoa(key)); err != nil {
			if _, blocked := cdnBlocked(err); blocked || ctx.Err() != nil {
				return
			}
		}
	}
}

// cdnGet loads a playlist of the source from the CDN as hls.js does, and
// tells the view of a failure (see viewSession.loadError).
func (source *alternativeSource) cdnGet(ctx context.Context, rawURL, kind string) ([]byte, error) {
	started := time.Now()
	response, err := source.cdnRequest(ctx, rawURL, false)
	if err != nil {
		source.loadFailed(kind, rawURL, 0, started, err)
		return nil, err
	}
	body, err := readAlternativeResponse(response)
	if err != nil {
		var refused *alternativeStatusError
		if errors.As(err, &refused) {
			source.loadFailed(kind, rawURL, refused.code, started, err)
			if blockedVD(refused.vd) {
				source.media.block(refused.vd)
			}
		}
		return nil, err
	}
	return body, nil
}

// cdnRequest is one request of the page to the CDN: hls.js's XHR, with the
// player's Accepts-Controls and Authorizations (and so a CORS preflight),
// or, for a subtitle, the <track> of a crossorigin video, a plain CORS GET.
func (source *alternativeSource) cdnRequest(ctx context.Context, rawURL string, subtitle bool) (*http.Response, error) {
	player := source.playerOf()
	request := browserRequest{
		kind: fetchXHR, url: rawURL, initiator: player.playerURL, referer: player.playerURL.String(),
		header: [][2]string{{"Accepts-Controls", source.control()}, {"Authorizations", "Bearer " + alternativeGuard}},
	}
	if subtitle {
		request.kind, request.header = fetchTrack, nil
	}
	return player.browser.do(ctx, request)
}

// firstVariant is the first variant of a quality's master: its attributes
// and its media playlist's URL.
func firstVariant(rawURL string, body []byte) (string, string, error) {
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
			resolved, err := absoluteURL(rawURL, candidate)
			if err != nil {
				return "", "", err
			}
			return strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"), resolved, nil
		}
	}
	return "", "", errors.New("variant playlist not found in upstream master")
}

func absoluteURL(base, reference string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	referenceURL, err := url.Parse(reference)
	if err != nil {
		return "", err
	}
	return baseURL.ResolveReference(referenceURL).String(), nil
}

// master is the source's master: every quality loaded, lowest first, with
// the attributes of its master, pointing at title-api's QualityPlaylist.
func (source *alternativeSource) master() []byte {
	source.mu.Lock()
	qualities, levels := source.qualities, source.levels
	source.mu.Unlock()
	keys := make([]int, 0, len(qualities))
	for key := range qualities {
		if value, err := strconv.Atoi(key); err == nil && value > 0 {
			keys = append(keys, value)
		}
	}
	sort.Ints(keys)
	var master strings.Builder
	master.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	for _, key := range keys {
		level := levels[strconv.Itoa(key)]
		if level == nil {
			continue
		}
		level.mu.Lock()
		text := ""
		if level.loaded {
			text = level.attributes
		}
		level.mu.Unlock()
		if text == "" {
			continue
		}
		fmt.Fprintf(&master, "#EXT-X-STREAM-INF:%s\n%s/v1/media/%s/quality/%d/playlist.m3u8\n", text, source.client.mediaBaseURL, source.id, key)
	}
	return []byte(master.String())
}

// QualityPlaylist is the media playlist of a quality of an alternative
// player's media, as the receiver plays it: the quality's master loaded
// first if it is not yet (the player's quality change), every URI made
// absolute, and the playlist served again for a few seconds (see
// levelPlaylistTTL).
func (c *Client) QualityPlaylist(ctx context.Context, id, quality string) ([]byte, error) {
	entry, err := c.masters.entry(id)
	if err != nil {
		return nil, err
	}
	if entry.source == nil {
		return nil, ErrMasterNotFound
	}
	if _, err := entry.playlist(ctx); err != nil {
		return nil, err
	}
	return entry.source.levelPlaylist(ctx, quality)
}

func (source *alternativeSource) levelPlaylist(ctx context.Context, quality string) ([]byte, error) {
	if vd, blocked := source.media.blocked(); blocked {
		return nil, fmt.Errorf("%w (X-VD: %s)", ErrCDNBlocked, vd)
	}
	level, err := source.loadQuality(ctx, quality)
	if err != nil {
		return nil, source.mediaErr(err)
	}
	level.mu.Lock()
	defer level.mu.Unlock()
	if level.playlist != nil && time.Since(level.servedAt) < levelPlaylistTTL {
		return level.playlist, nil
	}
	body, err := source.cdnGet(ctx, level.url, "level")
	if err != nil {
		return nil, source.mediaErr(err)
	}
	playlist, fragments := source.media.absorbPlaylist(level.url, body)
	level.playlist, level.servedAt, level.fragments = playlist, time.Now(), fragments
	return playlist, nil
}

// mediaErr is ErrCDNBlocked for a block, else err.
func (source *alternativeSource) mediaErr(err error) error {
	if vd, blocked := cdnBlocked(err); blocked {
		return fmt.Errorf("%w (X-VD: %s)", ErrCDNBlocked, vd)
	}
	return err
}

// MediaStatus is the status a failed media request is answered with: the
// CDN's own, with its X-VD, for a refusal of it; zero for any other error.
func MediaStatus(err error) (int, string) {
	var refused *alternativeStatusError
	if errors.As(err, &refused) {
		return refused.code, refused.vd
	}
	return 0, ""
}

// FetchMedia fetches a CDN URL of an alternative player's media for the
// router, as the page fetches it (see cdnRequest): only on the media's CDN
// hosts, the body streaming and the status passed on. A block of the CDN
// is answered with its 403 from then on (see blockedFor) without asking it
// again. Fragments feed the player's bandwidth estimate, failures its
// /errors.
func (c *Client) FetchMedia(ctx context.Context, id, rawURL string) (*http.Response, error) {
	entry, err := c.masters.entry(id)
	if err != nil {
		return nil, err
	}
	source := entry.source
	target, err := url.Parse(rawURL)
	if source == nil || err != nil || target.Scheme != "https" && target.Scheme != "http" {
		return nil, ErrMediaURL
	}
	if mapped := source.media.mapped(rawURL); mapped != "" {
		rawURL = mapped
	}
	subtitle, allowed := source.media.allowed(rawURL)
	if !allowed {
		return nil, ErrMediaURL
	}
	if vd, blocked := source.media.blocked(); blocked {
		return nil, &alternativeStatusError{status: "403 Forbidden", code: http.StatusForbidden, vd: vd}
	}
	started := time.Now()
	response, err := source.cdnRequest(ctx, rawURL, subtitle)
	kind := source.media.kindOf(rawURL)
	if err != nil {
		if !subtitle {
			source.loadFailed(kind, rawURL, 0, started, err)
		}
		return nil, err
	}
	if response.StatusCode >= 400 {
		vd := response.Header.Get("X-VD")
		if response.StatusCode == http.StatusForbidden && blockedVD(vd) {
			source.media.block(vd)
		}
		if !subtitle {
			source.loadFailed(kind, rawURL, response.StatusCode, started, &alternativeStatusError{status: response.Status, code: response.StatusCode, vd: vd})
		}
		return response, nil
	}
	if kind == "fragment" {
		response.Body = &measuredBody{ReadCloser: response.Body, first: time.Now(), done: func(bytes int64, elapsed time.Duration) {
			source.media.fragmentLoaded(rawURL, bytes, elapsed)
		}}
	}
	return response, nil
}

// loadFailed tells the view of a CDN request that failed: an hls.js error
// of a load of kind (manifest, level, fragment) with the HTTP status.
func (source *alternativeSource) loadFailed(kind, rawURL string, status int, started time.Time, err error) {
	source.mu.Lock()
	view := source.view
	source.mu.Unlock()
	view.loadError(source, kind, rawURL, status, started, err)
}

// measuredBody counts a fragment's bytes as it streams, and when it is
// over tells how many and how long they took from the first byte.
type measuredBody struct {
	io.ReadCloser
	first time.Time
	bytes int64
	once  sync.Once
	done  func(int64, time.Duration)
}

func (body *measuredBody) Read(buffer []byte) (int, error) {
	n, err := body.ReadCloser.Read(buffer)
	body.bytes += int64(n)
	if err == io.EOF {
		body.once.Do(func() { body.done(body.bytes, time.Since(body.first)) })
	}
	return n, err
}

// mediaState is what the page's hls.js knows of the CDN across the dubs
// of a page: the hosts of its links and playlists, which URLs are
// fragments (with their times) or subtitles, the bandwidth estimate, how
// far the fragments loaded reach, and a block.
type mediaState struct {
	mu        sync.Mutex
	hosts     map[string]bool
	subtitles map[string]bool
	fragments map[string]fragment
	inits     map[string]bool
	remap     map[string]string // old fragment URL -> the reloaded one
	estimator bandwidthEstimator
	loadedEnd float64 // the end of the last fragment loaded
	loads     int     // fragments loaded
	vd        string
	until     time.Time
}

// fragment is a fragment's place in its media playlist: its index and
// times.
type fragment struct {
	index      int
	start, end float64
}

func newMediaState() *mediaState {
	return &mediaState{
		hosts: map[string]bool{}, subtitles: map[string]bool{}, fragments: map[string]fragment{},
		inits: map[string]bool{}, remap: map[string]string{}, estimator: newBandwidthEstimator(),
	}
}

func hostOf(rawURL string) string {
	if target, err := url.Parse(rawURL); err == nil {
		return strings.ToLower(target.Host)
	}
	return ""
}

func (m *mediaState) allow(rawURL string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if host := hostOf(rawURL); host != "" {
		m.hosts[host] = true
	}
}

func (m *mediaState) allowSubtitle(rawURL string) {
	m.allow(rawURL)
	m.mu.Lock()
	m.subtitles[rawURL] = true
	m.mu.Unlock()
}

// allowed tells whether rawURL is on one of the media's hosts, and whether
// it is a subtitle.
func (m *mediaState) allowed(rawURL string) (subtitle, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.subtitles[rawURL], m.hosts[hostOf(rawURL)]
}

func (m *mediaState) kindOf(rawURL string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch _, fragment := m.fragments[rawURL]; {
	case fragment:
		return "fragment"
	case m.inits[rawURL]:
		return "init"
	case strings.Contains(strings.ToLower(strings.Split(rawURL, "?")[0]), ".m3u8"):
		return "level"
	default:
		return "fragment"
	}
}

func (m *mediaState) mapped(rawURL string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.remap[rawURL]
}

func (m *mediaState) block(vd string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vd, m.until = vd, time.Now().Add(blockedFor)
}

func (m *mediaState) blocked() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.vd, time.Now().Before(m.until)
}

func (m *mediaState) fragmentLoaded(rawURL string, bytes int64, elapsed time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.estimator.sample(elapsed, bytes)
	m.loads++
	if place, ok := m.fragments[rawURL]; ok {
		m.loadedEnd = place.end
	}
}

// stats is the bandwidth estimate (zero before a fragment loaded), how
// far the fragments loaded reach, and how many there were.
func (m *mediaState) stats() (bandwidth float64, loadedEnd float64, loads int) {
	if m == nil {
		return 0, 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if estimate, ok := m.estimator.estimate(); ok {
		bandwidth = estimate
	}
	return bandwidth, m.loadedEnd, m.loads
}

// absorbPlaylist makes a media playlist's URIs absolute, allows their
// hosts, and learns its fragments and their times and its init segments.
func (m *mediaState) absorbPlaylist(base string, body []byte) ([]byte, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	absolute := func(reference string) string {
		resolved, err := absoluteURL(base, reference)
		if err != nil {
			return reference
		}
		if host := hostOf(resolved); host != "" {
			m.hosts[host] = true
		}
		return resolved
	}
	var out strings.Builder
	var fragments []string
	position, duration := 0.0, 0.0
	for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "#"):
			if strings.HasPrefix(trimmed, "#EXTINF:") {
				value, _, _ := strings.Cut(strings.TrimPrefix(trimmed, "#EXTINF:"), ",")
				duration, _ = strconv.ParseFloat(strings.TrimSpace(value), 64)
			}
			init := strings.HasPrefix(trimmed, "#EXT-X-MAP:")
			line = uriAttribute.ReplaceAllStringFunc(line, func(match string) string {
				resolved := absolute(uriAttribute.FindStringSubmatch(match)[1])
				if init {
					m.inits[resolved] = true
				}
				return `URI="` + resolved + `"`
			})
		default:
			resolved := absolute(trimmed)
			m.fragments[resolved] = fragment{index: len(fragments), start: position, end: position + duration}
			fragments = append(fragments, resolved)
			position += duration
			line = resolved
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return []byte(strings.TrimSuffix(out.String(), "\n") + "\n"), fragments
}

var uriAttribute = regexp.MustCompile(`URI="([^"]*)"`)

// bandwidthEstimator is hls.js's EwmaBandWidthEstimator with its VoD
// settings: two moving averages of the fragments' throughput, with half-
// lives of 3 and 9 seconds of loading, of which the lower one counts.
type bandwidthEstimator struct {
	fast, slow ewma
}

type ewma struct {
	alpha, estimate, weight float64
}

func newBandwidthEstimator() bandwidthEstimator {
	return bandwidthEstimator{fast: ewma{alpha: math.Exp(math.Log(0.5) / 3)}, slow: ewma{alpha: math.Exp(math.Log(0.5) / 9)}}
}

func (e *ewma) sample(weight, value float64) {
	adjusted := math.Pow(e.alpha, weight)
	e.estimate = value*(1-adjusted) + adjusted*e.estimate
	e.weight += weight
}

func (e *ewma) value() float64 {
	if zero := 1 - math.Pow(e.alpha, e.weight); zero > 0 {
		return e.estimate / zero
	}
	return e.estimate
}

// sample takes in a fragment: its bytes over the time they took, at least
// the 50 ms hls.js counts any load as.
func (b *bandwidthEstimator) sample(elapsed time.Duration, bytes int64) {
	seconds := max(float64(elapsed)/float64(time.Second), 0.05)
	bits := 8 * float64(bytes) / seconds
	b.fast.sample(seconds, bits)
	b.slow.sample(seconds, bits)
}

func (b *bandwidthEstimator) estimate() (float64, bool) {
	if b.fast.weight < 0.001 {
		return 0, false
	}
	return min(b.fast.value(), b.slow.value()), true
}

// reloadLinks asks for new links once the source's expired, as the player
// checks every two minutes, and moves the qualities it loaded onto them:
// each master and media playlist again, as the player's new hls.js loads
// them, and the receiver's requests for the old fragments go to the new
// ones of the same place. The player tries three times in a row.
func (source *alternativeSource) reloadLinks(ctx context.Context, paused bool) {
	c := source.client
	player := source.playerOf()
	player.mu.Lock()
	due := !player.expires.IsZero() && !time.Now().Before(player.expires) && player.reloads <= 3
	if due {
		player.reloads++
	}
	player.mu.Unlock()
	if !due {
		return
	}
	playback, err := c.requestAlternativePlayback(ctx, player, source.fileID, true, paused)
	if err != nil {
		log.Printf("alternative player reload: %v", err)
		return
	}
	source.mu.Lock()
	old := source.levels
	for _, item := range playback.HLSSource {
		if item.Label == source.label {
			source.qualities = item.Quality
		}
	}
	source.levels, source.variants = map[string]*qualityLevel{}, map[string]variantInfo{}
	source.mu.Unlock()
	for quality, level := range old {
		level.mu.Lock()
		loaded, fragments := level.loaded, level.fragments
		level.mu.Unlock()
		if !loaded {
			continue
		}
		if _, err := source.levelPlaylist(ctx, quality); err != nil {
			log.Printf("alternative player reload of %s: %v", quality, err)
			continue
		}
		source.mu.Lock()
		fresh := source.levels[quality]
		source.mu.Unlock()
		fresh.mu.Lock()
		renewed := fresh.fragments
		fresh.mu.Unlock()
		source.media.mu.Lock()
		for index, previous := range fragments {
			if index < len(renewed) {
				source.media.remap[previous] = renewed[index]
				for older, newer := range source.media.remap {
					if newer == previous {
						source.media.remap[older] = renewed[index]
					}
				}
			}
		}
		source.media.mu.Unlock()
	}
}

// variantInfo is what the player's telemetry tells of a quality: the
// attributes of its variant and when its master was asked for and read.
type variantInfo struct {
	bandwidth, averageBandwidth int64
	frameRate                   float64
	width, height               int
	requested, loaded           time.Time
}

var (
	bandwidthAttribute        = regexp.MustCompile(`(?:^|,)BANDWIDTH=([0-9]+)`)
	averageBandwidthAttribute = regexp.MustCompile(`(?:^|,)AVERAGE-BANDWIDTH=([0-9]+)`)
	frameRateAttribute        = regexp.MustCompile(`(?:^|,)FRAME-RATE=([0-9.]+)`)
	resolutionAttribute       = regexp.MustCompile(`(?:^|,)RESOLUTION=([0-9]+)x([0-9]+)`)
)

func parseVariantInfo(attributes string) variantInfo {
	var info variantInfo
	number := func(pattern *regexp.Regexp, group int) string {
		if match := pattern.FindStringSubmatch(attributes); len(match) > group {
			return match[group]
		}
		return ""
	}
	info.bandwidth, _ = strconv.ParseInt(number(bandwidthAttribute, 1), 10, 64)
	info.averageBandwidth, _ = strconv.ParseInt(number(averageBandwidthAttribute, 1), 10, 64)
	info.frameRate, _ = strconv.ParseFloat(number(frameRateAttribute, 1), 64)
	info.width, _ = strconv.Atoi(number(resolutionAttribute, 1))
	info.height, _ = strconv.Atoi(number(resolutionAttribute, 2))
	return info
}
