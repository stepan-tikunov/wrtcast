package provider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Two encodes of one video, as the players have them: the primary's in
// MPEG-TS segments of 10 s at 640×360, the alternative's in fMP4 segments
// of 4 s at 1280×720 and another quality, beginning 2.1 s later into the
// video. A stretch of the alternative's matches the primary's at -2.1 s.
// The video is a chaotic cellular automaton, which no two moments of look
// alike.
func TestDecodeAndMatchEncodes(t *testing.T) {
	if ffmpegPath() == "" {
		t.Skip("no ffmpeg")
	}
	dir := t.TempDir()
	encode := func(args ...string) {
		t.Helper()
		base := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "cellauto=rule=30:size=64x36:rate=24:random_fill_ratio=0.5:random_seed=7,scale=1280:720:flags=neighbor"}
		output, err := exec.Command(ffmpegPath(), append(base, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, output)
		}
	}
	keys := []string{"-c:v", "libx264", "-g", "48", "-keyint_min", "48", "-sc_threshold", "0", "-f", "hls", "-hls_playlist_type", "vod"}
	encode(append([]string{"-t", "40", "-vf", "scale=640:360", "-crf", "30"},
		append(keys, "-hls_time", "10", "-hls_segment_filename", filepath.Join(dir, "p%d.ts"), filepath.Join(dir, "p.m3u8"))...)...)
	encode(append([]string{"-ss", "2.1", "-t", "37.9", "-crf", "20"},
		append(keys, "-hls_time", "4", "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "a-init.mp4",
			"-hls_segment_filename", filepath.Join(dir, "a%d.m4s"), filepath.Join(dir, "a.m3u8"))...)...)

	segments := func(name string) []hlsSegment {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		list, err := parseMediaPlaylist("file://"+dir+"/"+name, string(body))
		if err != nil {
			t.Fatal(err)
		}
		return list
	}
	read := func(rawURL string) []byte {
		t.Helper()
		body, err := os.ReadFile(rawURL[len("file://"):])
		if err != nil {
			t.Fatal(err)
		}
		return body
	}

	ctx := context.Background()
	var primary []frames
	for _, segment := range segments("p.m3u8") {
		decoded, err := decodeFrames(ctx, read(segment.url), segment.start)
		if err != nil {
			t.Fatal(err)
		}
		primary = append(primary, decoded)
	}
	alternative := segments("a.m3u8")
	if len(alternative) < 5 {
		t.Fatalf("%d alternative segments", len(alternative))
	}
	data := read(alternative[2].init)
	for _, segment := range alternative[2:4] {
		data = append(data, read(segment.url)...)
	}
	alt, err := decodeFrames(ctx, data, alternative[2].start)
	if err != nil {
		t.Fatal(err)
	}
	if alt.len() < 150 || !near(alt.times[0], alternative[2].start, 0.1) {
		t.Fatalf("%d alternative frames from %.3f", alt.len(), alt.times[0])
	}
	result, err := matchOffset(alt, appendFrames(primary...), 1, -10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !result.confident() || !near(result.offset, -2.1, 0.05) {
		t.Fatalf("match = %+v, want a confident -2.1", result)
	}
}
