package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"title-api/internal/marker"
)

// Per million tokens, input and output, for the cost shown.
var prices = map[string][2]float64{
	"claude-opus-5-5":   {4, 20},
	"claude-sonnet-5-5": {2, 10},
	"claude-haiku-4-5":  {1, 5},
	// For prompts up to 100k tokens, which these are.
	"claude-haiku-5-5": {0.10, 0.50},
}

type episodeResult struct {
	Episode   string             `json:"episode"`
	Track     string             `json:"track"`
	Duration  float64            `json:"duration"`
	Reference map[string]float64 `json:"reference"`
	Result    marker.Result      `json:"result"`
	Usage     marker.Usage       `json:"usage"`
}

func runMark(args []string) error {
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	dir := flags.String("dir", "", "fixtures directory (of one title)")
	model := flags.String("model", "claude-sonnet-5-5", "model")
	effort := flags.String("effort", "low", "effort")
	language := flags.String("lang", "en,eng,ru,rus", "subtitle languages by preference")
	flags.Parse(args)
	if *dir == "" {
		return errors.New("usage: sectionlab run -dir DIR/<title> [s2e5 ...]")
	}
	episodes := flags.Args()
	if len(episodes) == 0 {
		entries, err := os.ReadDir(*dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				episodes = append(episodes, entry.Name())
			}
		}
	}
	m := &marker.Marker{Client: anthropic.NewClient(), Model: *model, Effort: anthropic.OutputConfigEffort(*effort)}
	stamp := time.Now().Format("20060102-150405")
	var results []episodeResult
	for _, name := range episodes {
		before := m.Usage()
		result, err := markEpisode(context.Background(), m, filepath.Join(*dir, name), strings.Split(*language, ","))
		if err != nil {
			log.Printf("%s: %v", name, err)
			continue
		}
		result.Episode = name
		after := m.Usage()
		result.Usage = marker.Usage{
			Requests:     after.Requests - before.Requests,
			InputTokens:  after.InputTokens - before.InputTokens,
			OutputTokens: after.OutputTokens - before.OutputTokens,
		}
		runs := filepath.Join(*dir, name, "runs")
		if err := os.MkdirAll(runs, 0o755); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(runs, fmt.Sprintf("%s-%s-%s.json", stamp, *model, *effort)), result); err != nil {
			return err
		}
		results = append(results, result)
		printEpisode(result)
	}
	printSummary(results, *model)
	return nil
}

var boundaryNames = []string{"recap_end", "intro_start", "intro_end", "credits_start", "credits_end"}

func markEpisode(ctx context.Context, m *marker.Marker, dir string, languages []string) (episodeResult, error) {
	var result episodeResult
	var s stream
	if err := readJSON(filepath.Join(dir, "stream.json"), &s); err != nil {
		return result, err
	}
	var v video
	if err := readJSON(filepath.Join(dir, "video.json"), &v); err != nil {
		return result, err
	}
	result.Duration = v.Duration
	result.Reference = reference(s.Sections)
	// The provider's credits may run on a little past the video's end.
	if end, ok := result.Reference["credits_end"]; ok && end > v.Duration {
		result.Reference["credits_end"] = v.Duration
	}
	cues, name, err := pickTrack(dir, s.Subtitles, languages, v.Duration)
	if err != nil {
		return result, err
	}
	result.Track = name
	result.Result, err = m.Mark(ctx, localVideo(dir, v), cues, v.Duration)
	return result, err
}

// reference is the provider's boundaries by name.
func reference(sections []section) map[string]float64 {
	result := map[string]float64{}
	for _, s := range sections {
		switch s.Kind {
		case "KIND_RECAP":
			result["recap_end"] = s.End
		case "KIND_INTRO":
			result["intro_start"], result["intro_end"] = s.Start, s.End
		case "KIND_CREDITS":
			result["credits_start"], result["credits_end"] = s.Start, s.End
		}
	}
	return result
}

func pickTrack(dir string, tracks []track, languages []string, duration float64) ([]marker.Cue, string, error) {
	sort.SliceStable(tracks, func(i, j int) bool {
		return rank(tracks[i].Language, languages) < rank(tracks[j].Language, languages)
	})
	for _, t := range tracks {
		data, err := os.ReadFile(filepath.Join(dir, "subs", t.ID+".vtt"))
		if err != nil {
			continue
		}
		cues, err := marker.ParseVTT(data)
		if err != nil || t.Forced || !marker.Full(cues, duration) {
			continue
		}
		return cues, t.Name, nil
	}
	return nil, "", errors.New("no full subtitles")
}

func rank(language string, languages []string) int {
	for index, l := range languages {
		if strings.EqualFold(language, l) {
			return index
		}
	}
	return len(languages)
}

func localVideo(dir string, v video) marker.Video {
	result := marker.Video{}
	if v.Init != "" {
		result.Init = func() ([]byte, error) { return os.ReadFile(filepath.Join(dir, "seg", v.Init)) }
	}
	for _, s := range v.Segments {
		file := s.File
		result.Segments = append(result.Segments, marker.Segment{Start: s.Start, Duration: s.Duration, Load: func() ([]byte, error) {
			if file == "" {
				return nil, errors.New("not in the fixture")
			}
			return os.ReadFile(filepath.Join(dir, "seg", file))
		}})
	}
	return result
}

// stages are a boundary as the subtitles, the scan and the refinement
// had it.
func stages(r episodeResult, name string) (rough, scan, end float64, hasRough, hasScan, hasEnd bool) {
	x := r.Result
	switch name {
	case "recap_end":
		rough, hasRough = x.Rough.Recap.End, x.Rough.Recap.Present
	case "intro_start":
		rough, hasRough = x.Rough.Intro.Start, x.Rough.Intro.Present
	case "intro_end":
		rough, hasRough = x.Rough.Intro.End, x.Rough.Intro.Present
	case "credits_start":
		rough, hasRough = x.Rough.Credits.Start, x.Rough.Credits.Present
	case "credits_end":
		rough, hasRough = x.Rough.Credits.End, x.Rough.Credits.Present
	}
	for _, near := range x.Near {
		if near.Name == name || name == "intro_start" && near.Name == "recap_end" && x.Times["intro_start"] == x.Times["recap_end"] {
			scan, hasScan = near.Near, true
		}
	}
	end, hasEnd = x.Times[name]
	return
}

func printEpisode(r episodeResult) {
	x := r.Result
	fmt.Printf("\n%s  (%s, %.0f s)\n  subtitles: %s\n  head %.0f–%.0f: %s\n  tail %.0f–%.0f: %s\n", r.Episode, r.Track, r.Duration,
		x.Rough.Notes, x.HeadRange[0], x.HeadRange[1], x.Head.Notes, x.TailRange[0], x.TailRange[1], x.Tail.Notes)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "boundary\treference\tsubs\terr\tscan\terr\tfinal\terr\tconf\t")
	for _, name := range boundaryNames {
		ref, hasRef := r.Reference[name]
		rough, scan, end, hasRough, hasScan, hasEnd := stages(r, name)
		if !hasRef && !hasEnd && !hasRough {
			continue
		}
		conf := ""
		if exact, ok := x.Exact[name]; ok {
			conf = fmt.Sprintf("%.2f", exact.Confidence)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", name, value(ref, hasRef),
			value(rough, hasRough), diff(rough, ref, hasRough && hasRef),
			value(scan, hasScan), diff(scan, ref, hasScan && hasRef),
			value(end, hasEnd), diff(end, ref, hasEnd && hasRef), conf)
	}
	w.Flush()
	for name, err := range x.Errors {
		fmt.Printf("  %s: %s\n", name, err)
	}
	fmt.Printf("  credits skippable: %v\n", x.Skippable)
}

func value(v float64, ok bool) string {
	if !ok {
		return "—"
	}
	return fmt.Sprintf("%.2f", v)
}

func diff(v, ref float64, ok bool) string {
	if !ok {
		return ""
	}
	return fmt.Sprintf("%+.2f", v-ref)
}

func printSummary(results []episodeResult, model string) {
	fmt.Printf("\n%d episodes\n", len(results))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "boundary\tn\tsubs med\tscan med\tfinal med\tfinal max\t≤0.5 s\t≤1 s\tmissed\textra\t")
	for _, name := range boundaryNames {
		var rough, scan, end []float64
		missed, extra := 0, 0
		for _, r := range results {
			ref, hasRef := r.Reference[name]
			roughValue, scanValue, endValue, hasRough, hasScan, hasEnd := stages(r, name)
			switch {
			case hasRef && !hasEnd:
				missed++
			case !hasRef && hasEnd:
				extra++
			case hasRef && hasEnd:
				end = append(end, math.Abs(endValue-ref))
				if hasRough {
					rough = append(rough, math.Abs(roughValue-ref))
				}
				if hasScan {
					scan = append(scan, math.Abs(scanValue-ref))
				}
			}
		}
		fmt.Fprintf(w, "%s\t%d\t%.2f\t%.2f\t%.2f\t%.2f\t%d\t%d\t%d\t%d\t\n", name, len(end),
			median(rough), median(scan), median(end), maximum(end), within(end, 0.5), within(end, 1), missed, extra)
	}
	w.Flush()
	var usage marker.Usage
	for _, r := range results {
		usage.Requests += r.Usage.Requests
		usage.InputTokens += r.Usage.InputTokens
		usage.OutputTokens += r.Usage.OutputTokens
	}
	price := prices[model]
	cost := float64(usage.InputTokens)/1e6*price[0] + float64(usage.OutputTokens)/1e6*price[1]
	fmt.Printf("%d requests, %d input and %d output tokens, $%.3f", usage.Requests, usage.InputTokens, usage.OutputTokens, cost)
	if len(results) > 0 {
		fmt.Printf(" ($%.3f an episode)", cost/float64(len(results)))
	}
	fmt.Println()
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[middle-1] + sorted[middle]) / 2
	}
	return sorted[middle]
}

func maximum(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	result := values[0]
	for _, v := range values {
		result = math.Max(result, v)
	}
	return result
}

func within(values []float64, limit float64) int {
	n := 0
	for _, v := range values {
		if v <= limit {
			n++
		}
	}
	return n
}

// runFrames writes the frames a fixture's video has from low to high
// seconds, step apart, named by their times, for a look at what the model
// is shown.
func runFrames(args []string) error {
	flags := flag.NewFlagSet("frames", flag.ExitOnError)
	dir := flags.String("dir", "", "episode fixture directory")
	out := flags.String("out", "", "directory to write the frames to")
	low := flags.Float64("from", 0, "seconds")
	high := flags.Float64("to", 0, "seconds")
	step := flags.Float64("step", 0.5, "seconds between frames")
	flags.Parse(args)
	var v video
	if err := readJSON(filepath.Join(*dir, "video.json"), &v); err != nil {
		return err
	}
	frames, err := localVideo(*dir, v).Decode(context.Background(), *low, *high)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	last := math.Inf(-1)
	for _, frame := range frames {
		if frame.Time-last < *step-1e-6 {
			continue
		}
		last = frame.Time
		if err := os.WriteFile(filepath.Join(*out, fmt.Sprintf("%09.3f.jpg", frame.Time)), frame.JPEG, 0o644); err != nil {
			return err
		}
	}
	return nil
}
