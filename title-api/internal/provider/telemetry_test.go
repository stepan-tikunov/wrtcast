package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"mime/multipart"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testDomain = "https%3A%2F%2Fsite.example%2F"

// newTestView is a view of a page loaded at t0 whose player started 300 ms
// later, with one source whose 720 master was resolved at 50 s, on its own
// clock: the tests run it by hand.
func newTestView(t *testing.T) (*viewSession, *alternativeSource, time.Time) {
	t.Helper()
	client, err := NewClient(Config{BaseURL: "https://example.com", MediaBaseURL: "https://api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.UnixMilli(1_800_000_000_000)
	playerURL, _ := url.Parse("https://player.example/?token_movie=abc&token=tok")
	player := &alternativePlayer{
		playerURL: playerURL, token: "tok", domain: testDomain,
		navigationStart: t0, started: t0.Add(300 * time.Millisecond),
		playbackSent: t0.Add(320 * time.Millisecond), playbackAnswered: t0.Add(520 * time.Millisecond),
	}
	view := newViewSession(client, player, 1547557)
	source := &alternativeSource{
		client: client, label: "(Russian) Dub", player: player, view: view,
		qualities: map[string]string{
			"480": "https://cdn.example/480.m3u8?sig=1",
			"720": "https://cdn.example/720.m3u8?sig=1 or https://cdn2.example/720.m3u8?sig=1",
		},
		variants: map[string]variantInfo{
			"720": {bandwidth: 2000000, averageBandwidth: 1600000, frameRate: 25, width: 1280, height: 720,
				requested: t0.Add(50 * time.Second), loaded: t0.Add(50200 * time.Millisecond)},
			"480": {bandwidth: 900000, width: 854, height: 480},
		},
	}
	return view, source, t0
}

func at(t0 time.Time, milliseconds int64) time.Time {
	return t0.Add(time.Duration(milliseconds) * time.Millisecond)
}

func (v *viewSession) takeOutbox() []telemetryRequest {
	requests := v.outbox
	v.outbox = nil
	return requests
}

// eventsOf decodes an /events fetch: the payload as it was sent and its
// events.
func eventsOf(t *testing.T, request telemetryRequest) (string, []map[string]any) {
	t.Helper()
	if request.path != "/events" || request.kind != fetchFetch || request.contentType != "application/x-www-form-urlencoded;charset=UTF-8" {
		t.Fatalf("not an /events fetch: %+v", request)
	}
	form, err := url.ParseQuery(request.body)
	if err != nil || !strings.HasPrefix(request.body, "token=tok&payload=") || len(form) != 2 {
		t.Fatalf("events body %q: %v", request.body, err)
	}
	return payloadEvents(t, form.Get("payload"))
}

func payloadEvents(t *testing.T, payload string) (string, []map[string]any) {
	t.Helper()
	var decoded struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload %q: %v", payload, err)
	}
	return payload, decoded.Events
}

func eventTypes(events []map[string]any) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, fmt.Sprint(event["event_type"]))
	}
	return types
}

// objectKeys is the keys of each object in the events array of payload,
// in the order they were written.
func objectKeys(t *testing.T, payload string) [][]string {
	t.Helper()
	var envelope struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		t.Fatal(err)
	}
	var all [][]string
	for _, raw := range envelope.Events {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		_, _ = decoder.Token()
		var keys []string
		for decoder.More() {
			key, _ := decoder.Token()
			keys = append(keys, key.(string))
			var skip json.RawMessage
			_ = decoder.Decode(&skip)
		}
		all = append(all, keys)
	}
	return all
}

var commonEventKeys = []string{"url", "manifestUrl", "currentTime", "duration", "bufferLength", "paused", "audioTrack", "quality", "subtitle"}

func wantKeys(t *testing.T, keys []string, specific ...string) {
	t.Helper()
	want := append(append(append([]string(nil), commonEventKeys...), specific...), "at", "since")
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("event keys %v, want %v", keys, want)
	}
}

func wantStat(t *testing.T, request telemetryRequest, percent string) {
	t.Helper()
	want := "id=1547557&token=tok&domain=https%253A%252F%252Fsite.example%252F" +
		"&url=https%3A%2F%2Fplayer.example%2F%3Ftoken_movie%3Dabc%26token%3Dtok&type=mgpo&ab=false" + percent +
		"&info%5Bwasm%5D=true&info%5Bsw%5D=false" +
		"&info%5Bresolution%5D%5BscreenWidth%5D=1920&info%5Bresolution%5D%5BscreenHeight%5D=1080" +
		"&info%5Bresolution%5D%5BwindowWidth%5D=854&info%5Bresolution%5D%5BwindowHeight%5D=480" +
		"&info%5Bresolution%5D%5BdevicePixelRatio%5D=1&info%5BwebSocket%5D=true&info%5Bplatform%5D=Win32"
	if request.path != "/stat" || request.kind != fetchXHR || request.contentType != "application/x-www-form-urlencoded; charset=UTF-8" ||
		request.body != want {
		t.Errorf("stat = %+v\nwant body %s", request, want)
	}
}

// A view from the play button to the end of the video, on the player's
// timers and with the router's reports, field for field.
func TestViewTelemetry(t *testing.T) {
	t.Parallel()
	view, source, t0 := newTestView(t)
	ctx := context.Background()
	view.mu.Lock()
	defer view.mu.Unlock()
	report := func(milliseconds int64, report PlaybackReport) viewChange {
		t.Helper()
		view.leaseUntil = at(t0, milliseconds).Add(time.Minute)
		return view.reportLocked(at(t0, milliseconds), source, report)
	}
	step := func(milliseconds int64) {
		t.Helper()
		if view.stepLocked(ctx, at(t0, milliseconds)) {
			t.Fatalf("the view ended at %d ms", milliseconds)
		}
	}

	// The router's first headers request presses play: the preroll's ad
	// lists come back empty, and the media plays.
	view.leaseUntil = at(t0, 50500).Add(time.Minute)
	view.beginLocked(at(t0, 50500), source, PlaybackReport{Resolution: "720"})
	report(50500, PlaybackReport{Resolution: "720"})
	if wake := view.nextWakeLocked(at(t0, 50500)); !wake.Equal(at(t0, 51100)) {
		t.Fatalf("next wake %v", wake.Sub(t0))
	}
	step(51100)
	requests := view.takeOutbox()
	if len(requests) != 1 {
		t.Fatalf("after the play %+v", requests)
	}
	wantStat(t, requests[0], "")
	// The next timers of the player, which started at 300 ms.
	if !view.nextFlush.Equal(at(t0, 60300)) || !view.nextHeartbeat.Equal(at(t0, 60300)) {
		t.Fatalf("timers at %v, %v", view.nextFlush.Sub(t0), view.nextHeartbeat.Sub(t0))
	}

	// The TV plays: the first frame, at once to /events.
	change := report(58000, PlaybackReport{
		Resolution: "720", Position: 0, HasPosition: true, HasPaused: true, Duration: 2640,
		Bytes: 3000000, HasBytes: true, Bandwidth: 12000000,
	})
	if change != (viewChange{}) {
		t.Fatalf("first frame change %+v", change)
	}
	requests = view.takeOutbox()
	if len(requests) != 2 {
		t.Fatalf("after the first frame %+v", requests)
	}
	payload, events := eventsOf(t, requests[0])
	if got := eventTypes(events); !reflect.DeepEqual(got, []string{
		"ad_block_start", "ad_block_end", "view_start", "startup", "play", "playback_ready", "first_frame", "file_load_complete",
	}) {
		t.Fatalf("first batch %v", got)
	}
	envelope := fmt.Sprintf(`{"token":"tok","domain":%q,"isTrailer":false,"dropped":0,"playerInitAt":1800000000300,`+
		`"clientSessionId":%q,"clientRequestId":%q,`+
		`"env":{"connectionType":"4g","downlink":10,"pixelRatio":1,"screenW":1920,"screenH":1080},"events":[`,
		testDomain, view.sessionID, view.requestID)
	common := func(time, duration, buffer string, paused bool) string {
		return `{"url":null,"manifestUrl":"https://cdn.example/720.m3u8?sig=1","currentTime":` + time +
			`,"duration":` + duration + `,"bufferLength":` + buffer + fmt.Sprintf(`,"paused":%t`, paused) +
			`,"audioTrack":"(Russian) Dub","quality":720,"subtitle":null,`
	}
	for _, want := range []string{
		envelope + common("0", "null", "0", true) + `"event_type":"ad_block_start","at":1800000050500,"since":50500},`,
		common("0", "null", "0", true) + `"event_type":"ad_block_end","adDurationMs":600,"adDurationCumulativeMs":600,"at":1800000051100,"since":51100},`,
		common("0", "null", "0", false) + `"event_type":"view_start","fileChangeType":"initial","autoplay":false,"resumeFrom":0,` +
			`"startupQuality":720,"startupAudio":"(Russian) Dub","startupBitrate":2000000,"startupFrameRate":25,` +
			`"renderWidth":854,"renderHeight":480,"at":1800000051100,"since":51100},`,
		common("0", "null", "0", false) + `"event_type":"startup","fileChangeType":"initial","playerStartupMs":50800,` +
			`"manifestLoadMs":200,"userWaitMs":900,"bandwidthEstimate":500000,"startupBitrate":2000000,"at":1800000051100,"since":51100},`,
		common("0", "null", "0", false) + `"event_type":"play","at":1800000051100,"since":51100},`,
		common("0", "2640", "15", false) + `"event_type":"playback_ready","timeToPlaybackReadyMs":6900,"bandwidthEstimate":12000000,"at":1800000058000,"since":58000},`,
		common("0", "2640", "15", false) + `"event_type":"first_frame","timeToFirstFrameMs":6900,"adDurationMs":600,"viewStartSource":"playing","at":1800000058000,"since":58000},`,
		common("0", "2640", "15", false) + `"event_type":"file_load_complete","fileChangeType":"initial","viewStartSource":"playing",` +
			`"totalFileChangeMs":56780,"backendMs":200,"manifestLoadMs":200,"userWaitMs":900,"playToFirstFrameMs":6900,` +
			`"adDurationMs":600,"segmentLoadMs":6300,"autoplay":false,"at":1800000058000,"since":58000}]}`,
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload lacks\n%s\nin\n%s", want, payload)
		}
	}
	// At 0 percent, /stat too.
	wantStat(t, requests[1], "&percent=0")

	// The flush goes before the heartbeat of the same moment, which counts
	// the bytes and the time played.
	step(60300)
	requests = view.takeOutbox()
	if len(requests) != 1 {
		t.Fatalf("at 60.3 s %+v", requests)
	}
	payload, events = eventsOf(t, requests[0])
	if got := eventTypes(events); !reflect.DeepEqual(got, []string{"view_percent"}) ||
		events[0]["percent"] != 0.0 || events[0]["watchTimeSec"] != 0.0 {
		t.Fatalf("batch at 60.3 s %v", events)
	}
	wantKeys(t, objectKeys(t, payload)[0], "event_type", "percent", "watchTimeSec")
	// More bytes come at 300 kB/s; the next heartbeat runs them on.
	report(68000, PlaybackReport{Position: 10, HasPosition: true, Bytes: 6000000, HasBytes: true})
	step(70300)
	// Paused at 17 s: the heartbeats stop.
	if change := report(75000, PlaybackReport{Position: 17, HasPosition: true, Paused: true, HasPaused: true}); change != (viewChange{paused: true}) {
		t.Fatalf("pause change %+v", change)
	}
	step(80300)
	step(90300)
	requests = view.takeOutbox()
	if len(requests) != 1 {
		t.Fatalf("at 90.3 s %+v", requests)
	}
	payload, events = eventsOf(t, requests[0])
	if got := eventTypes(events); !reflect.DeepEqual(got, []string{"heartbeat", "heartbeat", "pause"}) {
		t.Fatalf("batch at 90.3 s %v", got)
	}
	keys := objectKeys(t, payload)
	wantKeys(t, keys[0], "event_type", "bandwidthEstimate", "droppedFrames", "droppedFramesDelta", "playedSecondsDelta",
		"isBuffering", "isSeeking", "playbackRate", "currentBitrate", "currentFrameRate", "videoWidth", "videoHeight",
		"isUpscaling", "rebufferCountSession", "rebufferTotalMsSession", "bytesLoadedTotal", "bytesLoadedDelta")
	wantKeys(t, keys[2], "event_type")
	for index, want := range []map[string]any{
		// hls.js's worker has the fragments' bytes by the time the
		// bundle counts them.
		{"currentTime": 2.3, "playedSecondsDelta": 2.3, "bytesLoadedTotal": 0.0, "bytesLoadedDelta": 0.0},
		{"currentTime": 12.3, "playedSecondsDelta": 10.0, "bytesLoadedTotal": 0.0, "bytesLoadedDelta": 0.0},
	} {
		for key, value := range want {
			if events[index][key] != value {
				t.Errorf("heartbeat %d %s = %v, want %v", index, key, events[index][key], value)
			}
		}
		if events[index]["bandwidthEstimate"] != 12e6 || events[index]["currentBitrate"] != 2e6 ||
			events[index]["currentFrameRate"] != 25.0 || events[index]["videoWidth"] != 1280.0 ||
			events[index]["isUpscaling"] != false || events[index]["paused"] != false {
			t.Errorf("heartbeat %d = %v", index, events[index])
		}
	}
	if events[2]["currentTime"] != 17.0 || events[2]["paused"] != true {
		t.Errorf("pause = %v", events[2])
	}

	// Resumed: play with how long the pause took.
	report(95000, PlaybackReport{HasPaused: true})
	// A seek to the middle passes 10 and 30 percent.
	if change := report(100000, PlaybackReport{Position: 1300, HasPosition: true}); change != (viewChange{seeked: true}) {
		t.Fatalf("seek change %+v", change)
	}
	// /stat is next at 59.5 percent.
	if wake := view.nextMilestoneLocked(at(t0, 100000)); wake.Sub(at(t0, 100000+270800)).Abs() > 2*time.Millisecond {
		t.Fatalf("next milestone in %v", wake.Sub(at(t0, 100000)))
	}
	// Another quality: the batch so far goes, and the switch has a request
	// id of its own.
	requestID := view.requestID
	report(101000, PlaybackReport{Resolution: "480"})
	requests = view.takeOutbox()
	if len(requests) != 1 || view.requestID == requestID {
		t.Fatalf("quality switch sent %+v", requests)
	}
	payload, events = eventsOf(t, requests[0])
	if got := eventTypes(events); !reflect.DeepEqual(got, []string{"play", "seek_start", "seek_end", "view_percent", "view_percent"}) ||
		events[0]["pauseDurationMs"] != 20000.0 || events[1]["seekFrom"] != 1300.0 || events[2]["seekDistance"] != 0.0 ||
		events[3]["percent"] != 10.0 || events[4]["percent"] != 30.0 {
		t.Fatalf("batch before the switch %v", events)
	}
	if !strings.Contains(payload, `"clientRequestId":"`+requestID+`"`) {
		t.Error("the batch before the switch went with the new request id")
	}
	report(106000, PlaybackReport{Resolution: "480", Position: 1305, HasPosition: true})

	// Near the end, the end endpoint is the video ending.
	report(110000, PlaybackReport{Position: 2637, HasPosition: true})
	view.ending = true
	if !view.stepLocked(ctx, at(t0, 111000)) || view.running {
		t.Fatal("the view went on after the end")
	}
	requests = view.takeOutbox()
	if len(requests) != 1 {
		t.Fatalf("at the end %+v", requests)
	}
	payload, events = eventsOf(t, requests[0])
	if got := eventTypes(events); !reflect.DeepEqual(got, []string{
		"quality_switch", "switch_complete", "seek_start", "seek_end", "view_percent", "view_percent", "pause", "view_finish",
	}) {
		t.Fatalf("last batch %v", got)
	}
	keys = objectKeys(t, payload)
	wantKeys(t, keys[0], "event_type", "qualityBefore", "qualityAfter", "isUpscale", "isDownscale", "bandwidthEstimate")
	wantKeys(t, keys[1], "event_type", "switchType", "switchToFirstFrameMs", "manifestLoadMs", "segmentLoadMs", "bandwidthEstimate")
	wantKeys(t, keys[6], "event_type")
	wantKeys(t, keys[7], "event_type", "watchTimeSec", "completed")
	if switched := events[0]; switched["qualityBefore"] != 720.0 || switched["qualityAfter"] != 480.0 ||
		switched["isUpscale"] != nil || switched["isDownscale"] != true || switched["quality"] != 720.0 {
		t.Errorf("quality_switch = %v", switched)
	}
	if complete := events[1]; complete["switchType"] != "quality" || complete["switchToFirstFrameMs"] != 5000.0 ||
		complete["quality"] != 480.0 || complete["manifestUrl"] != "https://cdn.example/480.m3u8?sig=1" {
		t.Errorf("switch_complete = %v", complete)
	}
	// Played: 17 s before the pause, then 5 to the first seek, 1 to the
	// switch, 5 to the next report (which takes 1 back, not counted), 4 to
	// the second seek and 1 to the end; the seeks don't count.
	// The media element pauses as it ends.
	if finish := events[7]; finish["completed"] != true || finish["watchTimeSec"] != 33.0 || events[5]["percent"] != 97.0 ||
		events[6]["paused"] != true {
		t.Errorf("view_finish = %v, %v", finish, events[5])
	}
}

// A page that goes away before the TV played sends what is left by beacon,
// as multipart form data, when it is hidden, and then tells it never
// started.
func TestViewTelemetryPageGoes(t *testing.T) {
	t.Parallel()
	view, source, t0 := newTestView(t)
	view.mu.Lock()
	defer view.mu.Unlock()
	view.leaseUntil = at(t0, 60000)
	view.beginLocked(at(t0, 50500), source, PlaybackReport{})
	view.stepLocked(context.Background(), at(t0, 51100))
	view.takeOutbox()
	if !view.stepLocked(context.Background(), at(t0, 60000)) {
		t.Fatal("the lease ran out and the view went on")
	}
	requests := view.takeOutbox()
	if len(requests) != 2 || requests[0].path != "/events" || requests[1].path != "/events" {
		t.Fatalf("at the lease's end %+v", requests)
	}
	mediaType, parameters, err := mime.ParseMediaType(requests[0].contentType)
	if err != nil || mediaType != "multipart/form-data" ||
		!strings.HasPrefix(parameters["boundary"], "----WebKitFormBoundary") || len(parameters["boundary"]) != 38 {
		t.Fatalf("beacon Content-Type %q", requests[0].contentType)
	}
	reader := multipart.NewReader(strings.NewReader(requests[0].body), parameters["boundary"])
	form, err := reader.ReadForm(1 << 20)
	if err != nil || len(form.Value) != 2 || form.Value["token"][0] != "tok" {
		t.Fatalf("beacon form %+v, %v", form, err)
	}
	if !strings.HasPrefix(requests[0].body, "--"+parameters["boundary"]+"\r\nContent-Disposition: form-data; name=\"token\"\r\n\r\ntok\r\n") {
		t.Errorf("beacon body %q", requests[0].body)
	}
	_, events := payloadEvents(t, form.Value["payload"][0])
	if got := eventTypes(events); !reflect.DeepEqual(got, []string{"ad_block_start", "ad_block_end", "view_start", "startup", "play"}) {
		t.Fatalf("beacon events %v", events)
	}
	_, parameters, _ = mime.ParseMediaType(requests[1].contentType)
	form, _ = multipart.NewReader(strings.NewReader(requests[1].body), parameters["boundary"]).ReadForm(1 << 20)
	if _, events := payloadEvents(t, form.Value["payload"][0]); len(events) != 1 ||
		events[0]["event_type"] != "exit_before_start" || events[0]["timeToExitMs"] != 8900.0 {
		t.Fatalf("pagehide beacon events %v", events)
	}
}

func TestPlayheadBytes(t *testing.T) {
	t.Parallel()
	t0 := time.Unix(1_800_000_000, 0)
	source, other := &alternativeSource{}, &alternativeSource{}
	head := playhead{sourceBytes: map[*alternativeSource]int64{}}
	head.addBytes(t0, source, 1000)
	if head.loaded(t0.Add(time.Hour)) != 1000 {
		t.Fatal("one report ran on")
	}
	// 100 bytes a second, run on for half a minute at most.
	head.addBytes(t0.Add(10*time.Second), source, 2000)
	for seconds, want := range map[int]int64{10: 2000, 15: 2500, 40: 5000, 100: 5000} {
		if got := head.loaded(t0.Add(time.Duration(seconds) * time.Second)); got != want {
			t.Errorf("loaded at %d s = %d, want %d", seconds, got, want)
		}
	}
	// A heartbeat showed 4000; a smaller report doesn't take it back.
	head.shown = 4000
	head.addBytes(t0.Add(20*time.Second), source, 2000)
	if got := head.loaded(t0.Add(25 * time.Second)); got != 4000 {
		t.Errorf("loaded after a report below the shown = %d", got)
	}
	// Another source adds its own; a count started anew adds all of it.
	head.addBytes(t0.Add(30*time.Second), other, 3000)
	head.addBytes(t0.Add(40*time.Second), source, 500)
	if head.total != 5500 {
		t.Errorf("total = %d", head.total)
	}
	// Paused, nothing runs on.
	head.pause(t0.Add(40*time.Second), true)
	if got := head.loaded(t0.Add(50 * time.Second)); got != 5500 {
		t.Errorf("loaded while paused = %d", got)
	}
}

func TestTelemetryEncoding(t *testing.T) {
	t.Parallel()
	if got := searchParams([][2]string{{"a b", "x*y~z-._!'()é/"}}); got != "a+b=x*y%7Ez-._%21%27%28%29%C3%A9%2F" {
		t.Errorf("searchParams = %q", got)
	}
	if got := jqueryParam([][2]string{{"info[resolution][screenWidth]", "a b!*~"}}); got != "info%5Bresolution%5D%5BscreenWidth%5D=a+b!*~" {
		t.Errorf("jqueryParam = %q", got)
	}
	if got := string(appendJSValue(nil, jsObject{
		{"s", "<a&b>é"}, {"n", nil}, {"f", 12.5}, {"i", 3.0}, {"big", int64(1800000000300)}, {"nan", nan()},
		{"tiny", 1e-7}, {"list", []jsObject{{{"x", true}}}},
	})); got != `{"s":"<a&b>é","n":null,"f":12.5,"i":3,"big":1800000000300,"nan":null,"tiny":1e-7,"list":[{"x":true}]}` {
		t.Errorf("appendJSValue = %s", got)
	}
	for userAgent, want := range map[string]string{
		defaultUserAgent: "Linux armv7l",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36":         "Win32",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36":                   "Linux x86_64",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15": "MacIntel",
	} {
		if got := navigatorPlatform(userAgent); got != want {
			t.Errorf("navigatorPlatform(%q) = %q, want %q", userAgent, got, want)
		}
	}
	if id := randomID(); len(id) != 32 || id[12] != '4' || strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("randomID = %q", id)
	}
	base := time.Unix(1000, 0)
	for _, test := range []struct{ now, want int }{{0, 10}, {5, 10}, {10, 20}, {-5, 10}} {
		if got := nextTick(base, 10*time.Second, base.Add(time.Duration(test.now)*time.Second)); !got.Equal(base.Add(time.Duration(test.want) * time.Second)) {
			t.Errorf("nextTick at %d = %v", test.now, got.Sub(base))
		}
	}
}

func nan() float64 {
	zero := 0.0
	return zero / zero
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !done(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no %s", what)
		}
	}
}

// The view of a source without a socket, over HTTP: same-origin requests
// with the page's cookies, a batch per flush and none more when the site
// refuses them, a beacon when the page goes; ready playlists have none.
func TestViewTelemetryOverHTTP(t *testing.T) {
	t.Parallel()
	fake := newFakeAlternativePlayer(t, true, 1, "")
	client, err := NewClient(Config{BaseURL: "https://example.com", MediaBaseURL: "https://api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	client.sessionTiming = testSessionTiming
	client.telemetryTiming = telemetryTiming{
		heartbeat: 40 * time.Millisecond, flush: 120 * time.Millisecond, adBlock: 20 * time.Millisecond,
		firstFrame: time.Second, switchComplete: 200 * time.Millisecond, request: 2 * time.Second,
	}
	t.Cleanup(client.Close)
	id := alternativeMasterID(t, client, fake)
	ctx := context.Background()

	headers, err := client.MediaHeaders(ctx, id, PlaybackReport{Resolution: "720"})
	if err != nil || headers.TTL != activeHeadersTTL || headers.Proxy != "/v1/media/"+id+"/cdn" {
		t.Fatalf("headers = %+v, %v", headers, err)
	}
	// The receiver loads the 720 the router chose.
	if _, err := client.QualityPlaylist(ctx, id, "720"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "/stat", func() bool { return len(fake.telemetryOf("/stat")) == 1 })
	sameOrigin := func(record telemetryRecord, contentType string, xhr bool) {
		t.Helper()
		for name, want := range map[string]string{
			"User-Agent": chromeUserAgent, "Accept": "*/*", "Content-Type": contentType,
			"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Dest": "empty", "sec-ch-ua-platform": chromePlatform,
			"Origin": fake.player.URL, "Referer": fake.url(), "Cookie": "page=1",
		} {
			if got := record.header.Get(name); got != want {
				t.Errorf("%s %s = %q, want %q", record.path, name, got, want)
			}
		}
		if got := record.header.Get("X-Requested-With"); xhr != (got == "XMLHttpRequest") {
			t.Errorf("%s X-Requested-With = %q", record.path, got)
		}
	}
	stat := fake.telemetryOf("/stat")[0]
	sameOrigin(stat, "application/x-www-form-urlencoded; charset=UTF-8", true)
	if want := "id=7&token=tok&domain=&url=" + encodeURIComponentJS(fake.url()) + "&type=mgpo&ab=false&info%5Bwasm%5D=true"; !strings.HasPrefix(stat.body, want) {
		t.Errorf("/stat body %q", stat.body)
	}

	// The TV plays.
	if _, err := client.MediaHeaders(ctx, id, PlaybackReport{
		Resolution: "720", HasPosition: true, HasPaused: true, Duration: 100, Bytes: 5000, HasBytes: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "heartbeats", func() bool {
		batches := fake.telemetryOf("/events")
		return len(batches) >= 2 && strings.Contains(batches[len(batches)-1].body, "heartbeat")
	})
	batches := fake.telemetryOf("/events")
	sameOrigin(batches[0], "application/x-www-form-urlencoded;charset=UTF-8", false)
	form, _ := url.ParseQuery(batches[0].body)
	if _, events := payloadEvents(t, form.Get("payload")); events[len(events)-1]["event_type"] != "file_load_complete" ||
		events[0]["manifestUrl"] != fake.cdn.URL+"/720.m3u8?sig=1" || events[2]["event_type"] != "view_start" ||
		events[2]["startupBitrate"] != 1000.0 {
		t.Errorf("first batch %v", events)
	}
	if stats := fake.telemetryOf("/stat"); len(stats) != 2 || !strings.Contains(stats[1].body, "&percent=0&") {
		t.Errorf("/stat at 0 percent: %+v", stats)
	}

	// Refused: logged, not retried, the next batch on its timer.
	fake.mu.Lock()
	fake.refuse = 403
	fake.mu.Unlock()
	before := len(fake.telemetryOf("/events"))
	time.Sleep(600 * time.Millisecond)
	refused := fake.telemetryOf("/events")[before:]
	if len(refused) < 3 || len(refused) > 6 {
		t.Errorf("%d batches in 600 ms of 120 ms flushes", len(refused))
	}
	for index := 1; index < len(refused); index++ {
		if gap := refused[index].at.Sub(refused[index-1].at); gap < 60*time.Millisecond {
			t.Errorf("batches %v apart", gap)
		}
	}

	// Stopped mid-way: what is left goes by beacon, and the view is over.
	client.EndMediaSession(id)
	entry, _ := client.masters.entry(id)
	view := entry.source.view
	view.mu.Lock()
	running := view.running
	view.mu.Unlock()
	last := fake.telemetryOf("/events")
	if running || !strings.HasPrefix(last[len(last)-1].header.Get("Content-Type"), "multipart/form-data; boundary=----WebKitFormBoundary") {
		t.Fatalf("after the end: running %t, last %+v", running, last[len(last)-1].header)
	}

	// A ready playlist keeps its headers for a minute and has no view.
	count := len(fake.telemetry)
	ready := client.masters.put([]byte("#EXTM3U\n"))
	if headers, err := client.MediaHeaders(ctx, ready, PlaybackReport{Resolution: "720"}); err != nil || headers.TTL != mediaHeadersTTL {
		t.Fatalf("ready playlist headers = %+v, %v", headers, err)
	}
	time.Sleep(100 * time.Millisecond)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.telemetry) != count {
		t.Fatalf("a ready playlist sent %d telemetry requests", len(fake.telemetry)-count)
	}
}
