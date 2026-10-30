package provider

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The alternative player reports a playback over a WebSocket when its
// playback reply names one (pnr, pnk): playback_start once the stream is
// ready and on every reconnect, resumed on play and playing every 30
// seconds from then until a pause (paused) or a lost connection, seeked
// after a seek. The server answers with config_update and an edge_hash,
// which from then on replaces the device fingerprint in Accepts-Controls of
// CDN requests. A connection that drops, comes back or can't be had any
// more is told to the site's /stats. title-api holds that socket while the
// router plays the source and hands the router the headers of the moment
// (MediaHeaders). The position and the pauses are the view's (see
// viewSession), so the socket and the telemetry tell the same.

const (
	// How long the router may keep the headers of a media.
	mediaHeadersTTL = 60 * time.Second
	// Until the first edge_hash arrives, so the router picks it up soon.
	pendingHeadersTTL = 5 * time.Second
)

var edgeHashPattern = regexp.MustCompile(`^[0-9A-Za-z._~+/=-]{1,256}$`)

// sessionTiming is the player's, and shorter in tests.
type sessionTiming struct {
	heartbeat      time.Duration // playing while playing
	lease          time.Duration // without a headers request, the socket closes
	connectTimeout time.Duration
	backoffBase    time.Duration // reconnects wait min(base·2^n, max)
	backoffMax     time.Duration
	attempts       int           // reconnects in a row before giving up
	closeWait      time.Duration // for the server's answer to our close
}

var defaultSessionTiming = sessionTiming{
	heartbeat:      30 * time.Second,
	lease:          3 * time.Minute,
	connectTimeout: 10 * time.Second,
	backoffBase:    time.Second,
	backoffMax:     15 * time.Second,
	attempts:       30,
	closeWait:      2 * time.Second,
}

// PlaybackReport is what the router knows of a playback when it asks for
// the headers of its media: the quality it plays ("720", empty when not
// known), the position, whether it is paused or waiting for data
// (BUFFERING), the duration the TV reports once it plays, the bytes of the
// media's segments loaded so far and the bandwidth they came at (bits/s),
// each when known.
type PlaybackReport struct {
	Resolution   string
	Position     float64
	HasPosition  bool
	Paused       bool
	HasPaused    bool
	Buffering    bool
	HasBuffering bool
	Duration     float64
	Bytes        int64
	HasBytes     bool
	Bandwidth    int64
}

// MediaHeaders are the headers CDN requests of a media need now, and how
// long they may be kept; Proxy, when set, is the path of title-api's relay
// of the media's CDN requests (see FetchMedia), which a router that knows
// it goes through instead.
type MediaHeaders struct {
	Headers []ProxyHeader
	TTL     time.Duration
	Proxy   string
	// The sections on the alternative player's video, once it is aligned
	// with the primary's; until then, whether that is still under way
	// (see videoAlignment.state).
	Sections []Section
	Aligning bool
}

// playbackSession is the playback socket of one source. A headers request
// opens it and extends its lease; it closes when the lease runs out, on
// end() and on shutdown, and the next headers request opens it again.
type playbackSession struct {
	client    *Client
	player    *alternativePlayer
	view      *viewSession
	pnr, pnk  string
	trackID   string
	qualities map[string]string

	mu         sync.Mutex
	resolution string
	edgeHash   string
	edgeTTL    time.Duration
	leaseUntil time.Time
	running    bool
	ending     bool // end() asked for ended and a close
	gaveUp     bool // the reconnects ran out, which the player's never retry
	restart    bool // the resolution changed: playback_start is due
	paused     bool // the router's playback is paused
	announced  bool // resumed was sent for the playing of now
	seekDue    bool // seeked is due
	// The run's numbers for /stats: reconnect attempts in a row since the
	// last config_update, reconnects, config_updates and messages, when
	// the connection opened and the last edge_hash came.
	attempts, reconnects, tokens, sent int
	openedAt, tokenAt                  time.Time
	kick                               chan struct{}
	done                               chan struct{} // of the last run
	changed                            chan struct{} // closed on a connection or an edge_hash
}

func newPlaybackSession(client *Client, player *alternativePlayer, view *viewSession, pnr, pnk, trackID string, qualities map[string]string) *playbackSession {
	return &playbackSession{
		client: client, player: player, view: view, pnr: pnr, pnk: pnk, trackID: trackID, qualities: qualities,
		resolution: pickResolution(qualities, ""),
		kick:       make(chan struct{}, 1),
		changed:    make(chan struct{}),
	}
}

// changedLocked wakes those who wait for the socket (see wait).
func (s *playbackSession) changedLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// wait waits up to timeout for ready, which it checks under the lock;
// whether it came.
func (s *playbackSession) wait(ctx context.Context, timeout time.Duration, ready func() bool) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		done, changed := ready(), s.changed
		s.mu.Unlock()
		if done {
			return true
		}
		select {
		case <-changed:
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// pickResolution is the quality key of the source for the one the router
// plays: the same, else the best below it, else the lowest; the best when
// the router did not say.
func pickResolution(qualities map[string]string, want string) string {
	keys := make([]int, 0, len(qualities))
	for key := range qualities {
		if value, err := strconv.Atoi(key); err == nil {
			keys = append(keys, value)
		}
	}
	if len(keys) == 0 {
		return want
	}
	sort.Sort(sort.Reverse(sort.IntSlice(keys)))
	wanted, err := strconv.Atoi(want)
	if err != nil {
		return strconv.Itoa(keys[0])
	}
	for _, key := range keys {
		if key <= wanted {
			return strconv.Itoa(key)
		}
	}
	return strconv.Itoa(keys[len(keys)-1])
}

func (s *playbackSession) signal() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// touch extends the lease, opening the socket if it is closed, takes in
// the router's report and what the view made of it (a pause, a resume, a
// seek), and answers with the headers of the moment.
func (s *playbackSession) touch(report PlaybackReport, change viewChange) MediaHeaders {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.leaseUntil = now.Add(s.client.sessionTiming.lease)
	if change.paused != s.paused || change.seeked {
		s.paused = change.paused
		s.seekDue = s.seekDue || change.seeked
		s.signal()
	}
	if report.Resolution != "" {
		if resolution := pickResolution(s.qualities, report.Resolution); resolution != s.resolution {
			s.resolution, s.restart = resolution, true
			s.signal()
		}
	}
	s.startLocked()
	control, ttl := s.player.fingerprint, mediaHeadersTTL
	switch {
	case s.edgeHash != "":
		control = s.edgeHash
		if s.edgeTTL > 0 && s.edgeTTL/2 < ttl {
			ttl = max(s.edgeTTL/2, pendingHeadersTTL)
		}
	case s.running:
		ttl = pendingHeadersTTL
	}
	return MediaHeaders{Headers: s.player.cdnHeadersWith(control), TTL: ttl}
}

// control is the current Accepts-Controls value: the edge_hash, else the
// fingerprint.
func (s *playbackSession) control() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.edgeHash != "" {
		return s.edgeHash
	}
	return s.player.fingerprint
}

// end sends ended, closes the socket and drops the lease.
func (s *playbackSession) end() {
	s.mu.Lock()
	s.leaseUntil = time.Time{}
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.ending = true
	s.signal()
	done := s.done
	s.mu.Unlock()
	<-done
}

func (s *playbackSession) startLocked() {
	if s.running || s.gaveUp {
		return
	}
	client := s.client
	client.sessionsMu.Lock()
	defer client.sessionsMu.Unlock()
	if client.sessionsClosed {
		return
	}
	client.sessions.Add(1)
	s.running, s.ending, s.restart, s.announced, s.seekDue = true, false, false, false, false
	s.attempts, s.reconnects, s.tokens, s.sent = 0, 0, 0, 0
	s.openedAt, s.tokenAt = time.Time{}, time.Time{}
	previous := s.done
	s.done = make(chan struct{})
	go s.run(client.sessionsCtx, previous, s.done)
}

// over tells whether the run must end and with which close code, and if so
// ends it: on shutdown, on end() or with the lease run out. It decides
// under the lock, so a headers request either finds the run going on or
// starts a new one.
func (s *playbackSession) over(ctx context.Context, lease *time.Timer) (code int, ended, over bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch remaining := time.Until(s.leaseUntil); {
	case ctx.Err() != nil:
		code = wsCloseGoingAway
	case s.ending:
		code, ended = wsCloseNormal, true
	case remaining <= 0:
		code = wsCloseNormal
	default:
		lease.Reset(remaining)
		return 0, false, false
	}
	s.stopLocked()
	return code, ended, true
}

// stopLocked marks the run over. The edge_hash goes with the socket: a
// new one starts with the fingerprint, as the player does before its first
// config_update.
func (s *playbackSession) stopLocked() {
	s.running, s.ending, s.restart = false, false, false
	s.edgeHash, s.edgeTTL = "", 0
	s.changedLocked()
}

// sessionRun is the state of one run, across its reconnects.
type sessionRun struct {
	*playbackSession
	ctx    context.Context
	lease  *time.Timer
	beat   *time.Ticker // from resumed to a pause or a lost connection
	opened bool         // a connection of the run opened before
	failed bool         // a /stats request failed, which is logged once
}

func (s *playbackSession) run(ctx context.Context, previous, done chan struct{}) {
	defer s.client.sessions.Done()
	defer close(done)
	// The socket of the last run is still closing.
	if previous != nil {
		<-previous
	}
	timing := s.client.sessionTiming
	r := &sessionRun{playbackSession: s, ctx: ctx, lease: time.NewTimer(timing.lease)}
	defer r.lease.Stop()
	defer r.stopBeat()
	for {
		if _, _, over := r.over(ctx, r.lease); over {
			return
		}
		conn, err := s.connect(ctx)
		if err == nil {
			s.mu.Lock()
			s.openedAt = time.Now()
			s.changedLocked()
			reconnect := r.opened
			if reconnect {
				s.reconnects++
			}
			s.mu.Unlock()
			r.opened = true
			if reconnect {
				r.stats("reconnected")
			}
			if !r.serve(conn) {
				return
			}
		} else if ctx.Err() == nil {
			log.Printf("playback socket %s: %v", s.pnr, err)
		}
		// As the player's: the attempts count from the last config_update,
		// not from the last connection, which the server may close at once.
		s.mu.Lock()
		attempts := s.attempts
		if attempts < timing.attempts {
			s.attempts++
		}
		s.mu.Unlock()
		if attempts >= timing.attempts {
			log.Printf("playback socket %s: gave up after %d attempts", s.pnr, attempts)
			r.stats("reconnect_failed", [2]string{"error", "max_attempts_30"})
			s.mu.Lock()
			s.gaveUp = true
			s.stopLocked()
			s.mu.Unlock()
			return
		}
		delay := min(timing.backoffBase<<attempts, timing.backoffMax)
		if !r.wait(delay) {
			return
		}
	}
}

func (r *sessionRun) stopBeat() {
	if r.beat != nil {
		r.beat.Stop()
		r.beat = nil
	}
}

// stats tells the site's /stats what became of the socket, as the
// player's beacon does: the page's token and domain, the run's numbers
// and the extra fields.
func (r *sessionRun) stats(status string, extra ...[2]string) {
	now := time.Now()
	seconds := func(since time.Time) string {
		if since.IsZero() {
			return "0"
		}
		return strconv.FormatInt(max(0, int64(math.Floor(now.Sub(since).Seconds()+0.5))), 10)
	}
	r.mu.Lock()
	fields := [][2]string{
		{"token", r.player.token}, {"domain", r.player.domain}, {"ws", "1"}, {"wsf", "1"},
		{"status", status}, {"reconnects", strconv.Itoa(r.reconnects)},
		{"tokens_received", strconv.Itoa(r.tokens)}, {"msgs_sent", strconv.Itoa(r.sent)},
		{"duration", seconds(r.openedAt)}, {"last_token_age", seconds(r.tokenAt)},
	}
	r.mu.Unlock()
	request := telemetryRequest{
		path: "/stats", contentType: "application/x-www-form-urlencoded",
		body: searchParams(append(fields, extra...)), kind: fetchBeacon,
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.client.telemetryTiming.request)
	defer cancel()
	if err := r.player.post(ctx, request); err != nil && !r.failed {
		r.failed = true
		log.Printf("playback socket %s: /stats: %v", r.pnr, err)
	}
}

// wait sleeps before a reconnect; false when the run ended meanwhile.
func (r *sessionRun) wait(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			return true
		case <-r.ctx.Done():
		case <-r.kick:
		case <-r.lease.C:
		}
		if _, _, over := r.over(r.ctx, r.lease); over {
			return false
		}
	}
}

// connect opens the socket as the page does, with the player's origin
// and the cookies Chrome would send.
func (s *playbackSession) connect(ctx context.Context) (*wsConn, error) {
	ctx, cancel := context.WithTimeout(ctx, s.client.sessionTiming.connectTimeout)
	defer cancel()
	rawURL := s.pnr + "?sid=" + encodeURIComponentJS(s.pnk) + "&v=2.1&t=" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	return s.player.browser.dialWebSocket(ctx, rawURL, serializedOrigin(s.player.playerURL))
}

// serve runs one connection: true when it was lost and the run goes on,
// false when the run is over.
func (r *sessionRun) serve(conn *wsConn) bool {
	lost := make(chan error, 1)
	go func() {
		for {
			message, err := conn.readMessage()
			if err != nil {
				lost <- err
				return
			}
			r.receive(message)
		}
	}()
	r.mu.Lock()
	r.restart, r.seekDue = false, false
	r.mu.Unlock()
	err := r.send(conn, "playback_start")
	if err == nil {
		err = r.follow(conn)
	}
	for err == nil {
		var beat <-chan time.Time
		if r.beat != nil {
			beat = r.beat.C
		}
		select {
		case <-beat:
			err = r.send(conn, "playing")
			continue
		case err = <-lost:
			// The player's heartbeat goes with the connection, and the
			// next play event starts it again.
			log.Printf("playback socket %s: %v", r.pnr, err)
			conn.Close()
			r.stopBeat()
			code := 1006
			var closed *wsCloseError
			if errors.As(err, &closed) {
				code = closed.code
			}
			status := "error"
			if code == wsCloseNormal {
				status = "closed"
			}
			r.stats(status, [2]string{"close_code", strconv.Itoa(code)})
			return true
		case <-r.ctx.Done():
		case <-r.kick:
		case <-r.lease.C:
		}
		if code, ended, over := r.over(r.ctx, r.lease); over {
			if ended {
				_ = r.send(conn, "ended")
			}
			_ = conn.writeClose(code)
			select {
			case <-lost:
			case <-time.After(r.client.sessionTiming.closeWait):
				conn.Close()
				<-lost
			}
			conn.Close()
			return false
		}
		r.mu.Lock()
		restart := r.restart
		r.restart = false
		r.mu.Unlock()
		if restart {
			err = r.send(conn, "playback_start")
		}
		if err == nil {
			err = r.follow(conn)
		}
	}
	// A failed write: the connection is gone.
	conn.Close()
	<-lost
	r.stopBeat()
	r.stats("error", [2]string{"close_code", "1006"})
	return true
}

// follow tells the server of a pause, a resume or a seek of the router's
// playback: paused stops the heartbeat, resumed starts it again (as on
// the first play), seeked follows a seek.
func (r *sessionRun) follow(conn *wsConn) error {
	r.mu.Lock()
	paused, announced, seek := r.paused, r.announced, r.seekDue
	r.seekDue = false
	r.announced = !paused
	r.mu.Unlock()
	var err error
	switch {
	case paused && announced:
		r.stopBeat()
		err = r.send(conn, "paused")
	case !paused && !announced:
		if err = r.send(conn, "resumed"); err == nil {
			r.beat = time.NewTicker(r.client.sessionTiming.heartbeat)
		}
	}
	if err == nil && seek {
		err = r.send(conn, "seeked")
	}
	return err
}

type sessionMessage struct {
	Type        string `json:"type"`
	CurrentTime int64  `json:"current_time"`
	Resolution  string `json:"resolution"`
	TrackID     string `json:"track_id"`
	Speed       int    `json:"speed"`
	Subtitle    int    `json:"subtitle"`
	TS          int64  `json:"ts"`
}

func (r *sessionRun) send(conn *wsConn, kind string) error {
	now := time.Now()
	// The view's position: the router's, run on in between.
	position := r.view.position(now)
	r.mu.Lock()
	message := sessionMessage{
		Type: kind, CurrentTime: int64(max(position, 0)), Resolution: r.resolution,
		TrackID: r.trackID, Speed: 1, Subtitle: -1, TS: now.UnixMilli(),
	}
	r.mu.Unlock()
	text, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if err = conn.writeText(text); err == nil {
		r.mu.Lock()
		r.sent++
		r.mu.Unlock()
	}
	return err
}

// receive keeps the edge_hash of a config_update; the player ignores the
// rest of it and every other message.
func (s *playbackSession) receive(message []byte) {
	var update struct {
		Type     string `json:"type"`
		EdgeHash string `json:"edge_hash"`
		TTL      int    `json:"ttl"`
	}
	if json.Unmarshal(message, &update) != nil || update.Type != "config_update" ||
		!edgeHashPattern.MatchString(update.EdgeHash) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.edgeHash = update.EdgeHash
	s.edgeTTL = time.Duration(max(update.TTL, 0)) * time.Second
	s.attempts, s.tokenAt = 0, time.Now()
	s.tokens++
	s.changedLocked()
}

// encodeURIComponentJS is JavaScript's encodeURIComponent, which the
// player builds the socket URL with; unlike encodeURIComponent of the
// challenge cookie it leaves !'()* alone.
func encodeURIComponentJS(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var builder strings.Builder
	for i := 0; i < len(value); i++ {
		character := value[i]
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.IndexByte("-_.!~*'()", character) >= 0 {
			builder.WriteByte(character)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(hexDigits[character>>4])
		builder.WriteByte(hexDigits[character&0x0f])
	}
	return builder.String()
}
