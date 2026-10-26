package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// The router's CDN requests go through title-api: the media's fragments
// stream through and feed the bandwidth estimate; a block is passed on,
// reported to /errors as fatal, and answered from then on without asking
// the CDN.
func TestMediaRelay(t *testing.T) {
	t.Parallel()
	fake := newFakeAlternativePlayer(t, true, 1, "")
	client, err := NewClient(Config{BaseURL: "https://example.com", MediaBaseURL: "https://api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	client.telemetryTiming.adBlock = 10 * time.Millisecond
	t.Cleanup(client.Close)
	id := alternativeMasterID(t, client, fake)
	ctx := context.Background()
	if _, err := client.MediaHeaders(ctx, id, PlaybackReport{Resolution: "720"}); err != nil {
		t.Fatal(err)
	}
	playlist, err := client.QualityPlaylist(ctx, id, "720")
	if err != nil || !strings.Contains(string(playlist), fake.cdn.URL+"/seg-0.ts") {
		t.Fatalf("playlist %q, %v", playlist, err)
	}
	response, err := client.FetchMedia(ctx, id, fake.cdn.URL+"/seg-0.ts")
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("fragment %+v, %v", response, err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	entry, _ := client.masters.entry(id)
	estimate, loadedEnd, loads := entry.source.media.stats()
	if len(body) != 50000 || estimate <= 0 || loadedEnd != 10 || loads != 1 {
		t.Fatalf("%d bytes; estimate %v, loaded to %v, %d loads", len(body), estimate, loadedEnd, loads)
	}

	fake.mu.Lock()
	fake.segments = "client_blocked"
	fake.mu.Unlock()
	response, err = client.FetchMedia(ctx, id, fake.cdn.URL+"/seg-1.ts")
	if err != nil || response.StatusCode != http.StatusForbidden || response.Header.Get("X-VD") != "client_blocked" {
		t.Fatalf("blocked fragment %+v, %v", response, err)
	}
	response.Body.Close()
	_, _, before := fake.counts()
	if _, err := client.FetchMedia(ctx, id, fake.cdn.URL+"/seg-1.ts"); !errors.Is(err, ErrCDNBlocked) && MediaStatusOf(err) != http.StatusForbidden {
		t.Fatalf("after the block: %v", err)
	}
	if _, _, after := fake.counts(); after != before {
		t.Fatalf("the CDN was asked again after it blocked the client")
	}
	waitFor(t, "/errors", func() bool { return len(fake.telemetryOf("/errors")) == 1 })
	record := fake.telemetryOf("/errors")[0]
	form, _ := url.ParseQuery(record.body)
	if payload := form.Get("payload"); !strings.Contains(payload, `"details":"fragLoadError","fatal":true,"parent":"main","url":"`+fake.cdn.URL+`/seg-1.ts"`) ||
		!strings.Contains(payload, `"httpCode":403`) || !strings.Contains(payload, `"fragSn":1,"fragStart":10,"fragDuration":10,"fragLevel":0`) ||
		record.header.Get("Sec-Fetch-Mode") != "cors" {
		t.Fatalf("/errors %s %v", payload, record.header)
	}
}

func MediaStatusOf(err error) int {
	status, _ := MediaStatus(err)
	return status
}

// The preroll asks the ad network for its lists as the page's jQuery
// does, the second only after an empty first; no VAST follows. A list
// with an ad ends the slot too.
func TestAdLists(t *testing.T) {
	var mu sync.Mutex
	var forms []url.Values
	var headers []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		form, _ := url.ParseQuery(string(body))
		mu.Lock()
		defer mu.Unlock()
		forms, headers = append(forms, form), append(headers, request.Header.Clone())
		if request.Method != http.MethodPost || !strings.HasPrefix(string(body), "token=tok&url=https%253A%252F%252Fsite.example%252F&type=3&count=") {
			t.Errorf("%s %q", request.Method, body)
		}
		if form.Get("bg") == "1" {
			_, _ = io.WriteString(response, `{"list":[{"id":1,"url":"https://vast.example/tag","info":"x","stat":"y"}]}`)
		}
	}))
	defer server.Close()
	previous := adListURL
	adListURL = server.URL + "/lists.php"
	defer func() { adListURL = previous }()

	view, source, t0 := newTestView(t)
	player := view.player
	player.browser, player.fingerprint, player.movieID = view.client.browser, "fp", "abc"
	player.ads = alternativeAds{Enabled: true, Preroll: "jsf:rotateP", Replace: map[string]json.RawMessage{}}
	player.ads.Midroll = append(player.ads.Midroll, struct {
		Link string `json:"link"`
	}{Link: "jsf:rotatePM"})
	for key, value := range map[string]string{
		"[token]": `"tok"`, "[domain]": `"https%3A%2F%2Fsite.example%2F"`, "[type]": "3", "[kp]": "277537",
		"[cat]": `"serial"`, "[movie]": "0", "[is_trailer]": "false", "[count_preroll]": "2",
	} {
		player.ads.Replace[key] = json.RawMessage(value)
	}
	view.mu.Lock()
	view.leaseUntil = at(t0, 600000)
	view.beginLocked(at(t0, 50500), source, PlaybackReport{})
	calls := view.adCalls
	view.adCalls = nil
	view.mu.Unlock()
	view.requestAds(context.Background(), calls)
	view.mu.Lock()
	pending, due := view.prerollPending, view.playDue
	view.firstFrameLocked(at(t0, 52000))
	calls = view.adCalls
	view.adCalls = nil
	view.mu.Unlock()
	view.requestAds(context.Background(), calls)

	mu.Lock()
	defer mu.Unlock()
	zone := browserTimeZone()
	if pending || due.IsZero() || len(forms) != 3 || forms[0].Get("count") != "1" || forms[1].Get("module") != "2" ||
		forms[2].Get("bg") != "1" || forms[0].Get("up") != "fp" || forms[0].Get("mt") != "abc" || forms[0].Get("lang") != "ru-RU" ||
		forms[0].Get("tz") != zone.name || forms[0].Get("trailer") != "0" || forms[0].Get("kp") != "277537" {
		t.Fatalf("preroll pending %t, play %v; lists %v", pending, due, forms)
	}
	if h := headers[0]; h.Get("Origin") != "https://player.example" || h.Get("X-Requested-With") != "" ||
		h.Get("Sec-Fetch-Site") != "cross-site" || h.Get("Cookie") != "" || h.Get("Referer") != player.playerURL.String() {
		t.Fatalf("list request headers %v", h)
	}
}

// The receiver waiting for data once the view started is a rebuffer.
func TestViewRebuffer(t *testing.T) {
	t.Parallel()
	view, source, t0 := newTestView(t)
	view.mu.Lock()
	defer view.mu.Unlock()
	view.leaseUntil = at(t0, 600000)
	view.beginLocked(at(t0, 50500), source, PlaybackReport{Resolution: "720"})
	view.stepLocked(context.Background(), at(t0, 51100))
	played := PlaybackReport{Resolution: "720", Duration: 2640, Position: 0, HasPosition: true, HasPaused: true}
	view.reportLocked(at(t0, 52000), source, played)
	waiting := played
	waiting.Position, waiting.Buffering, waiting.HasBuffering = 8, true, true
	view.reportLocked(at(t0, 60000), source, waiting)
	view.heartbeatLocked(at(t0, 61000))
	playing := waiting
	playing.Buffering = false
	view.reportLocked(at(t0, 62500), source, playing)
	view.outbox = nil
	view.flushLocked(at(t0, 63000), false)
	_, events := eventsOf(t, view.takeOutbox()[0])
	var start, end, beat map[string]any
	for _, event := range events {
		switch event["event_type"] {
		case "rebuffer_start":
			start = event
		case "rebuffer_end":
			end = event
		case "heartbeat":
			beat = event
		}
	}
	if start == nil || end == nil || end["rebufferDurationMs"] != 2500.0 || end["rebufferCountTotal"] != 1.0 ||
		end["rebufferTotalMsSession"] != 2500.0 || beat["isBuffering"] != true {
		t.Fatalf("events %v", events)
	}
}
