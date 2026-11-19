package faces

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
)

var errNoFFmpeg = errors.New("ffmpeg not found")

// ErrNoFrame is a segment with no frame where the pause is, or none that
// ffmpeg can decode (an fMP4 one without its init segment).
var ErrNoFrame = errors.New("no frame at the offset in the segment")

func ffmpegPath() string {
	if path := os.Getenv("FFMPEG"); path != "" {
		return path
	}
	path, _ := exec.LookPath("ffmpeg")
	return path
}

// pausedFrame is the frame offset seconds into a segment, as a JPEG of
// its full size: the segment as it was served (an fMP4 one's init segment
// first), its first frame at 0, as a segment begins with a key frame. The
// frame on the screen is the last one begun by then, so a frame a little
// before is taken rather than the next.
func pausedFrame(ctx context.Context, segment []byte, offset float64) ([]byte, error) {
	path := ffmpegPath()
	if path == "" {
		return nil, errNoFFmpeg
	}
	dir, err := os.MkdirTemp("", "faces-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "segment")
	if err := os.WriteFile(input, segment, 0o600); err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, path, "-hide_banner", "-nostats", "-loglevel", "error",
		"-i", input, "-map", "0:v:0", "-an", "-sn",
		"-vf", fmt.Sprintf("trim=start=%.3f", math.Max(offset-0.02, 0)),
		"-frames:v", "1", "-q:v", "2", "-f", "image2pipe", "-c:v", "mjpeg", "pipe:1")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		// Past the segment's last frame nothing gets to the encoder.
		if bytes.Contains(stderr.Bytes(), []byte("Nothing was written")) {
			return nil, ErrNoFrame
		}
		tail := stderr.Bytes()
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return nil, fmt.Errorf("%w: ffmpeg: %v: %s", ErrNoFrame, err, bytes.TrimSpace(tail))
	}
	if stdout.Len() == 0 {
		return nil, ErrNoFrame
	}
	return stdout.Bytes(), nil
}
