package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Besides the playback socket the alternative player reports every view to
// its own site, the page's origin:
//
//   - POST /stat (a jQuery form) registers the view on the first play of a
//     file and again whenever the position, rounded, is at 0, 10, 30, 60 or
//     97 percent;
//   - POST /events (fetch, a form with a JSON payload) gets its events in a
//     batch every 30 seconds counted from the player's start, at once after
//     the first frame and before a switch, and by beacon when the page
//     goes: the empty preroll, the startup sequence, a heartbeat every 10
//     seconds while playing (with the bytes loaded), pauses, seeks,
//     switches, milestones and the end.
//
// A viewSession plays that part for the router, one per file of a player
// page, shared by its sources (the dubs, between which the player switches
// as audio). It follows the router's headers requests (see
// alternativeSource.headers): the position, the pause, the duration the TV
// reports once it plays, the bytes it loaded and the quality, and runs on
// in between. The router's playback of a source is the player's playing of
// its file: the first headers request starts the view, the first one with
// the duration is the first frame, the end endpoint is the end of the
// video near its end and the page going away otherwise, and so is the lease
// running out.

const (
	// While a view runs, the router asks for the headers this often, so
	// the position, the pause and the bytes reach it soon.
	activeHeadersTTL = 20 * time.Second
	// A reported position this far off the expected one is a seek.
	seekSeconds = 8
	// The end endpoint this close to the end is the video ending.
	endSeconds = 5
	// hls.js's bandwidth estimate before a fragment loaded
	// (abrEwmaDefaultEstimate) and the player's maxBufferLength.
	defaultBandwidthEstimate = 500000
	maxBufferSeconds         = 60
	// The player keeps this many events and drops the oldest.
	maxBufferedEvents = 1000
)

var viewMilestones = []int{0, 10, 30, 60, 97}

// telemetryTiming is the player's, and shorter in tests.
type telemetryTiming struct {
	heartbeat time.Duration // heartbeat event while playing
	flush     time.Duration // /events batch
	// The preroll whose ad lists came back empty, when the ad network is
	// not asked (tests; see adListURL).
	adBlock time.Duration
	// Without a report from the TV, the first frame is assumed this long
	// after the play once bytes were loaded, and a switch complete after
	// this.
	firstFrame, switchComplete time.Duration
	request                    time.Duration // timeout of one request
}

var defaultTelemetryTiming = telemetryTiming{
	heartbeat:      10 * time.Second,
	flush:          30 * time.Second,
	adBlock:        600 * time.Millisecond,
	firstFrame:     20 * time.Second,
	switchComplete: 10 * time.Second,
	request:        10 * time.Second,
}

// What the player learns of the browser: the desktop Chrome of chrome.go
// with a Full HD screen, the player in an iframe of the provider's page
// (window.inner* and the video element's size), on a wired connection,
// which Chrome tells as 4g, in the time zone of TZ (see browserTimeZone).
const (
	screenWidth, screenHeight = 1920, 1080
	embedWidth, embedHeight   = 854, 480
	devicePixelRatio          = 1
	connectionType            = "4g" // navigator.connection.effectiveType
	downlinkMbps              = 10   // navigator.connection.downlink, which Chrome caps at 10
	connectionRTT             = 50   // navigator.connection.rtt, in steps of 25 ms
	navigatorLanguage         = "ru-RU"
)

// browserZone is Intl's time zone of the browser: TZ when it names one,
// else Moscow's.
type browserZone struct {
	name     string
	location *time.Location
}

func browserTimeZone() browserZone {
	if name := os.Getenv("TZ"); name != "" {
		if location, err := time.LoadLocation(name); err == nil && strings.Contains(name, "/") {
			return browserZone{name: name, location: location}
		}
	}
	location, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		location = time.FixedZone("Europe/Moscow", 3*3600)
	}
	return browserZone{name: "Europe/Moscow", location: location}
}

// offset is -getTimezoneOffset(): minutes east of UTC at at.
func (zone browserZone) offset(at time.Time) int {
	_, seconds := at.In(zone.location).Zone()
	return seconds / 60
}

var platformPattern = regexp.MustCompile(`\b(Linux [A-Za-z0-9_]+|Windows|Macintosh)\b`)

// navigatorPlatform is navigator.platform of a browser with userAgent.
func navigatorPlatform(userAgent string) string {
	switch platform := platformPattern.FindString(userAgent); platform {
	case "Windows":
		return "Win32"
	case "Macintosh":
		return "MacIntel"
	case "":
		return "Linux armv7l"
	default:
		return platform
	}
}

// viewChange is what a headers request made of the view, which the
// playback socket follows: the pause as it is now, and whether the request
// is a seek.
type viewChange struct {
	paused, seeked bool
}

// telemetryRequest is one POST to the player's origin: a jQuery form
// (fetchXHR), a fetch() body (fetchFetch) or a beacon (fetchBeacon).
type telemetryRequest struct {
	path, contentType, body string
	kind                    fetchKind
}

type viewSession struct {
	client *Client
	player *alternativePlayer
	statID int64
	timing telemetryTiming
	// performance.now() counts from navigationStart; the player started at
	// playerInitAt. The page's first playback request (a file change of
	// type "initial") went out and was answered at backendSent,
	// backendAnswered.
	navigationStart, playerInitAt time.Time
	backendSent, backendAnswered  time.Time

	mu                   sync.Mutex
	sessionID, requestID string
	// The source the router plays, its quality and what the player knows
	// of it.
	source      *alternativeSource
	label       string
	quality     string
	manifestURL string
	variant     variantInfo
	// When the master of the first quality was asked for and read, for
	// the first view; zero after.
	manifestRequested, manifestLoaded time.Time
	fileChangeStarted                 time.Time
	prerolled                         bool

	head playhead

	// The view, as the player's telemetry state.
	playAttempted, loaded, viewStarted, viewEverStarted bool
	statViewSent                                        bool
	playReadyAt, firstFrameAt, adStartedAt              time.Time
	adDuration                                          time.Duration
	paused                                              bool
	pauseStartedAt                                      time.Time
	playedSeconds, lastHeartbeatPlayed                  float64
	evaluated                                           float64 // the position progress last counted from
	lastHeartbeatBytes                                  int64
	percents                                            map[int]bool
	statPercent                                         int
	pendingSwitch                                       string
	switchStartedAt                                     time.Time
	events                                              []jsObject
	dropped                                             int
	outbox                                              []telemetryRequest
	// The media element's waiting and playing, and the rebuffers of the
	// view so far.
	isBuffering     bool
	rebufferStarted time.Time
	rebuffers       int
	rebufferTotal   int64 // milliseconds
	// Ad lists due (see adCall), whether the preroll waits for its own,
	// and the midroll slots done this view.
	adCalls                      []adCall
	prerollPending               bool
	midrollStarted, midrollAt300 bool
	// /errors: the hls.js errors buffered, their session, and the last
	// one's signature, which a repeat of is not buffered.
	errors                           []jsObject
	errorsDropped                    int
	errorsSessionID, errorsRequestID string
	lastErrorSignature               string
	lastErrorAt                      time.Time
	nextErrorsFlush, nextReload      time.Time
	reloadDue                        bool

	// The run: from the router's first headers request to the end or the
	// lease running out.
	running, ending                   bool
	leaseUntil                        time.Time
	nextHeartbeat, nextFlush          time.Time
	playDue, firstFrameDue, switchDue time.Time
	kick                              chan struct{}
	done                              chan struct{}
}

func newViewSession(client *Client, player *alternativePlayer, statID int64) *viewSession {
	now := time.Now()
	player.mu.Lock()
	backendSent, backendAnswered := player.playbackSent, player.playbackAnswered
	player.mu.Unlock()
	view := &viewSession{
		client: client, player: player, statID: statID, timing: client.telemetryTiming,
		navigationStart: player.navigationStart, playerInitAt: player.started,
		backendSent: backendSent, backendAnswered: backendAnswered,
		sessionID: randomID(), requestID: randomID(),
		errorsSessionID: randomID(), errorsRequestID: randomID(),
		fileChangeStarted: backendSent,
		statPercent:       -1,
		percents:          map[int]bool{},
		kick:              make(chan struct{}, 1),
	}
	if view.playerInitAt.IsZero() {
		view.playerInitAt = now
	}
	if view.navigationStart.IsZero() {
		view.navigationStart = view.playerInitAt
	}
	view.head.sourceBytes = map[*alternativeSource]int64{}
	return view
}

// randomID is the player's client session and request id: a random UUID
// without the dashes.
func randomID() string {
	random := make([]byte, 16)
	_, _ = rand.Read(random)
	random[6] = random[6]&0x0f | 0x40
	random[8] = random[8]&0x3f | 0x80
	return hex.EncodeToString(random)
}

func (v *viewSession) signal() {
	select {
	case v.kick <- struct{}{}:
	default:
	}
}

// position is the router's position, run on while it plays, for the
// playback socket.
func (v *viewSession) position(now time.Time) float64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.head.at(now)
}

// current is the pause as it is now, for a socket that opens.
func (v *viewSession) current() viewChange {
	v.mu.Lock()
	defer v.mu.Unlock()
	return viewChange{paused: v.head.paused}
}

// touch takes in a headers request of the router for source: it extends
// the lease, starting the view if it is not running, and follows the
// report. It tells what changed, and whether the view runs.
func (v *viewSession) touch(now time.Time, source *alternativeSource, report PlaybackReport) (viewChange, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.leaseUntil = now.Add(v.client.sessionTiming.lease)
	if !v.running && !v.startLocked(now, source, report) {
		return viewChange{paused: v.head.paused}, false
	}
	change := v.reportLocked(now, source, report)
	v.signal()
	return change, true
}

// end ends the view if source is the one playing, as the end of the video
// or the page going away, and waits for its last requests.
func (v *viewSession) end(source *alternativeSource) {
	v.mu.Lock()
	if !v.running || v.source != source {
		v.mu.Unlock()
		return
	}
	v.ending = true
	v.signal()
	done := v.done
	v.mu.Unlock()
	<-done
}

func (v *viewSession) startLocked(now time.Time, source *alternativeSource, report PlaybackReport) bool {
	client := v.client
	client.sessionsMu.Lock()
	defer client.sessionsMu.Unlock()
	if client.sessionsClosed {
		return false
	}
	client.sessions.Add(1)
	v.beginLocked(now, source, report)
	previous := v.done
	v.done = make(chan struct{})
	go v.run(client.sessionsCtx, previous, v.done)
	return true
}

// beginLocked starts a view: the player's timers run from its start, and
// the play button is pressed.
func (v *viewSession) beginLocked(now time.Time, source *alternativeSource, report PlaybackReport) {
	v.running, v.ending = true, false
	v.nextHeartbeat = nextTick(v.playerInitAt, v.timing.heartbeat, now)
	v.nextFlush = nextTick(v.playerInitAt, v.timing.flush, now)
	v.nextErrorsFlush = v.nextFlush
	v.nextReload = nextTick(v.playerInitAt, v.client.pageTiming.reload, now)
	v.resetLocked(now)
	v.setSourceLocked(now, source, report.Resolution)
	if !v.head.started.IsZero() {
		// A view after one that ended: no file change, no manifest.
		v.fileChangeStarted, v.manifestRequested, v.manifestLoaded = time.Time{}, time.Time{}, time.Time{}
	}
	v.head.started = now
	if !v.prerolled {
		v.beginPrerollLocked(now)
	} else {
		v.playDue = now
	}
}

// resetLocked starts a view anew, as the player does after the end of a
// video or a file change.
func (v *viewSession) resetLocked(now time.Time) {
	v.playAttempted, v.loaded, v.viewStarted, v.viewEverStarted = false, false, false, false
	v.playReadyAt, v.firstFrameAt, v.adStartedAt = time.Time{}, time.Time{}, time.Time{}
	v.adDuration = 0
	v.paused, v.pauseStartedAt = true, time.Time{}
	v.playedSeconds, v.lastHeartbeatPlayed, v.evaluated = 0, 0, 0
	v.percents = map[int]bool{}
	v.pendingSwitch, v.switchStartedAt = "", time.Time{}
	v.playDue, v.firstFrameDue, v.switchDue = time.Time{}, time.Time{}, time.Time{}
	v.head.resetBytes()
	v.lastHeartbeatBytes = 0
	v.isBuffering, v.rebufferStarted, v.rebuffers, v.rebufferTotal = false, time.Time{}, 0, 0
	v.midrollStarted, v.midrollAt300, v.prerollPending = false, false, false
}

// setSourceLocked makes source and the quality the router plays (want, or
// the one played so far) the view's: an audio switch or a quality switch
// once the view has played, else what it starts with.
func (v *viewSession) setSourceLocked(now time.Time, source *alternativeSource, want string) {
	source.mu.Lock()
	qualities, variants := source.qualities, source.variants
	source.mu.Unlock()
	if want == "" {
		want = v.quality
	}
	quality := pickResolution(qualities, want)
	switching := v.playAttempted && v.playDue.IsZero()
	if v.source != nil && v.source != source && switching {
		v.switchLocked(now, "audio", jsObject{
			{"event_type", "audio_switch"}, {"audioBefore", v.label}, {"audioAfter", source.label},
		})
	}
	if v.quality != "" && v.quality != quality && switching {
		before, _ := strconv.Atoi(v.quality)
		after, _ := strconv.Atoi(quality)
		v.switchLocked(now, "quality", jsObject{
			{"event_type", "quality_switch"}, {"qualityBefore", jsQuality(v.quality)},
			{"qualityAfter", jsQuality(quality)}, {"isUpscale", trueOrNull(after > before)},
			{"isDownscale", trueOrNull(after < before)}, {"bandwidthEstimate", v.bandwidthLocked()},
		})
	}
	first := v.source == nil
	v.source, v.label, v.quality = source, source.label, quality
	v.manifestURL = strings.TrimSpace(strings.Split(qualities[quality], " or ")[0])
	v.variant = variants[quality]
	if first {
		v.manifestRequested, v.manifestLoaded = v.variant.requested, v.variant.loaded
	}
}

// switchLocked is the player's quality or audio change: the batches so far
// go, the request ids are new, and the switch completes on the next play.
func (v *viewSession) switchLocked(now time.Time, kind string, event jsObject) {
	v.flushLocked(now, false)
	v.flushErrorsLocked(now, false)
	v.requestID, v.errorsRequestID = randomID(), randomID()
	v.pendingSwitch, v.switchStartedAt = kind, now
	v.switchDue = now.Add(v.timing.switchComplete)
	v.pushLocked(now, event)
}

// beginPrerollLocked is the play button: the preroll starts and asks the
// ad network for its lists, which ends it (see playLocked) once they are
// in, or without the ad network a moment later.
func (v *viewSession) beginPrerollLocked(now time.Time) {
	v.prerolled = true
	v.adStartedAt = now
	v.pushLocked(now, jsObject{{"event_type", "ad_block_start"}})
	if calls := v.player.adSlot(slotPreroll); len(calls) > 0 {
		v.adCalls = append(v.adCalls, calls...)
		v.prerollPending = true
		return
	}
	v.playDue = now.Add(v.timing.adBlock)
}

// playLocked ends the empty preroll, if any, and plays: view_start, startup
// and play, and the view is registered with /stat.
func (v *viewSession) playLocked(now time.Time) {
	v.playDue = time.Time{}
	if !v.adStartedAt.IsZero() {
		block := now.Sub(v.adStartedAt)
		v.adDuration += block
		v.adStartedAt = time.Time{}
		v.pushLocked(now, jsObject{
			{"event_type", "ad_block_end"}, {"adDurationMs", block.Milliseconds()},
			{"adDurationCumulativeMs", v.adDuration.Milliseconds()},
		})
	}
	v.playAttempted, v.playReadyAt, v.paused = true, now, false
	bitrate, frameRate := v.levelLocked()
	v.pushLocked(now, jsObject{
		{"event_type", "view_start"}, {"fileChangeType", "initial"}, {"autoplay", false},
		{"resumeFrom", 0}, {"startupQuality", jsQuality(v.quality)}, {"startupAudio", v.label},
		{"startupBitrate", bitrate}, {"startupFrameRate", frameRate},
		{"renderWidth", embedWidth}, {"renderHeight", embedHeight},
	})
	var manifestLoad any
	if !v.manifestRequested.IsZero() && !v.manifestLoaded.IsZero() {
		manifestLoad = milliseconds(v.manifestLoaded.Sub(v.manifestRequested))
	}
	var userWait int64
	if !v.manifestLoaded.IsZero() && now.After(v.manifestLoaded) {
		userWait = milliseconds(now.Sub(v.manifestLoaded))
	}
	v.pushLocked(now, jsObject{
		{"event_type", "startup"}, {"fileChangeType", "initial"},
		{"playerStartupMs", milliseconds(now.Sub(v.playerInitAt))}, {"manifestLoadMs", manifestLoad},
		{"userWaitMs", userWait}, {"bandwidthEstimate", v.bandwidthLocked()}, {"startupBitrate", bitrate},
	})
	v.pushLocked(now, jsObject{{"event_type", "play"}})
	v.firstFrameDue = now.Add(v.timing.firstFrame)
	if !v.statViewSent {
		v.statViewSent = true
		v.statLocked(nil)
	}
}

// reportLocked follows a headers request: the source and quality, the
// bytes, the duration, a seek, a pause or a resume, the first frame and
// the end of a switch.
func (v *viewSession) reportLocked(now time.Time, source *alternativeSource, report PlaybackReport) viewChange {
	change := viewChange{}
	v.progressLocked(now)
	v.setSourceLocked(now, source, report.Resolution)
	if report.HasBytes {
		v.head.addBytes(now, source, report.Bytes)
	}
	if report.Bandwidth > 0 {
		v.head.bandwidth = report.Bandwidth
	}
	if report.Duration > 0 {
		v.head.duration = report.Duration
	}
	if report.HasPosition {
		expected := v.head.at(now)
		v.head.set(now, report.Position)
		if v.loaded && math.Abs(report.Position-expected) > seekSeconds {
			v.seekLocked(now)
			change.seeked = true
		}
		v.evaluated = v.head.at(now)
	}
	if report.HasPaused && report.Paused != v.head.paused {
		v.head.pause(now, report.Paused)
		// The media element's pause and play events, which come with a
		// change only.
		if v.playAttempted && v.playDue.IsZero() && report.Paused != v.paused {
			switch {
			case report.Paused:
				v.paused, v.pauseStartedAt = true, now
				v.pushLocked(now, jsObject{{"event_type", "pause"}})
			case !v.pauseStartedAt.IsZero():
				pauseDuration := milliseconds(now.Sub(v.pauseStartedAt))
				v.paused, v.pauseStartedAt = false, time.Time{}
				v.pushLocked(now, jsObject{{"event_type", "play"}, {"pauseDurationMs", pauseDuration}})
			default:
				v.paused = false
				v.pushLocked(now, jsObject{{"event_type", "play"}})
			}
		}
	}
	change.paused = v.head.paused
	if report.HasBuffering {
		v.bufferingLocked(now, report.Buffering && !v.head.paused)
	}
	if v.playAttempted && v.playDue.IsZero() && !v.viewStarted && report.Duration > 0 {
		change.seeked = v.firstFrameLocked(now) || change.seeked
	} else if v.pendingSwitch != "" && now.Sub(v.switchStartedAt) >= time.Second {
		v.switchCompleteLocked(now)
	}
	v.progressLocked(now)
	return change
}

// bufferingLocked follows the receiver waiting for data: the media
// element's waiting is rebuffer_start once the view started, and its
// playing ends it with rebuffer_end and the view's count and time.
func (v *viewSession) bufferingLocked(now time.Time, waiting bool) {
	switch {
	case waiting && !v.isBuffering && v.viewStarted && v.pendingSwitch == "":
		v.isBuffering, v.rebufferStarted = true, now
		v.pushLocked(now, jsObject{
			{"event_type", "rebuffer_start"}, {"bandwidthEstimate", v.bandwidthLocked()}, {"bufferLengthAtStart", v.bufferLocked(now)},
		})
	case !waiting && v.isBuffering && v.pendingSwitch == "":
		duration := milliseconds(now.Sub(v.rebufferStarted))
		v.isBuffering = false
		v.rebuffers++
		v.rebufferTotal += duration
		v.pushLocked(now, jsObject{
			{"event_type", "rebuffer_end"}, {"rebufferDurationMs", duration}, {"rebufferCountTotal", v.rebuffers},
			{"rebufferTotalMsSession", v.rebufferTotal}, {"bandwidthEstimate", v.bandwidthLocked()},
		})
	}
}

// seekLocked is a seek of the receiver. The player takes seekFrom when the
// seek has begun, so it is the target too.
func (v *viewSession) seekLocked(now time.Time) {
	target := round3(v.head.at(now))
	v.pushLocked(now, jsObject{{"event_type", "seek_start"}, {"seekFrom", target}})
	v.pushLocked(now, jsObject{
		{"event_type", "seek_end"}, {"seekFrom", target}, {"seekTo", target}, {"seekDistance", 0},
	})
}

// firstFrameLocked is the TV playing: the player seeks to where it
// resumes, the first fragment is buffered and the first frame shows. It
// tells whether there was a seek.
func (v *viewSession) firstFrameLocked(now time.Time) bool {
	v.loaded = true
	v.firstFrameDue = time.Time{}
	// Played from the start unless the router said where.
	if v.head.reported.IsZero() {
		v.head.set(now, 0)
	}
	seeked := v.head.at(now) > 1
	if seeked {
		v.seekLocked(now)
	}
	v.pushLocked(now, jsObject{
		{"event_type", "playback_ready"}, {"timeToPlaybackReadyMs", milliseconds(now.Sub(v.playReadyAt))},
		{"bandwidthEstimate", v.bandwidthLocked()},
	})
	v.firstFrameAt = now
	v.viewStarted, v.viewEverStarted = true, true
	v.midrollLocked(now)
	var adDuration any
	if v.adDuration > 0 {
		adDuration = v.adDuration.Milliseconds()
	}
	playToFirstFrame := max(0, milliseconds(now.Sub(v.playReadyAt)))
	v.pushLocked(now, jsObject{
		{"event_type", "first_frame"}, {"timeToFirstFrameMs", playToFirstFrame},
		{"adDurationMs", adDuration}, {"viewStartSource", "playing"},
	})
	if !v.fileChangeStarted.IsZero() {
		var backend, manifestLoad, userWait any
		var wait int64
		if !v.backendSent.IsZero() && !v.backendAnswered.IsZero() {
			backend = milliseconds(v.backendAnswered.Sub(v.backendSent))
		}
		if !v.manifestRequested.IsZero() && !v.manifestLoaded.IsZero() {
			manifestLoad = milliseconds(v.manifestLoaded.Sub(v.manifestRequested))
		}
		if !v.manifestLoaded.IsZero() {
			wait = max(0, milliseconds(v.playReadyAt.Sub(v.manifestLoaded)))
			userWait = wait
		}
		v.pushLocked(now, jsObject{
			{"event_type", "file_load_complete"}, {"fileChangeType", "initial"}, {"viewStartSource", "playing"},
			{"totalFileChangeMs", max(0, milliseconds(now.Sub(v.fileChangeStarted))-wait)},
			{"backendMs", backend}, {"manifestLoadMs", manifestLoad}, {"userWaitMs", userWait},
			{"playToFirstFrameMs", milliseconds(now.Sub(v.playReadyAt))}, {"adDurationMs", adDuration},
			{"segmentLoadMs", max(0, milliseconds(now.Sub(v.playReadyAt))-v.adDuration.Milliseconds())},
			{"autoplay", false},
		})
		v.fileChangeStarted = time.Time{}
	}
	v.flushLocked(now, false)
	v.evaluated = v.head.at(now)
	return seeked
}

// switchCompleteLocked is the first frame after a switch, which loaded
// the new quality's master (see QualityPlaylist), unless it was loaded
// before.
func (v *viewSession) switchCompleteLocked(now time.Time) {
	switchMs := milliseconds(now.Sub(v.switchStartedAt))
	var manifestLoad, segmentLoad any
	if info := v.variant; !info.requested.IsZero() && !info.requested.Before(v.switchStartedAt) && info.loaded.After(info.requested) {
		load := milliseconds(info.loaded.Sub(info.requested))
		manifestLoad = load
		if switchMs > load {
			segmentLoad = switchMs - load
		}
	}
	v.pushLocked(now, jsObject{
		{"event_type", "switch_complete"}, {"switchType", v.pendingSwitch},
		{"switchToFirstFrameMs", switchMs},
		{"manifestLoadMs", manifestLoad}, {"segmentLoadMs", segmentLoad}, {"bandwidthEstimate", v.bandwidthLocked()},
	})
	v.pendingSwitch, v.switchStartedAt, v.switchDue = "", time.Time{}, time.Time{}
}

// progressLocked counts the time played and reaches the milestones, as the
// player's timeupdate does: view_percent of every milestone passed, once a
// view, and /stat whenever the rounded percent is one.
func (v *viewSession) progressLocked(now time.Time) {
	if !v.viewStarted {
		return
	}
	position := v.head.at(now)
	if delta := position - v.evaluated; delta > 0 && !v.paused {
		v.playedSeconds += delta
	}
	v.evaluated = position
	if position >= midrollTime {
		v.midrollLocked(now)
	}
	duration := v.head.duration
	if duration <= 0 {
		return
	}
	percent := position / duration * 100
	for _, milestone := range viewMilestones {
		if math.Floor(percent) >= float64(milestone) && !v.percents[milestone] {
			v.percents[milestone] = true
			v.pushLocked(now, jsObject{
				{"event_type", "view_percent"}, {"percent", milestone},
				{"watchTimeSec", int64(math.Floor(v.playedSeconds + 0.5))},
			})
		}
	}
	rounded := int(math.Floor(percent + 0.5))
	for _, milestone := range viewMilestones {
		if rounded == milestone && v.statPercent != rounded {
			v.statPercent = rounded
			v.statLocked(&rounded)
		}
	}
}

// nextMilestoneLocked is when the playing position reaches the next
// milestone of view_percent or /stat; zero without one.
func (v *viewSession) nextMilestoneLocked(now time.Time) time.Time {
	duration := v.head.duration
	if !v.viewStarted || v.paused || v.head.paused || duration <= 0 {
		return time.Time{}
	}
	position := v.head.at(now)
	next := math.Inf(1)
	for _, milestone := range viewMilestones {
		for _, at := range []float64{float64(milestone), float64(milestone) - 0.5} {
			target := at / 100 * duration
			sent := at == float64(milestone) && v.percents[milestone] || at != float64(milestone) && v.statPercent == milestone
			if target > position && !sent {
				next = min(next, target-position)
			}
		}
	}
	if math.IsInf(next, 1) {
		return time.Time{}
	}
	return now.Add(time.Duration(next*float64(time.Second)) + time.Millisecond)
}

// heartbeatLocked is the heartbeat timer: an event while the view plays.
func (v *viewSession) heartbeatLocked(now time.Time) {
	if !v.viewStarted || v.paused {
		return
	}
	v.progressLocked(now)
	played := v.playedSeconds - v.lastHeartbeatPlayed
	v.lastHeartbeatPlayed = v.playedSeconds
	bitrate, frameRate := v.levelLocked()
	var width, height, upscaling any
	if v.variant.width > 0 && v.variant.height > 0 {
		width, height, upscaling = v.variant.width, v.variant.height, embedWidth > v.variant.width
	}
	// The bundle counts the bytes of FRAG_LOADED's payload, which hls.js
	// has handed to its worker (and so emptied) by then: none, in a
	// desktop Chrome.
	v.pushLocked(now, jsObject{
		{"event_type", "heartbeat"}, {"bandwidthEstimate", v.bandwidthLocked()},
		{"droppedFrames", 0}, {"droppedFramesDelta", 0}, {"playedSecondsDelta", round3(played)},
		{"isBuffering", v.isBuffering}, {"isSeeking", false}, {"playbackRate", 1},
		{"currentBitrate", bitrate}, {"currentFrameRate", frameRate},
		{"videoWidth", width}, {"videoHeight", height}, {"isUpscaling", upscaling},
		{"rebufferCountSession", v.rebuffers}, {"rebufferTotalMsSession", v.rebufferTotal},
		{"bytesLoadedTotal", 0}, {"bytesLoadedDelta", 0},
	})
}

// finishLocked ends the view: at the end of the video the media element
// pauses and ends, view_finish goes at once, and the next play has its
// preroll again; a page going away sends what is left by beacon when it is
// hidden, and on pagehide tells of a view that never started.
func (v *viewSession) finishLocked(now time.Time, ended bool) {
	v.progressLocked(now)
	if ended {
		if !v.paused {
			v.paused = true
			v.pushLocked(now, jsObject{{"event_type", "pause"}})
		}
		v.pushLocked(now, jsObject{
			{"event_type", "view_finish"}, {"watchTimeSec", int64(math.Floor(v.playedSeconds + 0.5))},
			{"completed", true},
		})
		v.flushLocked(now, false)
		v.resetLocked(now)
		v.prerolled = false
		return
	}
	v.flushLocked(now, true)
	v.flushErrorsLocked(now, true)
	if v.playAttempted && !v.viewEverStarted {
		v.pushLocked(now, jsObject{
			{"event_type", "exit_before_start"}, {"timeToExitMs", milliseconds(now.Sub(v.playReadyAt))},
		})
		v.flushLocked(now, true)
	}
}

// variantLocked takes in the variant of the quality played once its
// master loaded (see QualityPlaylist), and the manifest's times for a view
// that has not played yet.
func (v *viewSession) variantLocked() {
	if v.source == nil {
		return
	}
	v.source.mu.Lock()
	info, ok := v.source.variants[v.quality]
	v.source.mu.Unlock()
	if !ok {
		return
	}
	v.variant = info
	if !v.playAttempted && v.manifestLoaded.IsZero() && !v.fileChangeStarted.IsZero() {
		v.manifestRequested, v.manifestLoaded = info.requested, info.loaded
	}
}

// levelLocked is the bitrate and the frame rate of the level played, as
// hls.js has them from the variant.
func (v *viewSession) levelLocked() (bitrate, frameRate any) {
	v.variantLocked()
	if v.variant.bandwidth > 0 {
		bitrate = v.variant.bandwidth
	}
	if v.variant.frameRate > 0 {
		frameRate = v.variant.frameRate
	}
	return bitrate, frameRate
}

// bandwidthLocked is hls.js's estimate: from the fragments title-api
// relayed (see mediaState), else what the router measured, else the
// default before a fragment loaded.
func (v *viewSession) bandwidthLocked() any {
	if v.source != nil {
		if estimate, _, _ := v.source.media.stats(); estimate > 0 {
			return estimate
		}
	}
	if v.head.bandwidth > 0 {
		return v.head.bandwidth
	}
	return defaultBandwidthEstimate
}

// bufferLocked is the length buffered ahead: up to the end of the last
// fragment title-api relayed, else the seconds the bytes the router loaded
// stand for at the variant's bitrate, less those played.
func (v *viewSession) bufferLocked(now time.Time) any {
	if v.source != nil {
		if _, loadedEnd, _ := v.source.media.stats(); loadedEnd > 0 {
			return round3(min(max(loadedEnd-v.head.at(now), 0), maxBufferSeconds))
		}
	}
	bitrate := v.variant.averageBandwidth
	if bitrate <= 0 {
		bitrate = v.variant.bandwidth
	}
	if bitrate <= 0 {
		return nil
	}
	seconds := float64(v.head.loaded(now)) * 8 / float64(bitrate)
	return round3(min(max(seconds-v.playedSeconds, 0), maxBufferSeconds))
}

// pushLocked adds an event with the fields every event has: what the
// media element and the player show.
func (v *viewSession) pushLocked(now time.Time, fields jsObject) {
	if len(v.events) >= maxBufferedEvents {
		v.events = v.events[1:]
		v.dropped++
	}
	var currentTime, duration, buffer any = 0, nil, 0
	if v.loaded {
		currentTime = round6(v.head.at(now))
		if v.head.duration > 0 {
			duration = v.head.duration
		}
		buffer = v.bufferLocked(now)
	}
	var manifest any
	if v.manifestURL != "" {
		manifest = v.manifestURL
	}
	event := jsObject{
		{"url", nil}, {"manifestUrl", manifest}, {"currentTime", currentTime}, {"duration", duration},
		{"bufferLength", buffer}, {"paused", !v.playAttempted || v.paused || !v.playDue.IsZero()},
		{"audioTrack", v.label}, {"quality", jsQuality(v.quality)}, {"subtitle", nil},
	}
	event = append(event, fields...)
	event = append(event, jsField{"at", now.UnixMilli()}, jsField{"since", milliseconds(now.Sub(v.navigationStart))})
	v.events = append(v.events, event)
}

// flushLocked sends the events so far, if any: by fetch, or by beacon as
// the page goes.
func (v *viewSession) flushLocked(now time.Time, beacon bool) {
	if len(v.events) == 0 {
		return
	}
	payload := appendJSValue(nil, jsObject{
		{"token", v.player.token}, {"domain", v.player.domain}, {"isTrailer", v.player.trailer},
		{"dropped", v.dropped}, {"playerInitAt", v.playerInitAt.UnixMilli()},
		{"clientSessionId", v.sessionID}, {"clientRequestId", v.requestID},
		{"env", jsObject{
			{"connectionType", connectionType}, {"downlink", downlinkMbps}, {"pixelRatio", devicePixelRatio},
			{"screenW", screenWidth}, {"screenH", screenHeight},
		}},
		{"events", v.events},
	})
	v.events, v.dropped = nil, 0
	v.outbox = append(v.outbox, pagePost("/events", v.player.token, string(payload), beacon))
}

// statLocked registers the view with /stat, or a milestone with percent.
func (v *viewSession) statLocked(percent *int) {
	fields := [][2]string{
		{"id", strconv.FormatInt(v.statID, 10)}, {"token", v.player.token}, {"domain", v.player.domain},
		{"url", v.player.playerURL.String()}, {"type", v.viewType()}, {"ab", "false"},
	}
	if percent != nil {
		fields = append(fields, [2]string{"percent", strconv.Itoa(*percent)})
	}
	fields = append(fields,
		[2]string{"info[wasm]", "true"}, [2]string{"info[sw]", "false"},
		[2]string{"info[resolution][screenWidth]", strconv.Itoa(screenWidth)},
		[2]string{"info[resolution][screenHeight]", strconv.Itoa(screenHeight)},
		[2]string{"info[resolution][windowWidth]", strconv.Itoa(embedWidth)},
		[2]string{"info[resolution][windowHeight]", strconv.Itoa(embedHeight)},
		[2]string{"info[resolution][devicePixelRatio]", strconv.Itoa(devicePixelRatio)},
		[2]string{"info[webSocket]", "true"}, [2]string{"info[platform]", navigatorPlatform(chromeUserAgent)},
	)
	v.outbox = append(v.outbox, telemetryRequest{
		path: "/stat", contentType: "application/x-www-form-urlencoded; charset=UTF-8",
		body: jqueryParam(fields), kind: fetchXHR,
	})
}

// viewType is /stat's type: t or m for a trailer or not, and the Web
// Crypto algorithms that work, AES-GCM, RSA-PSS and RSA-OAEP.
func (v *viewSession) viewType() string {
	if v.player.trailer {
		return "tgpo"
	}
	return "mgpo"
}

// stepLocked runs what is due at now; true when the run is over: on
// shutdown, on end() and with the lease run out.
func (v *viewSession) stepLocked(ctx context.Context, now time.Time) bool {
	switch {
	case v.ending:
		v.finishLocked(now, v.head.duration > 0 && v.head.at(now) >= v.head.duration-endSeconds)
	case ctx.Err() != nil || !v.leaseUntil.After(now):
		v.finishLocked(now, false)
	default:
		v.dueLocked(now)
		return false
	}
	v.running, v.ending = false, false
	return true
}

// dueLocked runs the timers that are due, in the order the player's would
// run: the flush before the heartbeat of the same moment.
func (v *viewSession) dueLocked(now time.Time) {
	if !v.nextFlush.After(now) {
		v.flushLocked(now, false)
		v.nextFlush = nextTick(v.playerInitAt, v.timing.flush, now)
	}
	if !v.nextErrorsFlush.After(now) {
		v.flushErrorsLocked(now, false)
		v.nextErrorsFlush = nextTick(v.playerInitAt, v.timing.flush, now)
	}
	if !v.nextReload.After(now) {
		v.reloadDue = true
		v.nextReload = nextTick(v.playerInitAt, v.client.pageTiming.reload, now)
	}
	if !v.nextHeartbeat.After(now) {
		v.heartbeatLocked(now)
		v.nextHeartbeat = nextTick(v.playerInitAt, v.timing.heartbeat, now)
	}
	if !v.playDue.IsZero() && !v.playDue.After(now) {
		v.playLocked(now)
	}
	// Without the duration from the TV, the bytes it loaded tell it plays.
	if !v.firstFrameDue.IsZero() && !v.firstFrameDue.After(now) {
		v.firstFrameDue = now.Add(v.timing.firstFrame)
		if _, _, loads := v.source.media.stats(); v.head.total > 0 || loads > 0 {
			v.firstFrameLocked(now)
		}
	}
	if !v.switchDue.IsZero() && !v.switchDue.After(now) {
		v.switchCompleteLocked(now)
	}
	v.progressLocked(now)
}

func (v *viewSession) nextWakeLocked(now time.Time) time.Time {
	next := v.leaseUntil
	for _, due := range []time.Time{v.nextFlush, v.nextErrorsFlush, v.nextReload, v.nextHeartbeat, v.playDue, v.firstFrameDue, v.switchDue, v.nextMilestoneLocked(now)} {
		if !due.IsZero() && due.Before(next) {
			next = due
		}
	}
	return next
}

func (v *viewSession) run(ctx context.Context, previous, done chan struct{}) {
	defer v.client.sessions.Done()
	defer close(done)
	if previous != nil {
		<-previous
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	failed := false
	for {
		v.mu.Lock()
		now := time.Now()
		over := v.stepLocked(ctx, now)
		requests, calls, reload := v.outbox, v.adCalls, v.reloadDue
		v.outbox, v.adCalls, v.reloadDue = nil, nil, false
		source, paused := v.source, v.head.paused
		wake := v.nextWakeLocked(now)
		v.mu.Unlock()
		for _, request := range requests {
			timeout := v.timing.request
			if over {
				timeout = min(timeout, 2*time.Second)
			}
			requestCtx, cancel := context.WithTimeout(context.Background(), timeout)
			// Failures are not retried, as the player's are not; one is
			// logged.
			if err := v.player.post(requestCtx, request); err != nil && !failed {
				failed = true
				log.Printf("player telemetry %s: %v", request.path, err)
			}
			cancel()
		}
		if over {
			return
		}
		if len(calls) > 0 {
			v.requestAds(ctx, calls)
			continue
		}
		if reload && source != nil {
			source.reloadLinks(ctx, paused)
		}
		timer.Reset(max(time.Until(wake), 0))
		select {
		case <-timer.C:
		case <-v.kick:
		case <-ctx.Done():
		}
	}
}

// post sends one telemetry request from the player page: same-origin, with
// the page's cookies, by jQuery (with its Accept and X-Requested-With),
// fetch() or a beacon.
func (player *alternativePlayer) post(ctx context.Context, telemetry telemetryRequest) error {
	endpoint := player.playerURL.ResolveReference(&url.URL{Path: telemetry.path})
	header := [][2]string{{"Content-Type", telemetry.contentType}}
	if telemetry.kind == fetchXHR {
		header = append(header, [2]string{"Accept", "*/*"}, [2]string{"X-Requested-With", "XMLHttpRequest"})
	}
	response, err := player.browser.do(ctx, browserRequest{
		kind: telemetry.kind, method: http.MethodPost, url: endpoint.String(),
		initiator: player.playerURL, referer: player.playerURL.String(), header: header, body: []byte(telemetry.body),
	})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("returned %s", response.Status)
	}
	return nil
}

// playhead is the router's playback as the view knows it: the position
// reported, run on while playing, the duration and the bytes loaded.
type playhead struct {
	position float64
	reported time.Time // when position was reported; zero without a report
	started  time.Time // without a report the position runs from here
	paused   bool
	duration float64
	// Bytes loaded over the view's sources, the last total each reported,
	// the rate between the last two reports and the most a heartbeat
	// showed, which never goes back.
	total, shown int64
	totalAt      time.Time
	rate         float64
	bandwidth    int64
	sourceBytes  map[*alternativeSource]int64
}

func (h *playhead) at(now time.Time) float64 {
	position := 0.0
	switch {
	case !h.reported.IsZero():
		position = h.position
		if !h.paused {
			position += now.Sub(h.reported).Seconds()
		}
	case !h.started.IsZero() && !h.paused:
		position = now.Sub(h.started).Seconds()
	}
	if h.duration > 0 {
		position = min(position, h.duration)
	}
	return max(position, 0)
}

func (h *playhead) set(now time.Time, position float64) {
	h.position, h.reported = position, now
}

func (h *playhead) pause(now time.Time, paused bool) {
	h.position, h.reported = h.at(now), now
	h.paused = paused
}

// addBytes takes in the total a source's segments came to; a total below
// the last one is a count started anew.
func (h *playhead) addBytes(now time.Time, source *alternativeSource, total int64) {
	delta := total - h.sourceBytes[source]
	if delta < 0 {
		delta = total
	}
	h.sourceBytes[source] = total
	if !h.totalAt.IsZero() && now.After(h.totalAt) {
		h.rate = float64(delta) / now.Sub(h.totalAt).Seconds()
	}
	h.total += delta
	h.totalAt = now
}

func (h *playhead) resetBytes() {
	h.total, h.shown, h.rate, h.totalAt = 0, 0, 0, time.Time{}
}

// loaded is the bytes loaded by now: the last total, run on at the last
// rate for up to half a minute while playing.
func (h *playhead) loaded(now time.Time) int64 {
	estimate := h.total
	if !h.paused && !h.totalAt.IsZero() {
		estimate += int64(h.rate * min(now.Sub(h.totalAt), 30*time.Second).Seconds())
	}
	return max(estimate, h.shown)
}

// nextTick is the first tick of a timer started at base after now.
func nextTick(base time.Time, period time.Duration, now time.Time) time.Time {
	if now.Before(base) {
		return base.Add(period)
	}
	return base.Add((now.Sub(base)/period + 1) * period)
}

// milliseconds is Math.round of a performance.now() difference.
func milliseconds(duration time.Duration) int64 {
	return int64(math.Floor(float64(duration)/float64(time.Millisecond) + 0.5))
}

func round3(value float64) float64 { return math.Round(value*1000) / 1000 }
func round6(value float64) float64 { return math.Round(value*1e6) / 1e6 }

func trueOrNull(value bool) any {
	if value {
		return true
	}
	return nil
}

// jsQuality is the player's quality, a number.
func jsQuality(quality string) any {
	if value, err := strconv.Atoi(quality); err == nil {
		return value
	}
	return nil
}

// Blink's alphabet for multipart boundaries, whose last two letters repeat
// so that it has 64.
const boundaryAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789AB"

// formBoundary is the random part of Chrome's multipart boundary.
func formBoundary() string {
	random := make([]byte, 16)
	_, _ = rand.Read(random)
	for i := range random {
		random[i] = boundaryAlphabet[random[i]&0x3f]
	}
	return string(random)
}

// searchParams is URLSearchParams.toString(): the WHATWG
// application/x-www-form-urlencoded serializer, which leaves *-._ alone
// and writes a space as +.
func searchParams(fields [][2]string) string {
	encode := func(value string) string {
		const hexDigits = "0123456789ABCDEF"
		var builder strings.Builder
		for i := 0; i < len(value); i++ {
			character := value[i]
			switch {
			case character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
				character >= '0' && character <= '9' || strings.IndexByte("*-._", character) >= 0:
				builder.WriteByte(character)
			case character == ' ':
				builder.WriteByte('+')
			default:
				builder.WriteByte('%')
				builder.WriteByte(hexDigits[character>>4])
				builder.WriteByte(hexDigits[character&0x0f])
			}
		}
		return builder.String()
	}
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		parts = append(parts, encode(field[0])+"="+encode(field[1]))
	}
	return strings.Join(parts, "&")
}

// jqueryParam is jQuery.param of the fields, flattened to bracketed keys
// already: encodeURIComponent of key and value, a space as +.
func jqueryParam(fields [][2]string) string {
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		parts = append(parts, encodeURIComponentJS(field[0])+"="+encodeURIComponentJS(field[1]))
	}
	return strings.ReplaceAll(strings.Join(parts, "&"), "%20", "+")
}

// jsObject is a JSON object with its keys in order, as JSON.stringify
// writes an object literal.
type jsObject []jsField

type jsField struct {
	key   string
	value any
}

// appendJSValue writes value as JSON.stringify would: no HTML escapes, and
// numbers as JavaScript prints them.
func appendJSValue(buffer []byte, value any) []byte {
	switch value := value.(type) {
	case nil:
		return append(buffer, "null"...)
	case bool:
		return strconv.AppendBool(buffer, value)
	case int:
		return strconv.AppendInt(buffer, int64(value), 10)
	case int64:
		return strconv.AppendInt(buffer, value, 10)
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return append(buffer, "null"...)
		}
		// encoding/json prints numbers as JavaScript does.
		text, _ := json.Marshal(value)
		return append(buffer, text...)
	case string:
		var text bytes.Buffer
		encoder := json.NewEncoder(&text)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(value)
		return append(buffer, bytes.TrimSuffix(text.Bytes(), []byte("\n"))...)
	case jsObject:
		buffer = append(buffer, '{')
		for index, field := range value {
			if index > 0 {
				buffer = append(buffer, ',')
			}
			buffer = appendJSValue(buffer, field.key)
			buffer = append(buffer, ':')
			buffer = appendJSValue(buffer, field.value)
		}
		return append(buffer, '}')
	case []jsObject:
		buffer = append(buffer, '[')
		for index, item := range value {
			if index > 0 {
				buffer = append(buffer, ',')
			}
			buffer = appendJSValue(buffer, item)
		}
		return append(buffer, ']')
	default:
		panic(fmt.Sprintf("appendJSValue: %T", value))
	}
}
