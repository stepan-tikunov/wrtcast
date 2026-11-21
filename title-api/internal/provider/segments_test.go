package provider

import (
	"context"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The relay keeps the segments it passes on and loads those ahead of the
// receiver: the receiver's next one comes from the disk, not the CDN, and
// each whole segment is told once.
func TestRelayKeepsAndLoadsAhead(t *testing.T) {
	t.Parallel()
	fake := newFakeAlternativePlayer(t, true, 1, "")
	client, err := NewClient(Config{BaseURL: "https://example.com", MediaBaseURL: "https://api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	client.telemetryTiming.adBlock = 10 * time.Millisecond
	t.Cleanup(client.Close)
	var mu sync.Mutex
	var told []Segment
	if err := client.KeepSegments(SegmentOptions{Dir: t.TempDir(), Limit: 1 << 20, Ahead: 60, Sink: func(s Segment) {
		mu.Lock()
		told = append(told, s)
		mu.Unlock()
	}}); err != nil {
		t.Fatal(err)
	}
	id := alternativeMasterID(t, client, fake)
	ctx := context.Background()
	if _, err := client.MediaHeaders(ctx, id, PlaybackReport{Resolution: "720"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.QualityPlaylist(ctx, id, "720"); err != nil {
		t.Fatal(err)
	}
	get := func(name string) int {
		response, err := client.RelayMedia(ctx, id, fake.cdn.URL+"/"+name, false)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s: %+v, %v", name, response, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		return len(body)
	}
	if n := get("seg-0.ts"); n != 50000 {
		t.Fatalf("seg-0: %d bytes", n)
	}
	waitFor(t, "seg-1 loaded ahead", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(told) == 2
	})
	_, _, before := fake.counts()
	if n := get("seg-1.ts"); n != 50000 {
		t.Fatalf("seg-1: %d bytes", n)
	}
	if _, _, after := fake.counts(); after != before {
		t.Fatalf("seg-1 was asked of the CDN again")
	}
	mu.Lock()
	defer mu.Unlock()
	// The one loaded ahead may be over first.
	sort.Slice(told, func(i, j int) bool { return told[i].Start < told[j].Start })
	if told[0].Start != 0 || told[0].End != 10 || told[1].Start != 10 || told[1].Video.Title != "title" ||
		len(told[1].Data) != 50000 || strings.TrimSpace(told[1].Video.Source) == "" {
		t.Fatalf("told %+v", []any{told[0].Video, told[0].Start, told[1].Start, told[1].End})
	}
	entry, _ := client.masters.entry(id)
	if _, _, loads := entry.source.media.stats(); loads != 2 {
		t.Fatalf("%d loads counted", loads)
	}
}
