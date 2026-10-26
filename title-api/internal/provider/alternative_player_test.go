package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRealAlternativePlayer(t *testing.T) {
	if os.Getenv("MOVIE_PROVIDER_INTEGRATION") == "" {
		t.Skip("set MOVIE_PROVIDER_INTEGRATION=1 to call the live service")
	}
	client, err := NewClient(Config{BaseURL: os.Getenv("MOVIE_PROVIDER_BASE_URL"), RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_, page, _, err := client.movieDocument(ctx, client.ExternalTitleID("43222"))
	if err != nil {
		t.Fatal(err)
	}
	urls := alternativePlayerURLs(page, client.providerURL().Scheme)
	if len(urls) == 0 {
		t.Fatalf("alternative player not found; token_movie offset=%d, cdn offset=%d", strings.Index(string(page), "token_movie"), strings.Index(string(page), "cdn."))
	}
	streams, err := client.alternativeStreams(ctx, urls[0], 1, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) == 0 {
		t.Fatal("alternative player returned no streams")
	}
}

func TestAlternativePlayerBootstrapAndSelection(t *testing.T) {
	t.Parallel()
	page := []byte(`<iframe data-src="https:\/\/cdn.example\/?token_movie=abc123&amp;token=def456"></iframe>`)
	urls := alternativePlayerURLs(page, "https")
	if len(urls) != 1 || urls[0] != "https://cdn.example/?token_movie=abc123&token=def456" {
		t.Fatalf("unexpected player URLs: %#v", urls)
	}

	catalog := alternativeCatalog{Type: "serial", All: json.RawMessage(`{"1":{"2":{"t66":{"id":985376,"translation":"Dub"}}}}`)}
	file, err := catalog.selectedFile(1, "2")
	if err != nil || file.ID != 985376 {
		t.Fatalf("selectedFile() = %+v, %v", file, err)
	}
}

func TestPlayerChallenge(t *testing.T) {
	t.Parallel()
	input := "1noQZdRf5yQxlcFB4oU2hIiRzHz2l2nDVVEc9VYx5IFVo81z81Bz41nyZOMTeVVEkTTT33EUNRNaMQVNMOMNMMdW"
	want := "cVRfWV9zdEo2MkV8MTc5MTY1OTQ4M3xBN3UyNE5zRUZiNzI4aHFQMzF1Q2doVlVnN2IxZnoyODRhMV81TVBleE1n"
	if got := playerChallenge(input); got != want {
		t.Fatalf("playerChallenge() = %q, want %q", got, want)
	}
}

func TestAlternativePlaybackStreams(t *testing.T) {
	t.Parallel()
	const fingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	requests, preflights := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodOptions {
			preflights++
			if request.Header.Get("Access-Control-Request-Headers") != "accepts-controls,authorizations" ||
				request.Header.Get("Origin") != "https://player.example" {
				t.Errorf("CDN preflight headers: %v", request.Header)
			}
			response.Header().Set("Access-Control-Allow-Origin", "*")
			response.Header().Set("Access-Control-Allow-Headers", "Accepts-Controls, Authorizations")
			response.WriteHeader(http.StatusNoContent)
			return
		}
		requests++
		if request.Header.Get("Accepts-Controls") != fingerprint ||
			request.Header.Get("Authorizations") != "Bearer "+alternativeGuard ||
			request.Header.Get("Origin") != "https://player.example" ||
			request.Header.Get("Referer") != "https://player.example/?token_movie=abc&token=def" ||
			request.Header.Get("Sec-Fetch-Mode") != "cors" || request.Header.Get("Sec-Fetch-Site") != "cross-site" ||
			request.Header.Get("Cookie") != "" || request.UserAgent() != chromeUserAgent {
			t.Errorf("CDN request headers: %v", request.Header)
		}
		switch request.URL.Path {
		case "/720/master.m3u8":
			_, _ = response.Write([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000,RESOLUTION=1280x720,CODECS=\"avc1.64001f,mp4a.40.2\"\nmedia.m3u8\n"))
		case "/720/media.m3u8":
			_, _ = response.Write([]byte("#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6,\nseg-1.m4s\n#EXTINF:4,\nhttps://edge.example/seg-2.m4s\n#EXT-X-ENDLIST\n"))
		default:
			t.Errorf("CDN request for %s", request.URL)
		}
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: "https://example.com", MediaBaseURL: "https://api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	var playback alternativePlayback
	playback.HLSSource = append(playback.HLSSource, alternativeHLSSource{Label: "(Russian) Dub", Quality: map[string]string{
		"720": server.URL + "/720/master.m3u8",
	}})
	playback.Tracks = append(playback.Tracks, struct {
		Kind     string `json:"kind"`
		Label    string `json:"label"`
		Source   string `json:"src"`
		Language string `json:"language"`
		Default  bool   `json:"default"`
	}{Kind: "captions", Label: "Russian", Source: "https://cdn.example/sub.vtt?lang=ru", Language: "rus", Default: true})

	playerURL, _ := url.Parse("https://player.example/?token_movie=abc&token=def")
	player := &alternativePlayer{browser: client.browser, playerURL: playerURL, fingerprint: fingerprint}
	streams := client.alternativePlaybackStreams(player, alternativeFile{ID: 7}, playback)
	if requests != 0 {
		t.Fatalf("listing sources made %d CDN requests", requests)
	}
	if len(streams) != 1 || streams[0].Name != "(Russian) Dub" ||
		streams[0].AudioTracks[0].Name != "(Russian) Dub" ||
		streams[0].AudioTracks[0].Language != "ru" || len(streams[0].Subtitles) != 1 {
		t.Fatalf("unexpected streams: %+v", streams)
	}
	// Appended as the player does, though the link has a query already.
	if got, want := streams[0].Subtitles[0].PlaylistURL,
		"https://cdn.example/sub.vtt?lang=ru?t="+alternativeGuard+"&b="+fingerprint; got != want {
		t.Fatalf("subtitle URL = %q, want %q", got, want)
	}
	headers := map[string]string{}
	for _, header := range streams[0].ProxyHeaders {
		headers[header.Name] = header.Value
		// The router's limits for &h= lines (valid_headers in wrtcast.uc).
		if line := header.Name + ": " + header.Value; len(line) > 512 ||
			!regexp.MustCompile(`^[A-Za-z0-9-]{1,40}: [ -~]+$`).MatchString(line) {
			t.Errorf("proxy header %q breaks the router's limits", line)
		}
	}
	if len(streams[0].ProxyHeaders) > 8 || len(headers) != 4 ||
		headers["Origin"] != "https://player.example" ||
		headers["Referer"] != "https://player.example/?token_movie=abc&token=def" ||
		headers["Accepts-Controls"] != fingerprint ||
		headers["Authorizations"] != "Bearer "+alternativeGuard {
		t.Fatalf("unexpected proxy headers: %+v", streams[0].ProxyHeaders)
	}
	id := strings.Split(strings.TrimSuffix(streams[0].MasterPlaylistURL, "/master.m3u8"), "/")
	master, err := client.MasterPlaylist(context.Background(), id[len(id)-1])
	if err != nil || requests != 1 || preflights != 1 ||
		!strings.Contains(string(master), "https://api.example.com/v1/media/"+id[len(id)-1]+"/quality/720/playlist.m3u8") {
		t.Fatalf("unexpected synthetic master after %d requests: %q", requests, master)
	}
	// The quality's media playlist, its URIs absolute; again within a few
	// seconds without asking the CDN.
	for range 2 {
		playlist, err := client.QualityPlaylist(context.Background(), id[len(id)-1], "720")
		if err != nil || requests != 2 || preflights != 2 ||
			!strings.Contains(string(playlist), `#EXT-X-MAP:URI="`+server.URL+`/720/init.mp4"`) ||
			!strings.Contains(string(playlist), "\n"+server.URL+"/720/seg-1.m4s\n") {
			t.Fatalf("playlist after %d requests: %q, %v", requests, playlist, err)
		}
	}
	// Only the media's hosts go through the relay.
	if _, err := client.FetchMedia(context.Background(), id[len(id)-1], "https://elsewhere.example/seg.m4s"); !errors.Is(err, ErrMediaURL) {
		t.Fatalf("relay of a foreign host: %v", err)
	}
}

// The synthetic master lists every quality: the one loaded with its own
// attributes, the others scaled from it.
func TestScaleAttributes(t *testing.T) {
	t.Parallel()
	got := scaleAttributes(`BANDWIDTH=1000000,AVERAGE-BANDWIDTH=800000,RESOLUTION=854x480,CODECS="avc1.64001f,mp4a.40.2",FRAME-RATE=25.000`, 1.5)
	want := `BANDWIDTH=2250000,AVERAGE-BANDWIDTH=1800000,RESOLUTION=1282x720,CODECS="avc1.64001f,mp4a.40.2",FRAME-RATE=25.000`
	if got != want {
		t.Fatalf("scaleAttributes = %q, want %q", got, want)
	}
}

type memoryFingerprintStore struct {
	value string
	saves int
}

func (s *memoryFingerprintStore) LoadPlayerFingerprint(context.Context) (string, error) {
	return s.value, nil
}

func (s *memoryFingerprintStore) SavePlayerFingerprint(_ context.Context, value string) error {
	s.value = value
	s.saves++
	return nil
}

// The player's fingerprint is made once and outlives the Client when there
// is a store for it.
func TestPlayerFingerprintIsStable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	newClient := func(store PlayerFingerprintStore) *Client {
		client, err := NewClient(Config{BaseURL: "https://example.com", FingerprintStore: store})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	fingerprints := func(client *Client) (string, string) {
		first, err := client.playerFingerprint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		second, err := client.playerFingerprint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return first, second
	}

	first, second := fingerprints(newClient(nil))
	if !playerFingerprintPattern.MatchString(first) || first != second {
		t.Fatalf("fingerprints without a store = %q, %q", first, second)
	}

	// One of another identity (the Chromecast's, before) is another
	// device's.
	store := &memoryFingerprintStore{value: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	first, second = fingerprints(newClient(store))
	if !playerFingerprintPattern.MatchString(first) || first != second || store.value != playerIdentity()+":"+first || store.saves != 1 {
		t.Fatalf("fingerprints = %q, %q; stored %q after %d saves", first, second, store.value, store.saves)
	}
	if again, _ := fingerprints(newClient(store)); again != first || store.saves != 1 {
		t.Fatalf("restarted client fingerprint = %q, want %q (%d saves)", again, first, store.saves)
	}
}

// The page's userParam.domain (kept %-encoded), movie.type and the file's
// id_file are what the player's telemetry sends.
func TestAlternativePlayerTelemetryBootstrap(t *testing.T) {
	t.Parallel()
	page := `<meta name="viewporti" content="nonce">
<script>
	const userParam = {
		token: 'ec4acf58bc9bf62f8be18cad95484c',
		domain: 'https%3A%2F%2Fgo.example.pro%2F',
		device: 0,
		hidden: JSON.parse('[]'),
	};
	const movie = {
		id: '997c32850c0ac731d1a77bcd232556',
		type: 'trailer',
	};
	const fileList = JSON.parse('{"type":"movie","active":{"id":1547557,"id_file":2001,"translation":"SDI Media"},"all":{}}');
</script>`
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(page))
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: "https://example.com", MediaBaseURL: "https://api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	player, err := client.loadAlternativePlayer(context.Background(), server.URL+"/?token_movie=abc&token=def")
	if err != nil {
		t.Fatal(err)
	}
	if player.token != "ec4acf58bc9bf62f8be18cad95484c" || player.domain != "https%3A%2F%2Fgo.example.pro%2F" || !player.trailer ||
		player.catalog.Active.statID() != 2001 || (alternativeFile{ID: 7}).statID() != 7 ||
		player.navigationStart.Before(before) || player.started.Before(player.navigationStart) {
		t.Fatalf("player = %+v", player)
	}
}
