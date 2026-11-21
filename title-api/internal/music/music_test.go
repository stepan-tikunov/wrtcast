package music

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// stretch is frames every half second from from to to: music, speech and
// song as given.
func stretch(from, to, music, speech, song float64) []frame {
	var result []frame
	for t := from; t < to-0.001; t += 0.5 {
		result = append(result, frame{T: t, Music: music, Speech: speech, Song: song})
	}
	return result
}

func concat(parts ...[]frame) []frame {
	var result []frame
	for _, p := range parts {
		result = append(result, p...)
	}
	return result
}

func TestSpans(t *testing.T) {
	frames := concat(
		stretch(0, 10, 0.1, 0.9, 0),    // speech
		stretch(10, 30, 0.8, 0.1, 0),   // music alone
		stretch(30, 31, 0.1, 0.8, 0),   // a word: within the gap
		stretch(31, 40, 0.8, 0.1, 0),   // the same music
		stretch(40, 60, 0.9, 0.7, 0),   // score under speech: not
		stretch(60, 75, 0.8, 0.7, 0.5), // a song under speech
		stretch(75, 80, 0.8, 0.1, 0),   // the song on, alone
		stretch(90, 95, 0.8, 0.1, 0),   // too short
	)
	got := spans(frames)
	want := []span{{10, 39.5 + window}, {60, 79.5 + window}}
	if len(got) != len(want) {
		t.Fatalf("spans %v, want %v", got, want)
	}
	for i := range want {
		if math.Abs(got[i].Start-want[i].Start) > 1e-9 || math.Abs(got[i].Stop-want[i].Stop) > 1e-9 {
			t.Fatalf("spans %v, want %v", got, want)
		}
	}
	// No audio between: two stretches, neither long enough.
	broken := concat(stretch(0, 6, 0.8, 0, 0), stretch(10, 16, 0.8, 0, 0))
	if got := spans(broken); len(got) != 0 {
		t.Fatalf("spans across a break: %v", got)
	}
}

func TestPlanAsksEachStretchOnce(t *testing.T) {
	frames := concat(stretch(0, 20, 0.1, 0.9, 0), stretch(20, 50, 0.8, 0.1, 0.5), stretch(50, 80, 0.1, 0.9, 0))
	occurrences, wanted := plan(frames, nil)
	if len(occurrences) != 0 || len(wanted) != 1 || wanted[0] < 20 || wanted[0]+askLength > 50+window {
		t.Fatalf("plan: %v %v", occurrences, wanted)
	}
	// Found nothing: a long stretch is asked again, away from the first.
	first := ask{Start: wanted[0] - 2, Stop: wanted[0] + 10}
	_, wanted = plan(frames, []ask{first})
	if len(wanted) != 1 || math.Abs(wanted[0]-first.Start) < retryApart {
		t.Fatalf("second ask: %v after %v", wanted, first)
	}
	second := ask{Start: wanted[0], Stop: wanted[0] + 8}
	if _, wanted = plan(frames, []ask{first, second}); len(wanted) != 0 {
		t.Fatalf("a third ask: %v", wanted)
	}
	// A short stretch is asked once.
	short := concat(stretch(0, 12, 0.8, 0, 0), stretch(12, 30, 0.1, 0.9, 0))
	if _, wanted = plan(short, []ask{{Start: 0, Stop: 8}}); len(wanted) != 0 {
		t.Fatalf("a short stretch asked again: %v", wanted)
	}
}

func TestPlanFollowsATrackAcrossADialogue(t *testing.T) {
	frames := concat(
		stretch(100, 130, 0.8, 0.1, 0.6), // the song
		stretch(130, 150, 0.2, 0.9, 0),   // a dialogue over it
		stretch(150, 170, 0.8, 0.1, 0.6), // the song again
		stretch(170, 200, 0.2, 0.9, 0),
		stretch(200, 240, 0.8, 0.1, 0.6), // past the song's end at 220: another
	)
	// Told at 104, 34 s into a song of 154 s: it started at 70, ends at 224.
	asks := []ask{{Start: 104, Stop: 116, Key: "song", Offset: 34, Duration: 154}}
	occurrences, wanted := plan(frames, asks)
	if len(occurrences) != 1 || occurrences[0].Key != "song" || occurrences[0].TrackStart != 70 {
		t.Fatalf("occurrences %v", occurrences)
	}
	o := occurrences[0]
	if o.Start != 70 || math.Abs(o.Stop-224) > 1e-9 {
		t.Fatalf("the song heard %v–%v, want 70–224", o.Start, o.Stop)
	}
	// Only what follows the song's end is asked about.
	if len(wanted) != 1 || wanted[0] < 224 {
		t.Fatalf("wanted %v", wanted)
	}
	asks = append(asks, ask{Start: 224, Stop: 236, Key: "next", Offset: 1, Duration: 0})
	occurrences, wanted = plan(frames, asks)
	if len(occurrences) != 2 || occurrences[1].Key != "next" || len(wanted) != 0 {
		t.Fatalf("after the next one: %v %v", occurrences, wanted)
	}
	if occurrences[1].Start != 224 || math.Abs(occurrences[1].Stop-(239.5+window)) > 1e-9 {
		t.Fatalf("the next one heard %v–%v", occurrences[1].Start, occurrences[1].Stop)
	}
}

func TestPlanAsksAgainOfATrackOfUnknownLength(t *testing.T) {
	frames := concat(stretch(0, 20, 0.8, 0, 0.6), stretch(20, 40, 0.2, 0.9, 0), stretch(40, 60, 0.8, 0, 0.6))
	asks := []ask{{Start: 2, Stop: 14, Key: "song", Offset: 10}}
	occurrences, wanted := plan(frames, asks)
	if len(occurrences) != 1 || len(wanted) != 1 || wanted[0] < 40 {
		t.Fatalf("plan: %v %v", occurrences, wanted)
	}
	// The same song again: one occurrence.
	asks = append(asks, ask{Start: 42, Stop: 54, Key: "song", Offset: 50})
	occurrences, wanted = plan(frames, asks)
	if len(occurrences) != 1 || len(wanted) != 0 || occurrences[0].Start != 0 {
		t.Fatalf("merged: %v %v", occurrences, wanted)
	}
}

// fakeMusic is a music service: audio "M" is music, anything else
// speech; Shazam tells what recognize says, or is over its limit.
type fakeMusic struct {
	mu         sync.Mutex
	classified [][]byte
	asked      [][]byte
	limited    bool
	recognize  *match
}

func (f *fakeMusic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/v1/classify":
		f.classified = append(f.classified, body)
		var frames [][4]float64
		for t := 0.0; t < 3.5; t += 0.5 {
			if strings.HasPrefix(string(body), "M") {
				frames = append(frames, [4]float64{t, 0.9, 0.05, 0.5})
			} else {
				frames = append(frames, [4]float64{t, 0.05, 0.9, 0})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"frames": frames})
	case "/v1/recognize":
		f.asked = append(f.asked, body)
		if f.limited {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "limited", "retry_after": 120})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"match": f.recognize})
	}
}

func newTestService(t *testing.T, fake *fakeMusic) *Service {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	service, err := New(context.Background(), db, server.URL, filepath.Join(t.TempDir(), "music"))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestServiceTellsTheMusicOfAVideo(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMusic{recognize: &match{Key: "1", Title: "Song", Artist: "Band", Offset: 30, Duration: 200}}
	service := newTestService(t, fake)
	v := Video{Title: "t1", Season: 1, Episode: 2, Source: "alternative"}
	// 0–20 s speech, 20–40 s music, in 4 s segments.
	for start := 0.0; start < 40; start += 4 {
		audio := "S speech"
		if start >= 20 {
			audio = "M music " + time.Duration(start).String()
		}
		if err := service.Audio(ctx, v, start, start+4, []byte(audio)); err != nil {
			t.Fatal(err)
		}
	}
	// Again: not looked at twice.
	if err := service.Audio(ctx, v, 20, 24, []byte("S")); err != nil {
		t.Fatal(err)
	}
	result, err := service.Tracks(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tracks) != 0 || result.Pending != 1 || result.Received != 40 {
		t.Fatalf("before asking: %+v", result)
	}
	// The speech's audio is not kept, the music's is.
	files, _, _ := service.store.files(ctx)
	if len(files) != 5 {
		t.Fatalf("%d files kept", len(files))
	}
	if pause := service.next(ctx); pause != 0 {
		t.Fatalf("ask: pause %v", pause)
	}
	if len(fake.asked) != 1 || !strings.HasPrefix(string(fake.asked[0]), "M music") {
		t.Fatalf("asked %q", fake.asked)
	}
	if pause := service.next(ctx); pause != time.Hour {
		t.Fatalf("nothing more to ask: pause %v", pause)
	}
	result, err = service.Tracks(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tracks) != 1 || result.Pending != 0 {
		t.Fatalf("after asking: %+v", result)
	}
	track := result.Tracks[0]
	if track.Title != "Song" || track.Artist != "Band" || track.Duration != 200 {
		t.Fatalf("track %+v", track)
	}
	// Told 30 s in at the ask's start: it started before the stretch, which
	// it is said to start at most 30 s before.
	ask, _ := service.store.asks(ctx, v.key())
	if track.Start != round(math.Max(math.Max(ask[0].Start-30, 20-leadMax), 0)) || math.Abs(track.Offset-(track.Start-(ask[0].Start-30))) > 0.1 {
		t.Fatalf("track %+v of ask %+v", track, ask[0])
	}
	// Forgotten audio: an ask finds none and is kept as finding nothing.
	other := Video{Title: "t2", Source: "alternative"}
	for start := 0.0; start < 20; start += 4 {
		if err := service.Audio(ctx, other, start, start+4, []byte("M")); err != nil {
			t.Fatal(err)
		}
	}
	files, _, _ = service.store.files(ctx)
	for _, file := range files {
		if strings.Contains(file, digest(other.key())) {
			_ = os.Remove(file)
		}
	}
	if pause := service.next(ctx); pause != 0 || len(fake.asked) != 1 {
		t.Fatalf("without audio: pause %v, asked %d", pause, len(fake.asked))
	}
	if asks, _ := service.store.asks(ctx, other.key()); len(asks) != 1 || asks[0].Key != "" {
		t.Fatalf("asks %+v", asks)
	}
}

func TestServiceWaitsWhenShazamIsLimited(t *testing.T) {
	ctx := context.Background()
	fake := &fakeMusic{limited: true}
	service := newTestService(t, fake)
	v := Video{Title: "t1", Source: "primary"}
	for start := 0.0; start < 12; start += 4 {
		if err := service.Audio(ctx, v, start, start+4, []byte("M")); err != nil {
			t.Fatal(err)
		}
	}
	if pause := service.next(ctx); pause != 2*time.Minute {
		t.Fatalf("pause %v", pause)
	}
	if asks, _ := service.store.asks(ctx, v.key()); len(asks) != 0 {
		t.Fatalf("an ask kept while limited: %+v", asks)
	}
	// The audio is trimmed past its age.
	if _, err := service.store.db.ExecContext(ctx, `UPDATE music_audio SET at = at - 100000`); err != nil {
		t.Fatal(err)
	}
	service.trim(ctx)
	if files, _, _ := service.store.files(ctx); len(files) != 0 {
		t.Fatalf("files left %v", files)
	}
}

// A segment of the alternative player (fMP4, video and audio) has its
// audio cut out as it is, ADTS.
func TestSegmentCutsTheAudio(t *testing.T) {
	path := ffmpegPath()
	if path == "" {
		t.Skip("no ffmpeg")
	}
	file := filepath.Join(t.TempDir(), "s.mp4")
	if out, err := exec.Command(path, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=64x36:rate=10:duration=4",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=4", "-c:v", "libx264", "-c:a", "aac",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof", file).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot make a segment: %v %s", err, out)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeMusic{}
	service := newTestService(t, fake)
	if err := service.Segment(context.Background(), Video{Title: "t", Source: SourceOf("x")}, 8, 12, nil, data); err != nil {
		t.Fatal(err)
	}
	if len(fake.classified) != 1 || len(fake.classified[0]) < 1000 || fake.classified[0][0] != 0xff {
		t.Fatalf("classified %d", len(fake.classified))
	}
	if _, from, seconds := adtsCut(fake.classified[0], 0, 100); from != 0 || math.Abs(seconds-4) > 0.1 {
		t.Fatalf("audio of %v s", seconds)
	}
}

// The source of a stream is its name hashed as the router hashes it.
func TestSourceOf(t *testing.T) {
	if got := SourceOf("abc"); got != "1a47e90b" {
		t.Fatalf("SourceOf(abc) = %s", got)
	}
	if EpisodeOf("12a") != 12 || EpisodeOf("") != 0 {
		t.Fatal("EpisodeOf")
	}
}
