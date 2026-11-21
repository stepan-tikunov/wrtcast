package music

import (
	"math"
	"sort"
)

// frame is YAMNet's look at a 0.975 s window of a video's audio starting
// at T seconds: how much it is music, speech and a song (the highest of
// the classes of songs); and the window's timbre, its MFCCs (none from
// before the music service told them).
type frame struct {
	T, Music, Speech, Song float64
	MFCC                   []float32
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
	// when Shazam says it started earlier and music is heard there under
	// the dialogue (see heardFrom).
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
// up to where the track ends. A track told goes on up to a join of songs
// (see joins) after where it was told, if an ask just after the join tells
// another track or none; until one is asked, it goes on.
func plan(frames []frame, asks []ask) ([]occurrence, []float64) {
	asks = append([]ask(nil), asks...)
	sort.Slice(asks, func(i, j int) bool { return asks[i].Start < asks[j].Start })
	cuts := joins(frames)
	var occurrences []occurrence
	var wanted []float64
	// The track told last, where it ends on the video (+Inf unknown, for
	// within its stretch only), where its music was last heard, and up to
	// where an ask told it.
	current, currentEnd, lastMusic, told := -1, 0.0, 0.0, 0.0
	// follow takes the current track on up to limit, within a stretch to
	// stop, unless an ask just after a join past where it was told tells
	// another track or none: then it ends at the join, which is answered
	// (else -1). A join not asked about yet is asked about.
	follow := func(limit, stop float64) float64 {
		o := &occurrences[current]
		for _, at := range cuts {
			if at <= told || at <= o.Start {
				continue
			}
			if at >= limit {
				break
			}
			p := probe(asks, at)
			if p < 0 {
				if t, ok := bestWindow(frames, at, math.Min(stop, at+probeReach), nil); ok {
					wanted = append(wanted, t)
				}
				break
			}
			if asks[p].Key == o.Key {
				told = math.Max(told, asks[p].Stop)
				continue
			}
			o.Stop = at
			current = -1
			return at
		}
		o.Stop = math.Max(o.Stop, limit)
		lastMusic = limit
		return -1
	}
	for _, s := range spans(frames) {
		from, joined := s.Start, false
		if current >= 0 && s.Start < currentEnd && s.Start-lastMusic <= chainGap {
			if at := follow(math.Min(s.Stop, currentEnd), s.Stop); at >= 0 {
				from, joined = at, true
			} else {
				from = currentEnd
			}
		} else {
			current = -1
		}
		for from < s.Stop && (s.Stop-from >= spanMin || joined) {
			joined = false
			var failed []ask
			found := -1
			for i, a := range asks {
				if m := a.middle(); m >= from && m < s.Stop {
					if a.Key != "" {
						found = i
						break
					}
					failed = append(failed, a)
				}
			}
			if found < 0 {
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
			a := asks[found]
			trackStart := a.Start - a.Offset
			end := math.Inf(1)
			if a.Duration > 0 {
				end = trackStart + a.Duration
			}
			start := math.Max(trackStart, heardFrom(frames, s.Start))
			if from > s.Start {
				start = math.Max(trackStart, from)
			}
			// Not before a join of songs before where it was told.
			for _, at := range cuts {
				if at > start && at <= a.Start {
					start = at
				}
			}
			start = math.Max(start, 0)
			occurrences = append(occurrences, occurrence{Key: a.Key, Start: start, Stop: start, TrackStart: trackStart})
			current, told = len(occurrences)-1, a.Stop
			if math.IsInf(end, 1) {
				// Of an unknown length, the track is this stretch's only.
				currentEnd = s.Stop
			} else {
				currentEnd = end
			}
			if at := follow(math.Min(s.Stop, end), s.Stop); at >= 0 {
				from, joined = at, true
				continue
			}
			if math.IsInf(end, 1) || end <= from {
				// Over before where it was heard: ask no further here.
				break
			}
			from = end
		}
	}
	return merged(occurrences), distinct(wanted)
}

// heardFrom is where the music heard up to a stretch starting at start
// starts: the frames before it with music, under speech too, gaps of
// gapMax at most, leadMax at most. Shazam's offset alone may say a track
// started long before any music is heard: another version of it, or an
// intro cut out.
func heardFrom(frames []frame, start float64) float64 {
	i := sort.Search(len(frames), func(i int) bool { return frames[i].T >= start })
	from, previous := start, start
	for i--; i >= 0; i-- {
		f := frames[i]
		if start-f.T > leadMax || previous-f.T > breakGap {
			break
		}
		previous = f.T
		if f.Music >= musicMin {
			if from-f.T > gapMax {
				break
			}
			from = f.T
		}
	}
	return from
}

// probe is the ask of the audio just after a join at at, -1 if none.
func probe(asks []ask, at float64) int {
	for i, a := range asks {
		if a.Start >= at-1 && a.Start < at+probeReach-askLength+1 {
			return i
		}
	}
	return -1
}

// distinct are the times in order, each once.
func distinct(times []float64) []float64 {
	sort.Float64s(times)
	var result []float64
	for _, t := range times {
		if n := len(result); n == 0 || t-result[n-1] > 0.01 {
			result = append(result, t)
		}
	}
	return result
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
