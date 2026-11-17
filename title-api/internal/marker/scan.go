package marker

import (
	"context"
	"fmt"
	"math"
	"sync"
)

const (
	// The head and the tail of a video are scanned this far at most: the
	// recap and the intro within the first, the credits within the last.
	HeadSpan = 480.0
	TailSpan = 360.0
	// A scan shows at most maxFrames frames, at least scanStep apart.
	scanStep = 2.0
)

// Span is a part as a scan found it: the numbers of its first frame and of
// the first frame after it (0 when it runs past the last frame shown).
type Span struct {
	Present bool `json:"present"`
	First   int  `json:"first"`
	After   int  `json:"after"`
}

// HeadScan is the beginning of the video as its frames show it.
type HeadScan struct {
	Recap Span   `json:"recap"`
	Intro Span   `json:"intro"`
	Notes string `json:"notes"`
}

// TailScan is the end of the video as its frames show it.
type TailScan struct {
	Credits Span `json:"credits"`
	// Whether a scene or dialogue follows the credits.
	MoreAfter bool   `json:"more_after"`
	Notes     string `json:"notes"`
}

const headSystem = `You mark the beginning of a TV episode on frames of it shown every few seconds, each preceded by its number and time, with the subtitles spoken there:

- recap: a montage of scenes from earlier episodes, often opened by a card or a voice saying "Previously on …". When present, it starts at the very beginning of the video.
- cold open: a scene of the new episode before the intro. It is neither recap nor intro.
- intro: the opening title sequence. It begins as soon as the recap or the cold open ends, together with whatever leads into it (black frames, a network or studio card such as "… presents"), and ends after its last title card. The first frame after it is the episode's story going on.

For each part, say whether it is there, the number of its first frame and the number of the first frame after it (0 if it runs past the last frame shown). Notes are one or two short sentences on what you saw.`

const tailSystem = `You mark the end credits of a TV episode on frames of its end shown every few seconds, each preceded by its number and time, with the subtitles spoken there.

The credits begin where the episode's story ends: at the cut or the fade to black after its last shot when black frames lead into the credit cards, or else with the first credit text over the picture. They end after the last credit.

Say whether the credits are there, the number of their first frame and the number of the first frame after them (0 if they run past the last frame shown), and whether a scene or dialogue of the episode follows them (not counting a preview of the next episode). Notes are one or two short sentences on what you saw.`

var spanSchema = object(map[string]any{
	"present": typed("boolean", "Whether the part is among the frames"),
	"first":   typed("integer", "Number of its first frame; 0 if not present"),
	"after":   typed("integer", "Number of the first frame after it; 0 if it runs past the last frame or is not present"),
})

var headSchema = object(map[string]any{
	"recap": spanSchema,
	"intro": spanSchema,
	"notes": typed("string", "What was seen, briefly"),
})

var tailSchema = object(map[string]any{
	"credits":    spanSchema,
	"more_after": typed("boolean", "Whether a scene or dialogue follows the credits"),
	"notes":      typed("string", "What was seen, briefly"),
})

// Result is what Mark found, and how.
type Result struct {
	Rough     Rough             `json:"rough"`
	HeadRange [2]float64        `json:"headRange"`
	Head      HeadScan          `json:"head"`
	TailRange [2]float64        `json:"tailRange"`
	Tail      TailScan          `json:"tail"`
	Near      []Boundary        `json:"near"`
	Exact     map[string]Exact  `json:"exact"`
	Errors    map[string]string `json:"errors,omitempty"`
	// The boundaries in the end, by name: recap_end, intro_start,
	// intro_end, credits_start, credits_end; and whether the credits may be
	// skipped.
	Times     map[string]float64 `json:"times"`
	Skippable bool               `json:"skippable"`
}

// Mark finds the recap, the intro and the credits of a video of duration
// seconds: roughly from its cues, then on frames of its head and its tail
// every few seconds, then each boundary on frames an eighth of a second
// apart.
func (m *Marker) Mark(ctx context.Context, video Video, cues []Cue, duration float64) (Result, error) {
	result := Result{Exact: map[string]Exact{}, Errors: map[string]string{}, Times: map[string]float64{}}
	rough, err := m.Rough(ctx, cues, duration)
	if err != nil {
		return result, fmt.Errorf("rough: %w", err)
	}
	result.Rough = rough
	result.HeadRange = [2]float64{0, headEnd(rough, cues, duration)}
	result.TailRange = [2]float64{tailStart(rough, cues, duration), duration}

	var headFrames, tailFrames []Frame
	var headErr, tailErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		headFrames, headErr = m.scan(ctx, video, cues, result.HeadRange, headSystem, headSchema, &result.Head)
	}()
	go func() {
		defer wg.Done()
		tailFrames, tailErr = m.scan(ctx, video, cues, result.TailRange, tailSystem, tailSchema, &result.Tail)
	}()
	wg.Wait()
	if headErr != nil {
		result.Errors["head"] = headErr.Error()
	}
	if tailErr != nil {
		result.Errors["tail"] = tailErr.Error()
	}

	head, tail := result.Head, result.Tail
	joined := head.Recap.Present && head.Intro.Present && head.Recap.After == head.Intro.First
	if headErr == nil {
		if head.Recap.Present {
			what := "where the recap (the montage of earlier episodes) ends: the first frame that is no longer part of it"
			if joined {
				what = "where the recap ends and what leads into the opening title sequence begins (black frames, a network card, the titles themselves)"
			}
			result.addNear("recap_end", what, headFrames, head.Recap.After)
		}
		if head.Intro.Present && !joined {
			result.addNear("intro_start", "where the opening title sequence begins, with whatever leads into it (black frames, a network card): its first frame", headFrames, head.Intro.First)
		}
		if head.Intro.Present {
			result.addNear("intro_end", "where the opening title sequence ends: the first frame after its last title card, where the episode goes on", headFrames, head.Intro.After)
		}
	}
	if tailErr == nil && tail.Credits.Present {
		result.addNear("credits_start", "where the end credits begin: the first black frame after the story's last shot when black leads into the credit cards, or else the first frame with credit text over the picture", tailFrames, tail.Credits.First)
		if tail.Credits.After == 0 {
			result.Times["credits_end"] = duration
		} else {
			result.addNear("credits_end", "where the end credits end: the first frame after the last credit", tailFrames, tail.Credits.After)
		}
		result.Skippable = !tail.MoreAfter
	}

	known := fmt.Sprintf("The video is %.1f s long. %s %s", duration, head.Notes, tail.Notes)
	var mu sync.Mutex
	for _, boundary := range result.Near {
		wg.Add(1)
		go func() {
			defer wg.Done()
			exact, err := m.Refine(ctx, video, cues, boundary, known)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				result.Errors[boundary.Name] = err.Error()
				result.Times[boundary.Name] = boundary.Near
				return
			}
			result.Exact[boundary.Name] = exact
			result.Times[boundary.Name] = exact.Time
		}()
	}
	wg.Wait()
	if joined {
		if t, ok := result.Times["recap_end"]; ok {
			result.Times["intro_start"] = t
		}
	}
	return result, nil
}

// addNear adds the boundary at the frame of number at (from 1) of a scan:
// between it and the frame before.
func (r *Result) addNear(name, what string, frames []Frame, at int) {
	if at < 1 || at > len(frames) {
		r.Errors[name] = fmt.Sprintf("frame %d of %d", at, len(frames))
		return
	}
	near, spread := frames[at-1].Time, scanStep
	if at > 1 {
		before := frames[at-2].Time
		near, spread = (near+before)/2, frames[at-1].Time-before
	}
	r.Near = append(r.Near, Boundary{Name: name, What: what, Near: near, Spread: spread})
}

// scan shows the model the frames over span, at most maxFrames of them,
// and reads its answer into result.
func (m *Marker) scan(ctx context.Context, video Video, cues []Cue, span [2]float64, system string, schema map[string]any, result any) ([]Frame, error) {
	decoded, err := video.Decode(ctx, span[0], span[1])
	if err != nil {
		return nil, err
	}
	step := math.Max(scanStep, math.Ceil((span[1]-span[0])/maxFrames*2)/2)
	frames := sample(decoded, span[0], span[1], step)
	text := fmt.Sprintf("Frames every %.1f s from %.1f to %.1f s.", step, span[0], span[1])
	if err := m.ask(ctx, system, framesContent(text, "Mark the parts.", frames, cues), schema, result); err != nil {
		return nil, err
	}
	return frames, nil
}

// headEnd is how far the head is scanned: past the end of the intro the
// subtitles tell, or the longest pause near the start, within HeadSpan.
func headEnd(rough Rough, cues []Cue, duration float64) float64 {
	end := 0.0
	if rough.Intro.Present {
		end = rough.Intro.End
	}
	longest := 0.0
	for _, gap := range Gaps(cues, duration, 15) {
		if gap.Start < HeadSpan*0.8 && gap.End-gap.Start > longest {
			longest, end = gap.End-gap.Start, math.Max(end, gap.End)
		}
	}
	if end == 0 {
		end = 240
	}
	return math.Min(math.Min(end+30, HeadSpan), duration)
}

// tailStart is where the tail is scanned from: before the credits the
// subtitles tell, or the last line, within TailSpan of the end.
func tailStart(rough Rough, cues []Cue, duration float64) float64 {
	start := duration - 120
	if rough.Credits.Present {
		start = rough.Credits.Start
	} else if len(cues) > 0 {
		start = cues[len(cues)-1].End
	}
	return math.Max(math.Max(start-60, duration-TailSpan), 0)
}
