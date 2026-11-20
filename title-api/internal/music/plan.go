package music

import (
	"math"
	"sort"
)

// frame is YAMNet's look at a 0.975 s window of a video's audio starting
// at T seconds: how much it is music, speech and a song (the highest of
// the classes of songs).
type frame struct {
	T, Music, Speech, Song float64
}

// What a stretch of music is, measured on two episodes of Russian sitcoms
// and three scenes of songs (2026-11-21): music under speech is mostly
// background score, which Shazam rarely tells under a dialogue; songs are
// told under it too. Of the stretches these thresholds picked Shazam told
// 44 in 49.
const (
	musicMin  = 0.5
	speechMax = 0.3
	songMin   = 0.3
	// Seconds of a window, and between the windows' starts.
	window = 0.975
	// Music frames this far apart are of one stretch; frames this far apart
	// have no audio received between them.
	gapMax   = 2.5
	breakGap = 2.5
	// A stretch shorter than this is not asked about.
	spanMin = 8.0
	// Seconds of audio Shazam is asked about.
	askLength = 8.0
	// A stretch this long is asked about again, at least retryApart from
	// the first, when the first ask finds nothing.
	retrySpan  = 30.0
	retryApart = 20.0
	// A later stretch is the same track's if it starts within the track's
	// length and this soon after the music of it: a song cut by a dialogue.
	chainGap = 60.0
	// How far before the stretch it was heard in a track is said to start,
	// when Shazam says it started earlier, under the dialogue.
	leadMax = 30.0
)

func musical(f frame) bool {
	return f.Music >= musicMin && (f.Speech <= speechMax || f.Song >= songMin)
}

// span is a stretch of music, from the start of its first music window to
// the end of its last.
type span struct {
	Start, Stop float64
}

// spans are the stretches of music of the frames, sorted by T, of spanMin
// or longer.
func spans(frames []frame) []span {
	var result []span
	var current *span
	lastMusic, previous := 0.0, math.Inf(-1)
	closeSpan := func() {
		if current != nil && current.Stop-current.Start >= spanMin {
			result = append(result, *current)
		}
		current = nil
	}
	for _, f := range frames {
		if f.T-previous > breakGap {
			closeSpan()
		}
		previous = f.T
		if !musical(f) {
			continue
		}
		if current != nil && f.T-lastMusic <= gapMax {
			current.Stop = f.T + window
		} else {
			closeSpan()
			current = &span{Start: f.T, Stop: f.T + window}
		}
		lastMusic = f.T
	}
	closeSpan()
	return result
}

// ask is Shazam asked about the audio from Start to Stop of a video: the
// track it told (Key, "" for none or no audio to ask about), Offset seconds
// into it at Start, and its Duration (0 unknown).
type ask struct {
	Start, Stop      float64
	Key              string
	Offset, Duration float64
}

func (a ask) middle() float64 { return (a.Start + a.Stop) / 2 }

// occurrence is a track heard from Start to Stop of a video; TrackStart is
// where the track would have started on the video.
type occurrence struct {
	Key                     string
	Start, Stop, TrackStart float64
}

// plan tells the tracks heard in a video by the asks made of its frames'
// stretches of music, and the starts of the windows to ask about next, in
// the order of the video. A stretch is asked about once, twice if long and
// the first finds nothing; one heard within the length of a track told
// before it, soon after its music, is that track's and not asked about,
// up to where the track ends.
func plan(frames []frame, asks []ask) ([]occurrence, []float64) {
	asks = append([]ask(nil), asks...)
	sort.Slice(asks, func(i, j int) bool { return asks[i].Start < asks[j].Start })
	var occurrences []occurrence
	var wanted []float64
	// The track told last, where it ends on the video (+Inf unknown, for
	// within its stretch only), and where its music was last heard.
	current, currentEnd, lastMusic := -1, 0.0, 0.0
	for _, s := range spans(frames) {
		from := s.Start
		if current >= 0 && s.Start < currentEnd && s.Start-lastMusic <= chainGap {
			o := &occurrences[current]
			o.Stop = math.Min(s.Stop, currentEnd)
			lastMusic = o.Stop
			from = currentEnd
		} else {
			current = -1
		}
		for from < s.Stop && s.Stop-from >= spanMin {
			var failed []ask
			told := -1
			for i, a := range asks {
				if m := a.middle(); m >= from && m < s.Stop {
					if a.Key != "" {
						told = i
						break
					}
					failed = append(failed, a)
				}
			}
			if told < 0 {
				switch {
				case len(failed) == 0:
					if t, ok := bestWindow(frames, from, s.Stop, nil); ok {
						wanted = append(wanted, t)
					}
				case len(failed) == 1 && s.Stop-from >= retrySpan:
					if t, ok := bestWindow(frames, from, s.Stop, &failed[0]); ok {
						wanted = append(wanted, t)
					}
				}
				break
			}
			a := asks[told]
			trackStart := a.Start - a.Offset
			end := math.Inf(1)
			if a.Duration > 0 {
				end = trackStart + a.Duration
			}
			start := math.Max(trackStart, s.Start-leadMax)
			if from > s.Start {
				start = math.Max(trackStart, from)
			}
			start = math.Max(start, 0)
			occurrences = append(occurrences, occurrence{
				Key: a.Key, Start: start, Stop: math.Min(s.Stop, end), TrackStart: trackStart,
			})
			current, lastMusic = len(occurrences)-1, math.Min(s.Stop, end)
			if math.IsInf(end, 1) {
				// Of an unknown length, the track is this stretch's only.
				currentEnd = s.Stop
				break
			}
			currentEnd = end
			if end <= from {
				// Told, but over before where it was heard: ask no further here.
				break
			}
			from = end
		}
	}
	return merged(occurrences), wanted
}

// merged joins a track's occurrences that follow one another.
func merged(occurrences []occurrence) []occurrence {
	var result []occurrence
	for _, o := range occurrences {
		if n := len(result); n > 0 && result[n-1].Key == o.Key && o.Start-result[n-1].Stop <= chainGap {
			result[n-1].Stop = math.Max(result[n-1].Stop, o.Stop)
			continue
		}
		result = append(result, o)
	}
	return result
}

// bestWindow is the start of the askLength seconds within from and to with
// the most music and song and the least speech, its frames all there; at
// least retryApart from the ask that found nothing, if any.
func bestWindow(frames []frame, from, to float64, not *ask) (float64, bool) {
	best, at, found := math.Inf(-1), 0.0, false
	need := int(math.Round(askLength / 0.5 * 0.8))
	for i, f := range frames {
		if f.T < from || f.T+askLength > to+0.01 {
			continue
		}
		if not != nil && math.Abs(f.T-not.Start) < retryApart {
			continue
		}
		var sum float64
		n := 0
		for _, g := range frames[i:] {
			if g.T >= f.T+askLength {
				break
			}
			sum += g.Music + g.Song - g.Speech
			n++
		}
		if n >= need && sum/float64(n) > best {
			best, at, found = sum/float64(n), f.T, true
		}
	}
	return at, found
}
