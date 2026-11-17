package marker

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Boundary is where one part of the video begins or ends, as a scan found
// it roughly.
type Boundary struct {
	Name string
	// What the boundary is, for the model.
	What string
	Near float64
	// How far off Near may be.
	Spread float64
}

// Exact is a boundary found on the frames.
type Exact struct {
	Time       float64 `json:"time"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
	Passes     int     `json:"passes"`
}

const (
	// The frames a boundary is refined on are so far apart, unless more
	// than maxFrames would be shown.
	refineStep = 1.0 / 8
	maxFrames  = 80
	// The window is moved this many times at most, when the boundary is
	// before or after the frames shown.
	windowMoves = 2
)

const refineSystem = `You find the exact frame where a part of a TV episode begins or ends. You are shown consecutive frames of the video, each preceded by its number and time, and the subtitles spoken around them.

Answer with the number of the first frame after the boundary: the first frame that belongs to what comes next. If the boundary is not among the frames shown, say whether it is before the first frame or after the last one. Confidence is from 0 to 1. Reason is one short sentence on what changes at that frame.`

var refineSchema = object(map[string]any{
	"position":   map[string]any{"type": "string", "enum": []string{"shown", "before", "after"}, "description": "Where the boundary is relative to the frames shown"},
	"frame":      typed("integer", "Number of the first frame after the boundary; 0 unless shown"),
	"confidence": typed("number", "0 to 1"),
	"reason":     typed("string", "What changes at that frame"),
})

type refineAnswer struct {
	Position   string  `json:"position"`
	Frame      int     `json:"frame"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// Refine finds boundary on the frames refineStep apart over its spread
// either side of where it was thought to be. known tells the model what
// else is known of the video's parts.
func (m *Marker) Refine(ctx context.Context, video Video, cues []Cue, boundary Boundary, known string) (Exact, error) {
	var result Exact
	window := boundary.Spread + 0.5
	center := boundary.Near
	for move := 0; ; move++ {
		low, high := math.Max(0, center-window), center+window
		decoded, err := video.Decode(ctx, low, high)
		if err != nil {
			return result, err
		}
		frames := sample(decoded, low, high, math.Max(refineStep, (high-low)/maxFrames))
		var answer refineAnswer
		result.Passes++
		text := fmt.Sprintf("Find %s.\n\n%s", boundary.What, known)
		question := fmt.Sprintf("Which is the first frame after the boundary: %s?", boundary.What)
		if err := m.ask(ctx, refineSystem, framesContent(text, question, frames, cues), refineSchema, &answer); err != nil {
			return result, err
		}
		switch {
		case answer.Position == "shown" && answer.Frame >= 1 && answer.Frame <= len(frames):
			frame := frames[answer.Frame-1]
			result.Time, result.Confidence, result.Reason = frame.Time, answer.Confidence, answer.Reason
			return result, nil
		case answer.Position == "shown":
			return result, fmt.Errorf("%s: frame %d of %d", boundary.Name, answer.Frame, len(frames))
		case move == windowMoves:
			return result, fmt.Errorf("%s: not found near %.1f s (%s)", boundary.Name, boundary.Near, answer.Position)
		case answer.Position == "before":
			center -= 1.8 * window
		default:
			center += 1.8 * window
		}
	}
}

// framesContent is a message of text, the frames numbered from 1 with
// their times, the cues spoken around them and question.
func framesContent(text, question string, frames []Frame, cues []Cue) []anthropic.ContentBlockParamUnion {
	if len(frames) == 0 {
		return []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(text)}
	}
	low, high := frames[0].Time, frames[len(frames)-1].Time
	var b strings.Builder
	b.WriteString(text)
	fmt.Fprintf(&b, "\n\n%d frames from %.2f to %.2f s follow.", len(frames), low, high)
	var spoken []string
	for _, cue := range cues {
		if cue.End >= low-10 && cue.Start <= high+10 {
			spoken = append(spoken, fmt.Sprintf("[%.1f–%.1f] %s", cue.Start, cue.End, cue.Text))
		}
	}
	if len(spoken) > 0 {
		fmt.Fprintf(&b, " Subtitles around them:\n%s", strings.Join(spoken, "\n"))
	} else {
		b.WriteString(" No subtitles around them.")
	}
	content := []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(b.String())}
	for index, frame := range frames {
		content = append(content,
			anthropic.NewTextBlock(fmt.Sprintf("Frame %d, %.2f s:", index+1, frame.Time)),
			anthropic.NewImageBlockBase64("image/jpeg", base64.StdEncoding.EncodeToString(frame.JPEG)))
	}
	return append(content, anthropic.NewTextBlock(question))
}
