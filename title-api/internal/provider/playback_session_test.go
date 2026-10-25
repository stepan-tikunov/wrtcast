package provider

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sync/atomic"
	"testing"
	"time"
)

const testEdgeHash = "0123456789abcdef0123456789abcdef"

// The player's timings, shortened.
var testSessionTiming = sessionTiming{
	heartbeat:      60 * time.Millisecond,
	lease:          time.Minute,
	connectTimeout: 2 * time.Second,
	backoffBase:    5 * time.Millisecond,
	backoffMax:     20 * time.Millisecond,
	attempts:       3,
	closeWait:      time.Second,
}

// newSessionFixture is a player whose playback reply names a socket on a
// test server, which checks the socket URL and the browser's headers, and
// a source of it listed by the client. accept may refuse connections.
func newSessionFixture(t *testing.T, timing sessionTiming, accept func(*http.Request) int) (*Client, *fakeAlternativePlayer, string, chan *wsTestPeer) {
	t.Helper()
	fake := newFakeAlternativePlayer(t, true, 1, "")
	origin := fake.player.URL
	peers := make(chan *wsTestPeer, 4)
	socket := newWSTestServer(t, func(request *http.Request) int {
		query := request.URL.Query()
		if request.URL.Path != "/ws/" || query.Get("sid") != "pn+k/1=" ||
			!regexp.MustCompile(`^sid=pn%2Bk%2F1%3D&v=2\.1&t=[0-9]{13}$`).MatchString(request.URL.RawQuery) ||
			request.Header.Get("Origin") != origin || request.UserAgent() != defaultUserAgent {
			t.Errorf("socket request %s with %v", request.URL, request.Header)
		}
		if accept != nil {
			return accept(request)
		}
		return http.StatusSwitchingProtocols
	}, peers)
	fake.mu.Lock()
	fake.pnr, fake.pnk = wsTestURL(socket, "/ws/"), "pn+k/1="
	fake.mu.Unlock()
	client, err := NewClient(Config{BaseURL: "https://example.com", MediaBaseURL: "https://api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	client.sessionTiming = timing
	t.Cleanup(client.Close)
	return client, fake, alternativeMasterID(t, client, fake), peers
}

// nextMessage is the next message other than the heartbeat (unless that
// is the one wanted), which must be of type want ("" for a close, whose
// code it returns).
func nextMessage(t *testing.T, peer *wsTestPeer, want string) (map[string]any, int) {
	t.Helper()
	for {
		message, code := peer.readMessage()
		if message != nil && message["type"] == "playing" && want != "playing" {
			continue
		}
		if got, _ := message["type"].(string); got != want {
			t.Fatalf("client sent %v (close %d), want %q", message, code, want)
		}
		return message, code
	}
}

func controlOf(headers []ProxyHeader) string {
	for _, header := range headers {
		if header.Name == "Accepts-Controls" {
			return header.Value
		}
	}
	return ""
}

func TestPlaybackSessionReportsPlayback(t *testing.T) {
	t.Parallel()
	client, fake, id, peers := newSessionFixture(t, testSessionTiming, nil)
	ctx := context.Background()

	// The first headers request resolves the master, which opens the socket.
	headers, err := client.MediaHeaders(ctx, id, PlaybackReport{})
	if _, _, cdnRequests := fake.counts(); err != nil || cdnRequests != 2 ||
		controlOf(headers.Headers) != fake.fingerprint || headers.TTL != pendingHeadersTTL || len(headers.Headers) != 4 {
		t.Fatalf("headers = %+v, %v after %d CDN requests", headers, err, cdnRequests)
	}
	peer := waitPeer(t, peers)
	start, _ := nextMessage(t, peer, "playback_start")
	if start["resolution"] != "720" || start["track_id"] != "2" || start["speed"] != 1.0 ||
		start["subtitle"] != -1.0 || start["current_time"] != 0.0 ||
		time.Since(time.UnixMilli(int64(start["ts"].(float64)))).Abs() > 5*time.Second || len(start) != 7 {
		t.Fatalf("playback_start = %v", start)
	}
	nextMessage(t, peer, "resumed")
	peer.writeFrame(true, wsText, []byte(`{"type":"hello"}`))
	peer.writeFrame(true, wsText, []byte(`{"type":"config_update","edge_hash":"`+testEdgeHash+`","edge_priority":2,"ttl":120,"ts":1}`))
	nextMessage(t, peer, "playing")

	// The edge_hash replaces the fingerprint; another quality and the
	// position are reported.
	report := PlaybackReport{Resolution: "480", Position: 100, HasPosition: true}
	for deadline := time.Now().Add(5 * time.Second); controlOf(headers.Headers) != testEdgeHash; {
		if time.Now().After(deadline) {
			t.Fatalf("headers kept %q", controlOf(headers.Headers))
		}
		time.Sleep(5 * time.Millisecond)
		if headers, err = client.MediaHeaders(ctx, id, report); err != nil {
			t.Fatal(err)
		}
	}
	if headers.TTL != mediaHeadersTTL {
		t.Fatalf("TTL with an edge_hash = %v", headers.TTL)
	}
	start, _ = nextMessage(t, peer, "playback_start")
	if position := start["current_time"].(float64); start["resolution"] != "480" || position < 100 || position > 102 {
		t.Fatalf("playback_start after a quality change = %v", start)
	}
	entry, _ := client.masters.entry(id)
	if control := controlOf(entry.source.cdnHeaders()); control != testEdgeHash {
		t.Fatalf("CDN requests of title-api send %q", control)
	}

	// A connection the server drops is opened again with playback_start,
	// the stale edge_hash in use until a new one comes.
	peer.writeClose(wsCloseGoingAway)
	if _, code := nextMessage(t, peer, ""); code != wsCloseGoingAway {
		t.Fatalf("close answered with %d", code)
	}
	peer.conn.Close()
	peer = waitPeer(t, peers)
	if start, _ = nextMessage(t, peer, "playback_start"); start["resolution"] != "480" {
		t.Fatalf("playback_start on reconnect = %v", start)
	}
	// The heartbeat goes on, with no resumed.
	nextMessage(t, peer, "playing")
	if headers, _ = client.MediaHeaders(ctx, id, report); controlOf(headers.Headers) != testEdgeHash {
		t.Fatalf("reconnect dropped the edge_hash: %+v", headers)
	}

	// The playback ends.
	ended := make(chan struct{})
	go func() {
		client.EndMediaSession(id)
		close(ended)
	}()
	nextMessage(t, peer, "ended")
	if _, code := nextMessage(t, peer, ""); code != wsCloseNormal {
		t.Fatalf("closed with %d", code)
	}
	peer.writeClose(wsCloseNormal)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("EndMediaSession did not return")
	}
	if control := controlOf(entry.source.cdnHeaders()); control != fake.fingerprint {
		t.Fatalf("after the end CDN requests send %q", control)
	}
	client.EndMediaSession(id)
}

func TestPlaybackSessionLeaseAndShutdown(t *testing.T) {
	t.Parallel()
	timing := testSessionTiming
	timing.lease = 300 * time.Millisecond
	client, _, id, peers := newSessionFixture(t, timing, nil)
	ctx := context.Background()

	if _, err := client.MediaHeaders(ctx, id, PlaybackReport{}); err != nil {
		t.Fatal(err)
	}
	peer := waitPeer(t, peers)
	nextMessage(t, peer, "playback_start")
	nextMessage(t, peer, "resumed")
	// No headers request for the lease: a normal close, without ended.
	if _, code := nextMessage(t, peer, ""); code != wsCloseNormal {
		t.Fatalf("closed with %d", code)
	}
	peer.writeClose(wsCloseNormal)

	// The next request opens it again, as a new playback.
	if _, err := client.MediaHeaders(ctx, id, PlaybackReport{Position: 42, HasPosition: true}); err != nil {
		t.Fatal(err)
	}
	peer = waitPeer(t, peers)
	if start, _ := nextMessage(t, peer, "playback_start"); start["current_time"] != 42.0 {
		t.Fatalf("playback_start = %v", start)
	}
	nextMessage(t, peer, "resumed")

	closed := make(chan struct{})
	go func() {
		client.Close()
		close(closed)
	}()
	if _, code := nextMessage(t, peer, ""); code != wsCloseGoingAway {
		t.Fatalf("shutdown closed with %d", code)
	}
	peer.writeClose(wsCloseGoingAway)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	// No socket after shutdown, and the plain headers.
	headers, err := client.MediaHeaders(ctx, id, PlaybackReport{})
	if err != nil || headers.TTL != mediaHeadersTTL {
		t.Fatalf("headers after shutdown = %+v, %v", headers, err)
	}
	select {
	case <-peers:
		t.Fatal("a socket opened after shutdown")
	case <-time.After(50 * time.Millisecond):
	}
}

// A socket that can't be opened is retried with backoff, then given up:
// the headers carry the fingerprint.
func TestPlaybackSessionGivesUp(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int32
	client, fake, id, _ := newSessionFixture(t, testSessionTiming, func(*http.Request) int {
		attempts.Add(1)
		return http.StatusForbidden
	})
	headers, err := client.MediaHeaders(context.Background(), id, PlaybackReport{})
	if err != nil || headers.TTL != pendingHeadersTTL {
		t.Fatalf("headers = %+v, %v", headers, err)
	}
	entry, _ := client.masters.entry(id)
	session := entry.source.session
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		session.mu.Lock()
		running := session.running
		session.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session did not give up")
		}
	}
	if got := attempts.Load(); got != int32(1+testSessionTiming.attempts) {
		t.Fatalf("%d connection attempts", got)
	}
	if control := controlOf(entry.source.cdnHeaders()); control != fake.fingerprint {
		t.Fatalf("Accepts-Controls = %q", control)
	}
}

// Sources whose playback reply names no socket keep the player's headers.
func TestMediaHeadersWithoutSocket(t *testing.T) {
	t.Parallel()
	fake := newFakeAlternativePlayer(t, true, 1, "")
	client, err := NewClient(Config{BaseURL: "https://example.com", MediaBaseURL: "https://api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	id := alternativeMasterID(t, client, fake)
	ctx := context.Background()
	if _, err := client.MediaHeaders(ctx, "unknown", PlaybackReport{}); !errors.Is(err, ErrMasterNotFound) {
		t.Fatalf("unknown media: %v", err)
	}
	headers, err := client.MediaHeaders(ctx, id, PlaybackReport{Resolution: "720"})
	if err != nil || headers.TTL != mediaHeadersTTL || len(headers.Headers) != 4 ||
		controlOf(headers.Headers) != fake.fingerprint {
		t.Fatalf("headers = %+v, %v", headers, err)
	}
	if entry, _ := client.masters.entry(id); entry.source.session != nil {
		t.Fatal("a source without a socket got a session")
	}
	client.EndMediaSession(id)
	client.EndMediaSession("unknown")
}

func TestPickResolution(t *testing.T) {
	t.Parallel()
	qualities := map[string]string{"360": "", "720": "", "1080": "", "auto": ""}
	for want, got := range map[string]string{"": "1080", "720": "720", "900": "720", "2160": "1080", "240": "360"} {
		if resolution := pickResolution(qualities, want); resolution != got {
			t.Errorf("pickResolution(%q) = %q, want %q", want, resolution, got)
		}
	}
	if got := encodeURIComponentJS("a+b/c= d!'()*~é"); got != "a%2Bb%2Fc%3D%20d!'()*~%C3%A9" {
		t.Errorf("encodeURIComponentJS = %q", got)
	}
}
