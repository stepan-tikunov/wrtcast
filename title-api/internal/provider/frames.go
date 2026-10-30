package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"sync"
)

// Frames of two videos are matched by their look at a tiny size: each frame
// scaled to frameWidth×frameHeight in gray, a tenth cut off every side (a
// dub's logo in a corner, a letterbox), and the rest as a vector of zero
// mean and unit length, so that two frames' similarity is their dot
// product, whatever the encodes' brightness and contrast.
const (
	frameWidth  = 32
	frameHeight = 18
	cropX       = frameWidth / 10
	cropY       = frameHeight / 10
	frameDims   = (frameWidth - 2*cropX) * (frameHeight - 2*cropY)
	// A frame whose gray varies less than this (of 255) is black or a
	// fade, and looks like any other.
	flatDeviation = 4
)

// frames are the frames of a stretch of video: the fingerprint of each
// (nil for a flat one) and its time on its playlist's timeline.
type frames struct {
	print [][]float32
	times []float64
}

func (f frames) len() int { return len(f.times) }

// textured is how many frames are not flat.
func (f frames) textured() int {
	n := 0
	for _, p := range f.print {
		if p != nil {
			n++
		}
	}
	return n
}

// The ffmpeg that decodes the frames: $FFMPEG, else the one on the PATH.
var ffmpegPath = sync.OnceValue(func() string {
	if path := os.Getenv("FFMPEG"); path != "" {
		return path
	}
	path, _ := exec.LookPath("ffmpeg")
	return path
})

var errNoFFmpeg = errors.New("ffmpeg not found")

var ptsTimePattern = regexp.MustCompile(`pts_time:\s*(-?[0-9.]+)`)

// decodeFrames decodes the video of one stretch of an HLS stream: its
// segments as they were served, one after another (an fMP4 one's init
// segment first). start is where the first segment begins on its playlist:
// the frames' times are moved onto the playlist's timeline by the first
// frame, as a segment begins with a key frame. The decoder skips the loop
// filter, which no 32×18 picture shows.
func decodeFrames(ctx context.Context, data []byte, start float64) (frames, error) {
	path := ffmpegPath()
	if path == "" {
		return frames{}, errNoFFmpeg
	}
	command := exec.CommandContext(ctx, path, "-hide_banner", "-nostats", "-loglevel", "info",
		"-skip_loop_filter", "all", "-copyts", "-i", "pipe:0", "-map", "0:v:0", "-an", "-sn",
		"-fps_mode", "passthrough",
		"-vf", fmt.Sprintf("scale=%d:%d:flags=area,format=gray,showinfo", frameWidth, frameHeight),
		"-f", "rawvideo", "pipe:1")
	command.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		tail := stderr.Bytes()
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return frames{}, fmt.Errorf("ffmpeg: %w: %s", err, bytes.TrimSpace(tail))
	}
	var pts []float64
	for _, match := range ptsTimePattern.FindAllSubmatch(stderr.Bytes(), -1) {
		value, err := strconv.ParseFloat(string(match[1]), 64)
		if err != nil {
			return frames{}, fmt.Errorf("ffmpeg pts_time %q: %w", match[1], err)
		}
		pts = append(pts, value)
	}
	size := frameWidth * frameHeight
	count := stdout.Len() / size
	if count == 0 || count != len(pts) {
		return frames{}, fmt.Errorf("ffmpeg gave %d frames and %d times", count, len(pts))
	}
	first := math.Inf(1)
	for _, value := range pts {
		first = math.Min(first, value)
	}
	result := frames{print: make([][]float32, count), times: make([]float64, count)}
	for index := range count {
		result.print[index] = fingerprint(stdout.Bytes()[index*size : (index+1)*size])
		result.times[index] = pts[index] - first + start
	}
	sortFrames(&result)
	return result, nil
}

// fingerprint is a gray frameWidth×frameHeight picture's vector, nil for a
// flat one.
func fingerprint(gray []byte) []float32 {
	vector := make([]float32, 0, frameDims)
	var sum float64
	for y := cropY; y < frameHeight-cropY; y++ {
		for x := cropX; x < frameWidth-cropX; x++ {
			value := float32(gray[y*frameWidth+x])
			vector = append(vector, value)
			sum += float64(value)
		}
	}
	mean := float32(sum / float64(len(vector)))
	var squares float64
	for index := range vector {
		vector[index] -= mean
		squares += float64(vector[index]) * float64(vector[index])
	}
	if math.Sqrt(squares/float64(len(vector))) < flatDeviation {
		return nil
	}
	norm := float32(math.Sqrt(squares))
	for index := range vector {
		vector[index] /= norm
	}
	return vector
}

func sortFrames(f *frames) {
	order := make([]int, f.len())
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(left, right int) bool { return f.times[order[left]] < f.times[order[right]] })
	prints, times := make([][]float32, len(order)), make([]float64, len(order))
	for index, from := range order {
		prints[index], times[index] = f.print[from], f.times[from]
	}
	f.print, f.times = prints, times
}

// appendFrames joins stretches of one video, in order.
func appendFrames(parts ...frames) frames {
	var result frames
	for _, part := range parts {
		result.print = append(result.print, part.print...)
		result.times = append(result.times, part.times...)
	}
	sortFrames(&result)
	return result
}

// What a match must show to be trusted: the frames lined up that alike on
// average, the best offset that much ahead of any other more than
// matchApart away (a still scene or a slow pan matches itself a little off
// too), and at least that many textured frames compared.
const (
	matchScore  = 0.9
	matchMargin = 0.05
	matchApart  = 0.5
	matchFrames = 60
	// The offsets tried are this far apart, a quarter of a frame.
	matchStep = 1.0 / 96
	// A frame lines up with the other video's nearest one this close to
	// the time it should be at.
	matchSlack = 0.03
)

// matchResult is the offset at which a stretch of one video (the
// alternative's) lines up with the other (the primary's): their times
// relate as alt = scale·primary + offset.
type matchResult struct {
	offset, score, runnerUp float64
	compared                int
}

func (m matchResult) confident() bool {
	return m.compared >= matchFrames && m.score >= matchScore && m.score-m.runnerUp >= matchMargin
}

// matchOffset finds the offset in [low, high] at which the alt frames line
// up best with the primary ones: for each offset, the mean similarity of
// every textured alt frame to the primary frame at its time. Offsets that
// line up fewer than half the alt frames (the primary stretch is too short
// there) are passed over.
func matchOffset(alt, primary frames, scale, low, high float64) (matchResult, error) {
	type candidate struct {
		offset, score float64
		compared      int
	}
	var textured []int
	for index, p := range alt.print {
		if p != nil {
			textured = append(textured, index)
		}
	}
	if len(textured) < matchFrames || primary.len() == 0 {
		return matchResult{}, fmt.Errorf("too few frames to match: %d textured of %d, %d primary", len(textured), alt.len(), primary.len())
	}
	// The similarities are worked out once, lazily: an alt frame meets
	// only the primary frames near its times.
	similarity := make(map[[2]int]float32)
	similar := func(a, p int) float32 {
		key := [2]int{a, p}
		if value, ok := similarity[key]; ok {
			return value
		}
		var dot float32
		for index, value := range alt.print[a] {
			dot += value * primary.print[p][index]
		}
		similarity[key] = dot
		return dot
	}
	var candidates []candidate
	for step := 0; ; step++ {
		offset := low + float64(step)*matchStep
		if offset > high {
			break
		}
		var sum float64
		compared := 0
		for _, a := range textured {
			want := (alt.times[a] - offset) / scale
			p := sort.SearchFloat64s(primary.times, want)
			if p > 0 && (p == primary.len() || want-primary.times[p-1] < primary.times[p]-want) {
				p--
			}
			if p >= primary.len() || math.Abs(primary.times[p]-want) > matchSlack {
				continue
			}
			compared++
			if primary.print[p] != nil {
				sum += float64(similar(a, p))
			}
		}
		if compared*2 < len(textured) {
			continue
		}
		candidates = append(candidates, candidate{offset: offset, score: sum / float64(compared), compared: compared})
	}
	if len(candidates) == 0 {
		return matchResult{}, errors.New("the primary stretch covers none of the offsets")
	}
	best := candidates[0]
	for _, c := range candidates {
		if c.score > best.score {
			best = c
		}
	}
	result := matchResult{offset: best.offset, score: best.score, compared: best.compared, runnerUp: -1}
	for _, c := range candidates {
		if math.Abs(c.offset-best.offset) > matchApart && c.score > result.runnerUp {
			result.runnerUp = c.score
		}
	}
	return result, nil
}
