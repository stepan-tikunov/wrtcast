package music

import (
	"math"
	"math/rand"
	"testing"
)

// timbre gives frames the MFCCs of a song: around base, a little apart
// from frame to frame.
func timbre(frames []frame, base float32, seed int64) []frame {
	r := rand.New(rand.NewSource(seed))
	for i := range frames {
		frames[i].MFCC = make([]float32, mfccs)
		for d := range frames[i].MFCC {
			frames[i].MFCC[d] = base*float32(d%3+1) + float32(r.NormFloat64())*2
		}
	}
	return frames
}

func TestJoins(t *testing.T) {
	frames := concat(
		timbre(stretch(0, 30, 0.9, 0, 0.6), 10, 1),
		timbre(stretch(30, 60, 0.9, 0, 0.6), 20, 2),
	)
	got := joins(frames)
	if len(got) != 1 || math.Abs(got[0]-30) > 1 {
		t.Fatalf("joins %v, want one at 30", got)
	}
	// One song throughout: none.
	if got := joins(timbre(stretch(0, 60, 0.9, 0, 0.6), 10, 3)); len(got) != 0 {
		t.Fatalf("joins within a song %v", got)
	}
	// Another song after a dialogue: at its start.
	frames = concat(
		timbre(stretch(0, 20, 0.9, 0, 0.6), 10, 4),
		stretch(20, 30, 0.1, 0.9, 0),
		timbre(stretch(30, 50, 0.9, 0, 0.6), 20, 5),
	)
	if got := joins(frames); len(got) != 1 || got[0] != 30 {
		t.Fatalf("joins across a dialogue %v, want 30", got)
	}
}

// A song told goes on up to a join only if the ask after the join tells
// another song or none.
func TestPlanEndsATrackAtAJoin(t *testing.T) {
	frames := concat(
		timbre(stretch(0, 30, 0.9, 0, 0.6), 10, 1),
		timbre(stretch(30, 70, 0.9, 0, 0.6), 20, 2),
	)
	// Told at 2, 2 s into a song of 200 s: it would go on to the end.
	asks := []ask{{Start: 2, Stop: 10, Key: "a", Offset: 2, Duration: 200}}
	occurrences, wanted := plan(frames, asks)
	if len(occurrences) != 1 || occurrences[0].Stop < 69 {
		t.Fatalf("before the join is asked about: %v", occurrences)
	}
	if len(wanted) != 1 || wanted[0] < 30 || wanted[0] > 34 {
		t.Fatalf("wanted %v, the audio just after the join", wanted)
	}
	at := wanted[0]
	// The same song after it: one track on.
	same := append(asks, ask{Start: at, Stop: at + askLength, Key: "a", Offset: at + 0, Duration: 200})
	occurrences, wanted = plan(frames, same)
	if len(occurrences) != 1 || occurrences[0].Stop < 69 || len(wanted) != 0 {
		t.Fatalf("the same song: %v %v", occurrences, wanted)
	}
	// Another: the first ends at the join, the other starts there.
	other := append(asks, ask{Start: at, Stop: at + askLength, Key: "b", Offset: 60, Duration: 180})
	occurrences, wanted = plan(frames, other)
	if len(occurrences) != 2 || occurrences[0].Stop != 30 || occurrences[1].Key != "b" ||
		occurrences[1].Start != 30 || occurrences[1].Stop < 69 || len(wanted) != 0 {
		t.Fatalf("another song: %v %v", occurrences, wanted)
	}
	// None: the first ends at the join; the rest, 40 s, is asked about
	// once more, as any stretch so long.
	none := append(asks, ask{Start: at, Stop: at + askLength})
	occurrences, wanted = plan(frames, none)
	if len(occurrences) != 1 || occurrences[0].Stop != 30 || len(wanted) != 1 || wanted[0] < at+retryApart {
		t.Fatalf("no song: %v %v", occurrences, wanted)
	}
}

func TestADTSCut(t *testing.T) {
	// 100 frames of 1024 samples at 48 kHz, 10 bytes each.
	var data []byte
	for i := 0; i < 100; i++ {
		data = append(data, 0xff, 0xf1, 0x4c, 0x80, 0x01, 0x40, 0xfc, 0, 0, byte(i))
	}
	cut, from, seconds := adtsCut(data, 0.5, 1)
	frame := 1024.0 / 48000
	if len(cut)%10 != 0 || math.Abs(from-0.5) > frame || math.Abs(seconds-1) > frame || cut[9] != byte(math.Round(from/frame)) {
		t.Fatalf("cut %d bytes from %v for %v", len(cut), from, seconds)
	}
	if _, _, seconds := adtsCut([]byte("not adts"), 0, 1); seconds != -1 {
		t.Fatalf("not ADTS: %v", seconds)
	}
}
