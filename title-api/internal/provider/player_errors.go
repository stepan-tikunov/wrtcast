package provider

import (
	"fmt"
	"strings"
	"time"
)

// The player reports every hls.js ERROR event to /errors: a record of the
// error, the network and the media element, buffered unless it repeats
// the last one (type, details, parent, reason, fatal), sent at once when
// fatal and otherwise with the batch every 30 seconds from the player's
// start, before a switch and by beacon when the page goes. title-api's
// CDN requests are the page's hls.js loads (see alternativeSource), so a
// failed one is such an error: a 4xx hls.js does not retry, and with one
// level and nothing to switch to it is fatal; a 5xx or a lost connection
// it retries, so it is not.

const maxBufferedErrors = 500

// loadError takes in a failed CDN load of kind (manifest, level, init,
// fragment) with its HTTP status (0 without a response), as hls.js reports
// it.
func (v *viewSession) loadError(source *alternativeSource, kind, rawURL string, status int, started time.Time, cause error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	details := map[string]string{"manifest": "manifestLoadError", "level": "levelLoadError"}[kind]
	if details == "" {
		details = "fragLoadError"
	}
	var parent any = "main"
	if kind == "manifest" {
		parent = nil
	}
	fatal := status >= 400 && status < 500
	// XMLHttpRequest's statusText, which HTTP/2 does not have.
	reason := fmt.Sprintf("HTTP Error %d ", status)
	flag := "N"
	if fatal {
		flag = "F"
	}
	if now.Sub(v.lastErrorAt) > 5*time.Second {
		v.lastErrorSignature = ""
	}
	signature := strings.Join([]string{"networkError", details, fmt.Sprint(parent), reason, flag}, "|")
	v.lastErrorAt = now
	if signature == v.lastErrorSignature {
		return
	}
	v.lastErrorSignature = signature
	var responseURL any
	if status > 0 {
		responseURL = rawURL
	}
	var networkDetails any
	if status > 0 {
		networkDetails = jsObject{{"responseURL", rawURL}, {"status", status}, {"statusText", nil}}
	}
	var fragSn, fragStart, fragDuration, fragLevel any
	if kind == "fragment" || kind == "init" {
		fragLevel = 0
		source.media.mu.Lock()
		if place, ok := source.media.fragments[rawURL]; ok {
			fragSn, fragStart, fragDuration = place.index, round6(place.start), round6(place.end-place.start)
		}
		source.media.mu.Unlock()
	}
	var currentTime, duration any = 0, nil
	if v.loaded {
		currentTime = round6(v.head.at(now))
	}
	if v.head.duration > 0 {
		duration = v.head.duration
	}
	readyState := 0
	if v.loaded {
		readyState = 4
	}
	buffers := "updating=false ranges=empty"
	if _, loadedEnd, _ := source.media.stats(); loadedEnd > 0 {
		buffers = fmt.Sprintf("updating=false ranges=%.1f->%.1f", max(0, v.head.at(now)-1), loadedEnd)
	}
	source.mu.Lock()
	alternatives := len(strings.Split(source.qualities[v.quality], " or "))
	source.mu.Unlock()
	var manifest any
	if v.manifestURL != "" {
		manifest = v.manifestURL
	}
	record := jsObject{
		{"at", now.UnixMilli()}, {"since", milliseconds(now.Sub(v.navigationStart))},
		{"type", "networkError"}, {"details", details}, {"fatal", fatal}, {"parent", parent},
		{"url", rawURL}, {"manifestUrl", manifest}, {"reason", reason}, {"errorName", "Error"},
		// The error's stack, frames of the bundle's hls.js, is not made up.
		{"errorStack", nil}, {"aborted", false},
		{"onlineState", true}, {"visibility", "visible"}, {"hasFocus", true}, {"saveData", false},
		{"rtt", connectionRTT}, {"effectiveTypeAtError", connectionType},
		{"timeToFailMs", milliseconds(now.Sub(started))}, {"responseUrl", responseURL},
		{"errorAction", nil}, {"errorActionFlags", nil}, {"errorEvent", "hlsError"},
		{"bytesLoaded", nil}, {"rangeStart", nil}, {"rangeEnd", nil}, {"trackId", nil},
		{"fragByteStart", nil}, {"fragByteEnd", nil}, {"httpCode", status}, {"httpText", ""},
		{"mediaErrorCode", nil}, {"mediaErrorMessage", nil}, {"currentTime", currentTime}, {"duration", duration},
		{"bufferLength", v.bufferLocked(now)}, {"droppedFrames", 0},
		{"paused", !v.playAttempted || v.paused || !v.playDue.IsZero()},
		{"bandwidthEstimate", v.bandwidthLocked()}, {"readyState", readyState}, {"networkState", 2}, {"seeking", false},
		{"audioTrack", v.label}, {"quality", jsQuality(v.quality)}, {"attachMediaHls", true}, {"changes", 0},
		{"manifestIndex", 0}, {"manifestTotal", alternatives}, {"sbAudio", buffers}, {"sbVideo", buffers},
		{"fragSn", fragSn}, {"fragStart", fragStart}, {"fragDuration", fragDuration}, {"fragLevel", fragLevel},
		{"networkDetails", networkDetails},
		{"retryCounters", jsObject{
			{"bufferAppendErrorCountAudio", 0}, {"bufferAppendErrorCountVideo", 0},
			{"bufferFullErrorCountAudio", 0}, {"tvStallCount", 0}, {"reloadManifest", 0},
		}},
	}
	if len(v.errors) >= maxBufferedErrors {
		v.errors = v.errors[1:]
		v.errorsDropped++
	}
	v.errors = append(v.errors, record)
	if fatal && v.running {
		v.flushErrorsLocked(now, false)
		v.signal()
	}
}

// flushErrorsLocked sends the errors so far, if any, as flushLocked sends
// the events.
func (v *viewSession) flushErrorsLocked(now time.Time, beacon bool) {
	if len(v.errors) == 0 {
		return
	}
	payload := appendJSValue(nil, jsObject{
		{"token", v.player.token}, {"domain", v.player.domain}, {"dropped", v.errorsDropped},
		{"playerInitAt", v.playerInitAt.UnixMilli()},
		{"clientSessionId", v.errorsSessionID}, {"clientRequestId", v.errorsRequestID},
		{"env", jsObject{
			{"connectionType", connectionType}, {"downlink", downlinkMbps}, {"pixelRatio", devicePixelRatio},
			{"screenW", screenWidth}, {"screenH", screenHeight},
		}},
		{"errors", v.errors},
	})
	v.errors, v.errorsDropped = nil, 0
	v.outbox = append(v.outbox, pagePost("/errors", v.player.token, string(payload), beacon))
}

// pagePost is a batch of the player's telemetry: by fetch() as a form, or
// by beacon as multipart form data when the page goes.
func pagePost(path, token, payload string, beacon bool) telemetryRequest {
	if beacon {
		boundary := "----WebKitFormBoundary" + formBoundary()
		var body strings.Builder
		for _, part := range [][2]string{{"token", token}, {"payload", payload}} {
			fmt.Fprintf(&body, "--%s\r\nContent-Disposition: form-data; name=%q\r\n\r\n%s\r\n", boundary, part[0], part[1])
		}
		fmt.Fprintf(&body, "--%s--\r\n", boundary)
		return telemetryRequest{path: path, contentType: "multipart/form-data; boundary=" + boundary, body: body.String(), kind: fetchBeacon}
	}
	return telemetryRequest{
		path: path, contentType: "application/x-www-form-urlencoded;charset=UTF-8",
		body: searchParams([][2]string{{"token", token}, {"payload", payload}}), kind: fetchFetch,
	}
}
