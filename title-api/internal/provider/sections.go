package provider

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
)

var sectionsPattern = regexp.MustCompile(`["']?sections["']?\s*:\s*\[`)

type SectionKind int

const (
	SectionUnknown SectionKind = iota
	// What happened in the earlier episodes.
	SectionRecap
	// The opening titles.
	SectionIntro
	// The end titles.
	SectionCredits
)

// Section is a part of a movie or an episode that viewers may skip, as the
// primary player marks it, in seconds from the start of its video.
// Skippable is false when there is more to see, such as a scene after the
// credits.
type Section struct {
	Kind       SectionKind
	Start, End float64
	Skippable  bool
}

// playerSection is a section as the primary player has it: its types are
// shown as "повтор", "интро" and "титры".
type playerSection struct {
	Type  string        `json:"type"`
	Start playerSeconds `json:"start"`
	End   playerSeconds `json:"end"`
	Skip  bool          `json:"skip"`
}

var playerSectionKinds = map[string]SectionKind{
	"repeat": SectionRecap,
	"screen": SectionIntro,
	"titer":  SectionCredits,
}

// playerSeconds is a time the player writes as a number or a string.
type playerSeconds float64

func (s *playerSeconds) UnmarshalJSON(data []byte) error {
	data = bytes.Trim(data, `"`)
	value, err := strconv.ParseFloat(string(data), 64)
	if err != nil {
		value = 0
	}
	*s = playerSeconds(value)
	return nil
}

// sections are the player's sections of known types that are not empty,
// in order.
func sections(source []playerSection) []Section {
	var result []Section
	for _, section := range source {
		kind := playerSectionKinds[section.Type]
		start, end := float64(section.Start), float64(section.End)
		if kind == SectionUnknown || start < 0 || end <= start {
			continue
		}
		result = append(result, Section{Kind: kind, Start: start, End: end, Skippable: section.Skip})
	}
	sort.SliceStable(result, func(left, right int) bool { return result[left].Start < result[right].Start })
	return result
}

// movieSections are the sections of a movie's embed page, whose one source
// has them as an episode does.
func movieSections(embedHTML []byte) []Section {
	match := sectionsPattern.FindIndex(embedHTML)
	if match == nil {
		return nil
	}
	start := match[1] - 1
	end, err := balancedJSONArrayEnd(embedHTML, start)
	if err != nil {
		return nil
	}
	var source []playerSection
	if json.Unmarshal(embedHTML[start:end], &source) != nil {
		return nil
	}
	return sections(source)
}

// shareSections gives the streams without sections of their own, such as
// the alternative player's, those of the movie or the episode.
func shareSections(streams []MovieStream, list []Section, duration float64) {
	if len(list) == 0 {
		return
	}
	for index := range streams {
		if len(streams[index].Sections) == 0 {
			streams[index].Sections = list
			streams[index].SectionsDuration = duration
		}
	}
}
