package marker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// The frames shown are scaled to so many pixels wide.
const frameWidth = 384

// Video is a media playlist's segments, which can be loaded.
type Video struct {
	Segments []Segment
	// The init segment, nil for MPEG-TS.
	Init func() ([]byte, error)
}

// Segment is a segment's place on the video's timeline and its bytes.
type Segment struct {
	Start, Duration float64
	Load            func() ([]byte, error)
}

// Frame is a frame of the video, as JPEG, and its time.
type Frame struct {
	Time float64
	JPEG []byte
}

var ptsTimePattern = regexp.MustCompile(`pts_time:\s*(-?[0-9.]+)`)

// FFmpeg is the ffmpeg that decodes the frames: $FFMPEG, else the one on
// the PATH.
func FFmpeg() string {
	if path := os.Getenv("FFMPEG"); path != "" {
		return path
	}
	path, _ := exec.LookPath("ffmpeg")
	return path
}

// Decode gives every frame of the video from low to high seconds. The
// segments over them are decoded together, and the frames' times moved
// onto the playlist's timeline by the first one, as a segment begins with
// a key frame.
func (v Video) Decode(ctx context.Context, low, high float64) ([]Frame, error) {
	path := FFmpeg()
	if path == "" {
		return nil, errors.New("no ffmpeg")
	}
	var data []byte
	if v.Init != nil {
		init, err := v.Init()
		if err != nil {
			return nil, fmt.Errorf("init segment: %w", err)
		}
		data = append(data, init...)
	}
	first := math.Inf(1)
	for _, segment := range v.Segments {
		if segment.Start+segment.Duration <= low || segment.Start >= high {
			continue
		}
		body, err := segment.Load()
		if err != nil {
			return nil, fmt.Errorf("segment at %.0f s: %w", segment.Start, err)
		}
		data = append(data, body...)
		first = math.Min(first, segment.Start)
	}
	if math.IsInf(first, 1) {
		return nil, fmt.Errorf("no segment from %.0f to %.0f s", low, high)
	}
	dir, err := os.MkdirTemp("", "marker-frames-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	command := exec.CommandContext(ctx, path, "-hide_banner", "-nostats", "-loglevel", "info",
		"-copyts", "-i", "pipe:0", "-map", "0:v:0", "-an", "-sn", "-fps_mode", "passthrough",
		"-vf", fmt.Sprintf("scale=%d:-2:flags=bicubic,showinfo", frameWidth),
		"-q:v", "4", filepath.Join(dir, "%06d.jpg"))
	command.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		tail := stderr.Bytes()
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, bytes.TrimSpace(tail))
	}
	var pts []float64
	for _, match := range ptsTimePattern.FindAllSubmatch(stderr.Bytes(), -1) {
		value, err := strconv.ParseFloat(string(match[1]), 64)
		if err != nil {
			return nil, fmt.Errorf("ffmpeg pts_time %q: %w", match[1], err)
		}
		pts = append(pts, value)
	}
	if len(pts) == 0 {
		return nil, errors.New("ffmpeg gave no frames")
	}
	zero := pts[0]
	for _, value := range pts {
		zero = math.Min(zero, value)
	}
	var frames []Frame
	for index, value := range pts {
		at := value - zero + first
		if at < low || at > high {
			continue
		}
		jpeg, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%06d.jpg", index+1)))
		if err != nil {
			return nil, err
		}
		frames = append(frames, Frame{Time: at, JPEG: jpeg})
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i].Time < frames[j].Time })
	return frames, nil
}

// sample picks from frames, in order, the one nearest to each step from
// low to high, each once.
func sample(frames []Frame, low, high, step float64) []Frame {
	var result []Frame
	last := -1
	for at := low; at <= high+1e-9; at += step {
		index := sort.Search(len(frames), func(i int) bool { return frames[i].Time >= at })
		if index == len(frames) || index > 0 && at-frames[index-1].Time < frames[index].Time-at {
			index--
		}
		if index < 0 || index <= last {
			continue
		}
		result = append(result, frames[index])
		last = index
	}
	return result
}
