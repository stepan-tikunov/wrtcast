package provider

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The player's ads come from its ad network's lists, which its rotate
// functions ask for as the page's config.ads names them: the preroll
// (rotateP) on the play button, a background slot (rotatePM, the midroll
// at time 0) once the content plays, and the midroll at 300 seconds
// (rotateM1). An empty list is asked for again, up to [count_preroll]
// times for the first two; the preroll ends once its lists are in.
// title-api asks for the lists when the player would and treats every
// list as one without a working ad: it fetches no VAST and no creative and
// fires no impression or tracking pixel, and the player reports such a
// preroll as an ad block of the lists' time and the others not at all.

// adListURL is the ad network's list endpoint; empty in tests, which time
// the preroll with telemetryTiming.adBlock instead.
var adListURL = "https://pc.alloviewroll.com/lists.php"

// midrollTime is the cue point of rotateM1.
const midrollTime = 300

type adSlotKind int

const (
	slotPreroll adSlotKind = iota // rotateP
	slotStart                     // rotatePM, the midroll at time 0
	slotMidroll                   // rotateM1, at midrollTime
)

// adCall is one request for a list: its count and module, and bg for the
// background slot. A call after one of the same slot goes only if that
// one's list was empty.
type adCall struct {
	slot              adSlotKind
	count, module, bg int
}

// adSlot is the calls a slot may make, if the page has it.
func (player *alternativePlayer) adSlot(slot adSlotKind) []adCall {
	if adListURL == "" || !player.ads.Enabled {
		return nil
	}
	link := map[adSlotKind]string{slotPreroll: "jsf:rotateP", slotStart: "jsf:rotatePM", slotMidroll: "jsf:rotateM1"}[slot]
	if !player.ads.has(link) {
		return nil
	}
	if slot == slotMidroll {
		return []adCall{{slot: slot, count: 5, module: 3}}
	}
	limit, err := strconv.Atoi(player.ads.value("[count_preroll]"))
	if err != nil {
		limit = 2
	}
	calls := make([]adCall, 0, limit)
	for request := 1; request <= min(limit, 10); request++ {
		module := request
		if request >= 3 {
			module = 9
		}
		call := adCall{slot: slot, count: request, module: module}
		if slot == slotStart {
			call.bg = 1
		}
		calls = append(calls, call)
	}
	return calls
}

// has tells whether the config names link as the preroll or a midroll.
func (ads alternativeAds) has(link string) bool {
	if ads.Preroll == link {
		return true
	}
	for _, midroll := range ads.Midroll {
		if midroll.Link == link {
			return true
		}
	}
	return false
}

// value is a value of the replace map as the page puts it into a URL.
func (ads alternativeAds) value(key string) string {
	raw := ads.Replace[key]
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	var flag bool
	if json.Unmarshal(raw, &flag) == nil {
		return strconv.FormatBool(flag)
	}
	return ""
}

// requestAds makes the calls in order, a slot's next one only after an
// empty list, and then ends a preroll that waits for them.
func (v *viewSession) requestAds(ctx context.Context, calls []adCall) {
	skip := map[adSlotKind]bool{}
	for _, call := range calls {
		if skip[call.slot] {
			continue
		}
		if !v.player.adList(ctx, call) {
			skip[call.slot] = true
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.prerollPending {
		v.prerollPending = false
		v.playDue = time.Now()
	}
}

// adList asks for a list as the page's jQuery does, a cross-origin form
// POST, and tells whether it came back empty.
func (player *alternativePlayer) adList(ctx context.Context, call adCall) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	zone := browserTimeZone()
	trailer := "0"
	if player.ads.value("[is_trailer]") == "true" {
		trailer = "1"
	}
	form := jqueryParam([][2]string{
		{"token", player.ads.value("[token]")}, {"url", player.ads.value("[domain]")}, {"type", player.ads.value("[type]")},
		{"count", strconv.Itoa(call.count)}, {"module", strconv.Itoa(call.module)}, {"up", player.fingerprint},
		{"kp", player.ads.value("[kp]")}, {"cat", player.ads.value("[cat]")}, {"movie", player.ads.value("[movie]")},
		{"trailer", trailer}, {"lang", navigatorLanguage}, {"tz", zone.name},
		{"tzo", strconv.Itoa(zone.offset(time.Now()))}, {"bg", strconv.Itoa(call.bg)}, {"mt", player.movieID},
	})
	response, err := player.browser.do(ctx, browserRequest{
		kind: fetchXHR, method: http.MethodPost, url: adListURL, initiator: player.playerURL, referer: player.playerURL.String(),
		header: [][2]string{{"Content-Type", "application/x-www-form-urlencoded; charset=UTF-8"}, {"Accept", "*/*"}},
		body:   []byte(form),
	})
	if err != nil {
		log.Printf("ad list: %v", err)
		return true
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var list struct {
		List []json.RawMessage `json:"list"`
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(string(body)), "{") ||
		json.Unmarshal(body, &list) != nil {
		return true
	}
	return len(list.List) == 0
}

// midrollLocked runs the midroll cue points that are due once the content
// plays, as the player's timeupdate does: of those due the highest, the
// others dropped; time 0 right away, 300 seconds once the position is
// there.
func (v *viewSession) midrollLocked(now time.Time) {
	if !v.viewStarted {
		return
	}
	switch position := v.head.at(now); {
	case position >= midrollTime && !v.midrollAt300:
		v.midrollAt300, v.midrollStarted = true, true
		v.adCalls = append(v.adCalls, v.player.adSlot(slotMidroll)...)
	case !v.midrollStarted:
		v.midrollStarted = true
		v.adCalls = append(v.adCalls, v.player.adSlot(slotStart)...)
	}
}
