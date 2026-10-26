package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The primary player is down (the provider's embed host answers 422), so the
// media type and the episode list must come from the alternative player.
func TestAlternativePlayerFallback(t *testing.T) {
	t.Parallel()

	catalogs := map[string]string{
		"aaa111": `{"type":"serial","all":{"1":{"1":{"t1":{"id":11,"translation":"Dub"}},"2":{"t1":{"id":12,"translation":"Dub"}}},"x":{"1":{}}}}`,
		"bbb222": `{"type":"movie","active":{"id":7,"translation":"Dub"},"all":{"directors":["x"]}}`,
	}
	player := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = fmt.Fprintf(response, `<meta name="viewporti" content="abc">
<script>const userParam = { token: 'tok' }; const fileList = JSON.parse('%s');</script>`,
			catalogs[request.URL.Query().Get("token_movie")])
	}))
	defer player.Close()

	var siteURL string
	site := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = response.Write([]byte("ready"))
		case "/index.php":
			movie := map[string]string{"20973": "aaa111", "42185": "bbb222"}[request.URL.Query().Get("newsid")]
			_, _ = fmt.Fprintf(response, `<iframe src="%s/embed/movie/378"></iframe>
<iframe src="%s/?token_movie=%s&amp;token=def456"></iframe>`, siteURL, player.URL, movie)
		default:
			response.WriteHeader(http.StatusUnprocessableEntity)
		}
	}))
	defer site.Close()
	siteURL = site.URL

	client, err := NewClient(Config{BaseURL: site.URL, RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}

	seriesID := client.ExternalTitleID("20973")
	_, page, _, err := client.movieDocument(ctx, seriesID)
	if err != nil {
		t.Fatal(err)
	}
	if mediaType, err := client.mediaTypeFromAlternativePlayers(ctx, page); err != nil || mediaType != MediaTypeSeries {
		t.Fatalf("series media type = %v, %v", mediaType, err)
	}
	seasons, err := client.Series(ctx, seriesID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seasons) != 1 || seasons[0].Number != 1 || len(seasons[0].Episodes) != 2 ||
		seasons[0].Episodes[0].Number != "1" || seasons[0].Episodes[1].Number != "2" {
		t.Fatalf("unexpected seasons: %+v", seasons)
	}

	_, page, _, err = client.movieDocument(ctx, client.ExternalTitleID("42185"))
	if err != nil {
		t.Fatal(err)
	}
	if mediaType, err := client.mediaTypeFromAlternativePlayers(ctx, page); err != nil || mediaType != MediaTypeMovie {
		t.Fatalf("movie media type = %v, %v", mediaType, err)
	}
}

// fakeAlternativePlayer is a player page, its /bnsi endpoint and its CDN.
// The page's viewporti nonce is single-use and each accepted playback
// request hands on the next challenge in Borth (unless chain is off). Links
// signed by the playback request numbered goodFrom or later are accepted by
// the CDN; the rest get cdnRefusal as X-VD. With pnr set the playback
// reply names a socket.
type fakeAlternativePlayer struct {
	t          *testing.T
	chain      bool
	goodFrom   int
	cdnRefusal string
	pnr, pnk   string

	mu          sync.Mutex
	player, cdn *httptest.Server
	pages       int
	playbacks   int
	cdnRequests int
	preflights  int
	challenge   string
	used        map[string]bool
	fingerprint string
	telemetry   []telemetryRecord
	refuse      int    // telemetry answers 403
	segments    string // X-VD of a 403 for every segment, if set
}

// telemetryRecord is a telemetry request the player page got.
type telemetryRecord struct {
	path   string
	header http.Header
	body   string
	at     time.Time
}

func newFakeAlternativePlayer(t *testing.T, chain bool, goodFrom int, cdnRefusal string) *fakeAlternativePlayer {
	fake := &fakeAlternativePlayer{t: t, chain: chain, goodFrom: goodFrom, cdnRefusal: cdnRefusal, used: map[string]bool{}}
	fake.cdn = httptest.NewServer(http.HandlerFunc(fake.serveCDN))
	fake.player = httptest.NewServer(http.HandlerFunc(fake.servePlayer))
	t.Cleanup(fake.cdn.Close)
	t.Cleanup(fake.player.Close)
	return fake
}

func (fake *fakeAlternativePlayer) url() string {
	return fake.player.URL + "/?token_movie=abc&token=tok"
}

func (fake *fakeAlternativePlayer) servePlayer(response http.ResponseWriter, request *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if path := request.URL.Path; path == "/stat" || path == "/stats" || path == "/events" || path == "/errors" {
		body, _ := io.ReadAll(request.Body)
		fake.telemetry = append(fake.telemetry, telemetryRecord{path: path, header: request.Header.Clone(), body: string(body), at: time.Now()})
		if fake.refuse > 0 {
			response.WriteHeader(fake.refuse)
		}
		return
	}
	if request.Method != http.MethodPost {
		if request.URL.Path != "/" {
			return
		}
		fake.pages++
		fake.challenge = fmt.Sprintf("viewport-nonce-%d", fake.pages)
		// The player's iframe is cross-site to the provider's page: only a
		// SameSite=None cookie is kept, as in Chrome.
		http.SetCookie(response, &http.Cookie{Name: "page", Value: "1", Path: "/", SameSite: http.SameSiteNoneMode, Secure: true})
		http.SetCookie(response, &http.Cookie{Name: "lax", Value: "1", Path: "/"})
		_, _ = fmt.Fprintf(response, `<meta name="viewporti" content="%s">
<script>const userParam = { token: 'tok' }; const fileList = JSON.parse('{"type":"movie","active":{"id":7,"translation":"Dub"},"all":{}}');</script>`, fake.challenge)
		return
	}
	body, _ := io.ReadAll(request.Body)
	for name, want := range map[string]string{
		"Accept":           "*/*",
		"Content-Type":     "application/x-www-form-urlencoded; charset=UTF-8",
		"X-Requested-With": "XMLHttpRequest",
		"Origin":           fake.player.URL,
		"Referer":          fake.url(),
	} {
		if got := request.Header.Get(name); got != want {
			fake.t.Errorf("playback request %s = %q, want %q", name, got, want)
		}
	}
	if string(body) != "token=tok&av1=false&autoplay=0&audio=&subtitle=" {
		fake.t.Errorf("playback request body = %q", body)
	}
	fingerprint, signed, _ := strings.Cut(request.Header.Get("Borth"), "|")
	if !playerFingerprintPattern.MatchString(fingerprint) ||
		fake.fingerprint != "" && fingerprint != fake.fingerprint {
		fake.t.Errorf("playback request fingerprint = %q, earlier %q", fingerprint, fake.fingerprint)
	}
	fake.fingerprint = fingerprint
	if signed != playerChallenge(fake.challenge) || fake.used[fake.challenge] {
		response.WriteHeader(http.StatusForbidden)
		return
	}
	fake.used[fake.challenge] = true
	fake.playbacks++
	if fake.chain {
		fake.challenge = fmt.Sprintf("chained-challenge-%d", fake.playbacks)
		response.Header().Set("Borth", fake.challenge)
	}
	socket := ""
	if fake.pnr != "" {
		socket = fmt.Sprintf(`,"pnr":%q,"pnk":%q`, fake.pnr, fake.pnk)
	}
	_, _ = fmt.Fprintf(response, `{"hlsSource":[{"label":"(Russian) Dub","audioId":2,"quality":{"480":"%[1]s/480.m3u8?sig=%[2]d","720":"%[1]s/720.m3u8?sig=%[2]d"}}]%[3]s}`,
		fake.cdn.URL, fake.playbacks, socket)
}

func (fake *fakeAlternativePlayer) serveCDN(response http.ResponseWriter, request *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	// The CORS preflight Chrome sends first, for hls.js's own headers.
	if request.Method == http.MethodOptions {
		fake.preflights++
		if got := request.Header.Get("Access-Control-Request-Headers"); got != "accepts-controls,authorizations" ||
			request.Header.Get("Origin") != fake.player.URL || request.Header.Get("Cookie") != "" {
			fake.t.Errorf("CDN preflight %v", request.Header)
		}
		response.Header().Set("Access-Control-Allow-Origin", "*")
		response.Header().Set("Access-Control-Allow-Headers", "accepts-controls, authorizations")
		response.Header().Set("Access-Control-Max-Age", "600")
		response.WriteHeader(http.StatusNoContent)
		return
	}
	fake.cdnRequests++
	for name, want := range map[string]string{
		"Accepts-Controls": fake.fingerprint,
		"Authorizations":   "Bearer " + alternativeGuard,
		"Origin":           fake.player.URL,
		"Referer":          fake.url(),
	} {
		if got := request.Header.Get(name); got != want {
			fake.t.Errorf("CDN request %s = %q, want %q", name, got, want)
		}
	}
	if sig, _ := strconv.Atoi(request.URL.Query().Get("sig")); request.URL.Query().Has("sig") && sig < fake.goodFrom {
		response.Header().Set("X-VD", fake.cdnRefusal)
		response.WriteHeader(http.StatusForbidden)
		return
	}
	if strings.HasSuffix(request.URL.Path, "/media.m3u8") {
		_, _ = response.Write([]byte("#EXTM3U\n#EXTINF:10,\nseg-0.ts\n#EXTINF:10,\nseg-1.ts\n#EXT-X-ENDLIST\n"))
		return
	}
	if strings.HasSuffix(request.URL.Path, ".ts") {
		if fake.segments != "" {
			response.Header().Set("X-VD", fake.segments)
			response.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = response.Write(make([]byte, 50000))
		return
	}
	_, _ = response.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000,RESOLUTION=1280x720\nmedia.m3u8\n"))
}

// telemetryOf is the telemetry requests to path so far.
func (fake *fakeAlternativePlayer) telemetryOf(path string) []telemetryRecord {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var records []telemetryRecord
	for _, record := range fake.telemetry {
		if record.path == path {
			records = append(records, record)
		}
	}
	return records
}

func (fake *fakeAlternativePlayer) counts() (pages, playbacks, cdnRequests int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.pages, fake.playbacks, fake.cdnRequests
}

func alternativeMasterID(t *testing.T, client *Client, fake *fakeAlternativePlayer) string {
	t.Helper()
	streams, err := client.alternativeStreams(context.Background(), fake.url(), 0, "")
	if pages, playbacks, cdnRequests := fake.counts(); err != nil || len(streams) != 1 ||
		pages != 1 || playbacks != 1 || cdnRequests != 0 {
		t.Fatalf("streams = %+v, err = %v after %d pages, %d playback and %d CDN requests",
			streams, err, pages, playbacks, cdnRequests)
	}
	return strings.TrimSuffix(strings.TrimPrefix(streams[0].MasterPlaylistURL, "https://api.example.com/v1/media/"), "/master.m3u8")
}

// The CDN sometimes rejects every freshly signed link (403 token_decrypt);
// a new playback request a moment later gets links that work. Sources are
// listed without CDN requests and only the requested one is resolved, one
// quality at a time. The new request is signed with the challenge the last
// reply handed on, or, when there is none, from a fresh copy of the page.
func TestAlternativePlayerRetriesRejectedLinks(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		chain bool
		pages int
	}{
		{name: "chained challenge", chain: true, pages: 1},
		{name: "fresh page", chain: false, pages: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeAlternativePlayer(t, test.chain, 2, "token_decrypt")
			client, err := NewClient(Config{BaseURL: "https://example.com", MediaBaseURL: "https://api.example.com"})
			if err != nil {
				t.Fatal(err)
			}
			id := alternativeMasterID(t, client, fake)
			master, err := client.MasterPlaylist(context.Background(), id)
			pages, playbacks, cdnRequests := fake.counts()
			// The first attempt's master, of the 480 the page starts
			// with, is refused; the second attempt's loads, then the
			// 720 one, and the master lists both qualities.
			if err != nil || !strings.Contains(string(master), "/quality/480/playlist.m3u8") ||
				!strings.Contains(string(master), "/quality/720/playlist.m3u8") ||
				pages != test.pages || playbacks != 2 || cdnRequests != 3 {
				t.Fatalf("master = %q, err = %v after %d pages, %d playback and %d CDN requests",
					master, err, pages, playbacks, cdnRequests)
			}
		})
	}
}

// A CDN that blocks the client is not asked again, nor is the player.
func TestAlternativePlayerStopsWhenCDNBlocksClient(t *testing.T) {
	t.Parallel()
	for _, refusal := range []string{"client_blocked", "session_blocked"} {
		t.Run(refusal, func(t *testing.T) {
			t.Parallel()
			fake := newFakeAlternativePlayer(t, true, 1000, refusal)
			client, err := NewClient(Config{BaseURL: "https://example.com", MediaBaseURL: "https://api.example.com"})
			if err != nil {
				t.Fatal(err)
			}
			id := alternativeMasterID(t, client, fake)
			_, err = client.MasterPlaylist(context.Background(), id)
			pages, playbacks, cdnRequests := fake.counts()
			if !errors.Is(err, ErrCDNBlocked) || !strings.Contains(err.Error(), refusal) ||
				pages != 1 || playbacks != 1 || cdnRequests != 1 {
				t.Fatalf("err = %v after %d pages, %d playback and %d CDN requests", err, pages, playbacks, cdnRequests)
			}
		})
	}
}
