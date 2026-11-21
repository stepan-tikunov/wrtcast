package music

import "sort"

// Where one song gives way to another is told by the timbre of the music
// on either side (the music service's 12 MFCCs of each window): of each of
// the 12, how far apart its means on the two sides are for how much it
// varies on them, (μ₁−μ₂)²/(σ₁²+σ₂²); the three furthest apart, averaged,
// are the change there. Measured on two episodes (2026-11-21), within a
// song ("Baby Blue", 110 s) it was 3.0 at most, at six joins of two tracks
// 4.4 to 11.6. The sides are the musical frames before and after (speech
// over the music, or a pause, is no change), so that a song after a
// dialogue is told from the one before it.
const (
	mfccs = 12
	// Musical frames on either side, within so many seconds of the change.
	changeFrames = 8
	changeReach  = 30.0
	// A change this big, the biggest of the changes within changeFrames
	// musical frames of it, is a join.
	changeMin = 4.0
	// The ask of a join is of the audio of this much after it.
	probeReach = 12.0
)

// joins are where the music of the frames, sorted by T, changes from one
// song to another, by the start of the first frame after the change.
func joins(frames []frame) []float64 {
	var music []frame
	for _, f := range frames {
		if musical(f) && len(f.MFCC) == mfccs {
			music = append(music, f)
		}
	}
	type change struct {
		i       int
		t, size float64
	}
	var changes []change
	for i := changeFrames; i+changeFrames <= len(music); i++ {
		if music[i].T-music[i-changeFrames].T > changeReach || music[i+changeFrames-1].T-music[i].T > changeReach {
			continue
		}
		changes = append(changes, change{i, music[i].T, separation(music[i-changeFrames:i], music[i:i+changeFrames])})
	}
	var result []float64
	for i, c := range changes {
		if c.size < changeMin {
			continue
		}
		biggest := true
		for j, d := range changes {
			if j != i && abs(d.i-c.i) <= changeFrames && (d.size > c.size || d.size == c.size && j < i) {
				biggest = false
				break
			}
		}
		if biggest {
			result = append(result, c.t)
		}
	}
	return result
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// separation is how far apart the timbres of two runs of frames are (see
// the constants above).
func separation(a, b []frame) float64 {
	moments := func(frames []frame, d int) (float64, float64) {
		var sum, squares float64
		for _, f := range frames {
			sum += float64(f.MFCC[d])
		}
		mean := sum / float64(len(frames))
		for _, f := range frames {
			squares += (float64(f.MFCC[d]) - mean) * (float64(f.MFCC[d]) - mean)
		}
		return mean, squares / float64(len(frames))
	}
	scores := make([]float64, mfccs)
	for d := range scores {
		ma, va := moments(a, d)
		mb, vb := moments(b, d)
		scores[d] = (ma - mb) * (ma - mb) / (va + vb + 1e-3)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(scores)))
	return (scores[0] + scores[1] + scores[2]) / 3
}
