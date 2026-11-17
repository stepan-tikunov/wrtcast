// Package marker finds the recap, the intro and the credits of an episode
// the provider has not marked: roughly from its subtitles first, then
// exactly from the frames around each boundary, by asking Claude.
package marker

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Cue is a subtitle line and its time on the video's timeline, in seconds.
type Cue struct {
	Start, End float64
	Text       string
}

var (
	cueTimingPattern = regexp.MustCompile(`^((?:\d+:)?\d{1,2}:\d{2}[.,]\d{1,3})\s+-->\s+((?:\d+:)?\d{1,2}:\d{2}[.,]\d{1,3})`)
	tagPattern       = regexp.MustCompile(`<[^>]*>|\{\\[^}]*\}`)
)

// ParseVTT reads the cues of a WebVTT (or SRT) file, their text on one line
// without tags.
func ParseVTT(data []byte) ([]Cue, error) {
	var cues []Cue
	var current *Cue
	var text []string
	flush := func() {
		if current != nil {
			current.Text = strings.Join(strings.Fields(strings.Join(text, " ")), " ")
			if current.Text != "" {
				cues = append(cues, *current)
			}
		}
		current, text = nil, nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if match := cueTimingPattern.FindStringSubmatch(line); match != nil {
			flush()
			start, err := parseCueTime(match[1])
			if err != nil {
				return nil, err
			}
			end, err := parseCueTime(match[2])
			if err != nil {
				return nil, err
			}
			current = &Cue{Start: start, End: end}
			continue
		}
		if line == "" {
			flush()
			continue
		}
		if current != nil {
			text = append(text, tagPattern.ReplaceAllString(line, ""))
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(cues) == 0 {
		return nil, fmt.Errorf("no cues")
	}
	return cues, nil
}

func parseCueTime(value string) (float64, error) {
	parts := strings.Split(strings.ReplaceAll(value, ",", "."), ":")
	total := 0.0
	for _, part := range parts {
		number, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, fmt.Errorf("cue time %q: %w", value, err)
		}
		total = total*60 + number
	}
	return total, nil
}

// Full tells a full track from a forced one, which has a few lines only
// (signs, foreign speech): at least one cue every two minutes on average.
func Full(cues []Cue, duration float64) bool {
	return duration > 0 && float64(len(cues)) >= duration/120
}

// Gap is a stretch without a cue.
type Gap struct{ Start, End float64 }

// Gaps are the stretches of at least min seconds without a cue, the one
// before the first cue and after the last included.
func Gaps(cues []Cue, duration, min float64) []Gap {
	var gaps []Gap
	last := 0.0
	for _, cue := range cues {
		if cue.Start-last >= min {
			gaps = append(gaps, Gap{last, cue.Start})
		}
		last = max(last, cue.End)
	}
	if duration-last >= min {
		gaps = append(gaps, Gap{last, duration})
	}
	return gaps
}
