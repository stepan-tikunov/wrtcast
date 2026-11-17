package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A fixture is one episode on disk, under <dir>/<title>/s<season>e<episode>:
//
//	stream.json  the primary player's stream, its sections the reference
//	video.json   the lightest variant's segments, those loaded with a file
//	subs/<id>.vtt
//	seg/init.*, seg/<index>.*
//
// Only the segments over the head and the tail of the video are loaded,
// one request at a time with a pause between, as a playback would.

// The CDN binds the primary player's signed links to the User-Agent
// title-api asks for them with, the Chromecast receiver's (see challenge.go).
const userAgent = "Mozilla/5.0 (X11; Linux armv7l) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/90.0.4430.225 Safari/537.36 CrKey/1.56.500000"

type stream struct {
	Name              string    `json:"name"`
	MasterPlaylistURL string    `json:"masterPlaylistUrl"`
	Subtitles         []track   `json:"subtitles"`
	Sections          []section `json:"sections"`
	SectionsDuration  float64   `json:"sectionsDurationSeconds"`
}

type track struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Language    string `json:"language"`
	Forced      bool   `json:"forced"`
	PlaylistURL string `json:"playlistUrl"`
}

type section struct {
	Kind      string  `json:"kind"`
	Start     float64 `json:"startSeconds"`
	End       float64 `json:"endSeconds"`
	Skippable bool    `json:"skippable"`
}

type video struct {
	Duration float64        `json:"duration"`
	Init     string         `json:"init,omitempty"`
	Segments []videoSegment `json:"segments"`
}

type videoSegment struct {
	Start    float64 `json:"start"`
	Duration float64 `json:"duration"`
	// The file under seg/, if loaded.
	File string `json:"file,omitempty"`
	url  string
}

type fetcher struct {
	client *http.Client
	pause  time.Duration
	last   time.Time
}

func runFetch(args []string) error {
	flags := flag.NewFlagSet("fetch", flag.ExitOnError)
	api := flags.String("api", "http://127.0.0.1:50052", "title-api HTTP address")
	dir := flags.String("dir", "", "fixtures directory")
	title := flags.String("title", "", "title ID")
	head := flags.Float64("head", 480, "seconds loaded from the start")
	tail := flags.Float64("tail", 360, "seconds loaded before the end")
	pause := flags.Duration("pause", 300*time.Millisecond, "pause between CDN requests")
	flags.Parse(args)
	if *dir == "" || *title == "" || flags.NArg() == 0 {
		return errors.New("usage: sectionlab fetch -dir DIR -title ID s2e5 ...")
	}
	f := &fetcher{client: &http.Client{Timeout: 60 * time.Second}, pause: *pause}
	ctx := context.Background()
	for _, name := range flags.Args() {
		season, episode, err := parseEpisode(name)
		if err != nil {
			return err
		}
		out := filepath.Join(*dir, *title, fmt.Sprintf("s%02de%02d", season, episode))
		if err := f.fetchEpisode(ctx, *api, *title, season, episode, out, *head, *tail); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func parseEpisode(name string) (int, int, error) {
	var season, episode int
	if _, err := fmt.Sscanf(strings.ToLower(name), "s%de%d", &season, &episode); err != nil {
		return 0, 0, fmt.Errorf("episode %q: want s<season>e<episode>", name)
	}
	return season, episode, nil
}

func (f *fetcher) fetchEpisode(ctx context.Context, api, title string, season, episode int, out string, head, tail float64) error {
	if err := os.MkdirAll(filepath.Join(out, "seg"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(out, "subs"), 0o755); err != nil {
		return err
	}
	var primary stream
	if err := readJSON(filepath.Join(out, "stream.json"), &primary); err != nil {
		body, err := f.get(ctx, fmt.Sprintf("%s/v1/titles/%s/playback-streams?seasonNumber=%d&episodeNumber=%d", api, title, season, episode), false)
		if err != nil {
			return err
		}
		var response struct {
			Streams []stream `json:"streams"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return err
		}
		// The primary player's stream is the CDN's own master; the
		// alternative's go through title-api.
		found := false
		for _, s := range response.Streams {
			if !strings.Contains(s.MasterPlaylistURL, "/v1/media/") {
				primary, found = s, true
				break
			}
		}
		if !found {
			return errors.New("no primary stream")
		}
		if err := writeJSON(filepath.Join(out, "stream.json"), primary); err != nil {
			return err
		}
	}
	log.Printf("%s: %s, %d sections, %d subtitle tracks", out, primary.Name, len(primary.Sections), len(primary.Subtitles))
	for _, t := range primary.Subtitles {
		file := filepath.Join(out, "subs", t.ID+".vtt")
		if _, err := os.Stat(file); err == nil {
			continue
		}
		body, err := f.get(ctx, t.PlaylistURL, true)
		if err != nil {
			return fmt.Errorf("subtitles %s: %w", t.Name, err)
		}
		if err := os.WriteFile(file, body, 0o644); err != nil {
			return err
		}
	}
	var v video
	saved := readJSON(filepath.Join(out, "video.json"), &v) == nil
	if !saved || !v.complete(head, tail) {
		listed, err := f.listVideo(ctx, primary.MasterPlaylistURL)
		if err != nil {
			return err
		}
		// The segments loaded before stay, if the playlist is the same.
		if saved && len(listed.Segments) == len(v.Segments) {
			for index := range listed.Segments {
				listed.Segments[index].File = v.Segments[index].File
			}
		}
		v = listed
	}
	if v.Init != "" && !strings.HasPrefix(v.Init, "init.") {
		name := "init" + extension(v.Init, ".mp4")
		body, err := f.get(ctx, v.Init, true)
		if err != nil {
			return fmt.Errorf("init segment: %w", err)
		}
		if err := os.WriteFile(filepath.Join(out, "seg", name), body, 0o644); err != nil {
			return err
		}
		v.Init = name
	}
	loaded := 0
	for index := range v.Segments {
		segment := &v.Segments[index]
		within := segment.Start < head || segment.Start+segment.Duration > v.Duration-tail
		if !within || segment.File != "" {
			continue
		}
		if segment.url == "" {
			return errors.New("segment links are gone; delete video.json to list them again")
		}
		name := fmt.Sprintf("%05d%s", index, extension(segment.url, ".ts"))
		body, err := f.get(ctx, segment.url, true)
		if err != nil {
			return fmt.Errorf("segment at %.0f s: %w", segment.Start, err)
		}
		if err := os.WriteFile(filepath.Join(out, "seg", name), body, 0o644); err != nil {
			return err
		}
		segment.File = name
		loaded++
		if err := writeJSON(filepath.Join(out, "video.json"), v); err != nil {
			return err
		}
	}
	log.Printf("%s: video %.1f s, %d segments loaded", out, v.Duration, loaded)
	return writeJSON(filepath.Join(out, "video.json"), v)
}

// complete tells whether the segments over the head and the tail are all
// loaded.
func (v video) complete(head, tail float64) bool {
	for _, segment := range v.Segments {
		if (segment.Start < head || segment.Start+segment.Duration > v.Duration-tail) && segment.File == "" {
			return false
		}
	}
	return len(v.Segments) > 0
}

// listVideo lists the segments of the master's lightest H.264 variant.
func (f *fetcher) listVideo(ctx context.Context, master string) (video, error) {
	body, err := f.get(ctx, master, true)
	if err != nil {
		return video{}, fmt.Errorf("master: %w", err)
	}
	variant := master
	if !strings.Contains(string(body), "#EXTINF") {
		if variant, err = lightestVariant(master, string(body)); err != nil {
			return video{}, err
		}
		if body, err = f.get(ctx, variant, true); err != nil {
			return video{}, fmt.Errorf("variant: %w", err)
		}
	}
	base, err := url.Parse(variant)
	if err != nil {
		return video{}, err
	}
	var v video
	duration := 0.0
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			value, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			duration, _ = strconv.ParseFloat(strings.TrimSpace(value), 64)
		case strings.HasPrefix(line, "#EXT-X-KEY:") && !strings.Contains(line, "METHOD=NONE"):
			return video{}, errors.New("segments are encrypted")
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE"):
			return video{}, errors.New("segments are byte ranges")
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			uri := attributes(strings.TrimPrefix(line, "#EXT-X-MAP:"))["URI"]
			v.Init = resolve(base, uri)
		case line != "" && !strings.HasPrefix(line, "#"):
			v.Segments = append(v.Segments, videoSegment{Start: v.Duration, Duration: duration, url: resolve(base, line)})
			v.Duration += duration
		}
	}
	if len(v.Segments) == 0 {
		return video{}, errors.New("variant has no segments")
	}
	return v, scanner.Err()
}

func lightestVariant(master, body string) (string, error) {
	base, err := url.Parse(master)
	if err != nil {
		return "", err
	}
	best, bestBandwidth := "", int64(math.MaxInt64)
	var attrs map[string]string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			attrs = attributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
		case attrs != nil && line != "" && !strings.HasPrefix(line, "#"):
			bandwidth, _ := strconv.ParseInt(attrs["BANDWIDTH"], 10, 64)
			codecs := attrs["CODECS"]
			if (codecs == "" || strings.Contains(codecs, "avc1") || strings.Contains(codecs, "avc3")) && bandwidth < bestBandwidth {
				best, bestBandwidth = resolve(base, line), bandwidth
			}
			attrs = nil
		}
	}
	if best == "" {
		return "", errors.New("master has no H.264 variant")
	}
	return best, nil
}

func attributes(value string) map[string]string {
	result := map[string]string{}
	for len(value) > 0 {
		value = strings.TrimLeft(value, " ,")
		key, rest, ok := strings.Cut(value, "=")
		if !ok {
			break
		}
		var field string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				break
			}
			field, value = rest[1:end+1], rest[end+2:]
		} else {
			field, value, _ = strings.Cut(rest, ",")
		}
		result[strings.TrimSpace(key)] = field
	}
	return result
}

func resolve(base *url.URL, reference string) string {
	target, err := url.Parse(reference)
	if err != nil {
		return reference
	}
	return base.ResolveReference(target).String()
}

func extension(link, fallback string) string {
	parsed, err := url.Parse(link)
	if err != nil {
		return fallback
	}
	if ext := path.Ext(parsed.Path); ext != "" && len(ext) <= 5 {
		return ext
	}
	return fallback
}

// get loads a link; one to the CDN waits out the pause since the last.
func (f *fetcher) get(ctx context.Context, link string, cdn bool) ([]byte, error) {
	if cdn {
		if wait := f.pause - time.Since(f.last); wait > 0 {
			time.Sleep(wait)
		}
		defer func() { f.last = time.Now() }()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	if cdn {
		request.Header.Set("User-Agent", userAgent)
	}
	response, err := f.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s %s", redact(link), response.Status, response.Header.Get("X-VD"))
	}
	return body, nil
}

// redact keeps a link's host and path, not its signature.
func redact(link string) string {
	parsed, err := url.Parse(link)
	if err != nil {
		return "?"
	}
	return parsed.Host + parsed.Path
}

func readJSON(file string, value any) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func writeJSON(file string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(file, data, 0o644)
}
