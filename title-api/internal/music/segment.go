package music

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// SourceOf is the source a video's stream of name is taken as: the name
// (a dub's, which may be cut otherwise) hashed, as the router hashes it
// (FNV-1a, 32 bits, in hex).
func SourceOf(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return fmt.Sprintf("%08x", h.Sum32())
}

var leadingNumber = regexp.MustCompile(`^[0-9]+`)

// EpisodeOf is the number an episode's name starts with, 0 for none.
func EpisodeOf(name string) int32 {
	n, _ := strconv.Atoi(leadingNumber.FindString(name))
	return int32(n)
}

// The ffmpeg that cuts the audio out of a segment: $FFMPEG, else the one
// on the PATH.
var ffmpegPath = sync.OnceValue(func() string {
	if path := os.Getenv("FFMPEG"); path != "" {
		return path
	}
	path, _ := exec.LookPath("ffmpeg")
	return path
})

// Segment takes a segment of a video as its player serves it (fMP4: its
// init segment, then the fragment; or MPEG-TS), from start to stop seconds
// of it: its audio, cut out by ffmpeg as it is (ADTS AAC), as Audio takes
// it. A segment taken before is not looked at again.
func (s *Service) Segment(ctx context.Context, v Video, start, stop float64, init, data []byte) error {
	if done, err := s.store.hasAudio(ctx, v.key(), start); err != nil || done {
		return err
	}
	path := ffmpegPath()
	if path == "" {
		return errors.New("ffmpeg not found")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "-hide_banner", "-nostats", "-loglevel", "error",
		"-i", "pipe:0", "-map", "0:a:0", "-vn", "-sn", "-c:a", "copy", "-f", "adts", "pipe:1")
	command.Stdin = bytes.NewReader(append(append([]byte(nil), init...), data...))
	var out, stderr bytes.Buffer
	command.Stdout, command.Stderr = &out, &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("cut the audio out: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	if out.Len() == 0 {
		return errors.New("cut the audio out: none")
	}
	return s.Audio(ctx, v, start, stop, out.Bytes())
}
