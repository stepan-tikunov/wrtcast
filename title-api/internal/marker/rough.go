package marker

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Part is where a part of the video was found to be, in seconds, if at all.
type Part struct {
	Present    bool    `json:"present"`
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Confidence float64 `json:"confidence"`
}

// Rough is the parts of a video as its subtitles tell them.
type Rough struct {
	Recap   Part `json:"recap"`
	Intro   Part `json:"intro"`
	Credits struct {
		Part
		// False when there is more to see after the credits.
		Skippable bool `json:"skippable"`
	} `json:"credits"`
	Notes string `json:"notes"`
}

// The pauses between subtitles listed for the model, at least so long.
const roughGap = 8.0

const roughSystem = `You mark the parts of a TV episode that a viewer may skip, from the episode's subtitles:

- recap: a montage of scenes from earlier episodes ("Previously on…", "Ранее в сериале…"), usually right at the start of the video. Its lines jump between unrelated scenes and refer to things the viewer is supposed to remember.
- intro: the opening title sequence. Usually there is no dialogue in it, so it shows as a pause in the subtitles of 15 s to 2 min, though a song's lyrics may be subtitled. It may follow the recap directly or come after a cold open (a scene of the new episode before the titles, which is not part of the recap).
- credits: the end titles. They begin after the last scene's final lines, usually as a pause running to the end of the video. If there is a scene or more dialogue after the credits, they are not skippable.

Any part may be missing; then set present to false and its times to 0. Times are in seconds from the start of the video. They need not be exact (frames will refine them later), but put each boundary where you think it most likely is: for a pause, decide where in it the part begins or ends. Confidence is from 0 to 1. Notes are one or two short sentences on what you saw.`

var partSchema = object(map[string]any{
	"present":    typed("boolean", "Whether the part is in the video"),
	"start":      typed("number", "Start, seconds"),
	"end":        typed("number", "End, seconds"),
	"confidence": typed("number", "0 to 1"),
})

var roughSchema = object(map[string]any{
	"recap": partSchema,
	"intro": partSchema,
	"credits": object(map[string]any{
		"present":    typed("boolean", "Whether the part is in the video"),
		"start":      typed("number", "Start, seconds"),
		"end":        typed("number", "End, seconds"),
		"confidence": typed("number", "0 to 1"),
		"skippable":  typed("boolean", "False when a scene or dialogue follows the credits"),
	}),
	"notes": typed("string", "What was seen, briefly"),
})

// Rough finds the parts of a video of duration seconds from its cues.
func (m *Marker) Rough(ctx context.Context, cues []Cue, duration float64) (Rough, error) {
	var text strings.Builder
	fmt.Fprintf(&text, "Video duration: %.1f s.\n\nSubtitles, [start–end] text:\n", duration)
	for _, cue := range cues {
		fmt.Fprintf(&text, "[%.1f–%.1f] %s\n", cue.Start, cue.End, cue.Text)
	}
	fmt.Fprintf(&text, "\nPauses without subtitles of %.0f s or more:\n", roughGap)
	for _, gap := range Gaps(cues, duration, roughGap) {
		fmt.Fprintf(&text, "%.1f–%.1f (%.0f s)\n", gap.Start, gap.End, gap.End-gap.Start)
	}
	var result Rough
	err := m.ask(ctx, StageSubtitles, roughSystem, []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(text.String())}, roughSchema, &result)
	return result, err
}
