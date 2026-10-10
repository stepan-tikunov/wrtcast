package provider

import (
	"bufio"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var audioIndexPattern = regexp.MustCompile(`[0-9]+$`)

func parseMaster(masterURL, body string, names []string, subtitles []SubtitleTrack) (MovieStream, error) {
	base, err := url.Parse(masterURL)
	if err != nil {
		return MovieStream{}, fmt.Errorf("parse master URL: %w", err)
	}

	stream := MovieStream{MasterPlaylistURL: masterURL, Subtitles: subtitles}
	seenAudio := make(map[string]bool)
	seenSubtitles := make(map[string]bool)
	for _, subtitle := range subtitles {
		seenSubtitles[subtitle.PlaylistURL] = true
	}

	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		attributes := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
		if attributes["URI"] == "" {
			continue
		}
		resolved := resolveReference(base, attributes["URI"])
		switch {
		case strings.EqualFold(attributes["TYPE"], "AUDIO"):
			trackID := attributes["NAME"]
			dedupeKey := trackID + "\x00" + attributes["LANGUAGE"]
			if seenAudio[dedupeKey] {
				continue
			}
			seenAudio[dedupeKey] = true
			name := trackID
			if match := audioIndexPattern.FindString(trackID); match != "" {
				index, _ := strconv.Atoi(match)
				if index >= 0 && index < len(names) {
					name = names[index]
				}
			}
			stream.AudioTracks = append(stream.AudioTracks, AudioTrack{
				ID:          trackID,
				Name:        name,
				Language:    attributes["LANGUAGE"],
				Default:     strings.EqualFold(attributes["DEFAULT"], "YES"),
				PlaylistURL: resolved,
			})
		case strings.EqualFold(attributes["TYPE"], "SUBTITLES"):
			if seenSubtitles[resolved] {
				continue
			}
			seenSubtitles[resolved] = true
			name := attributes["NAME"]
			stream.Subtitles = append(stream.Subtitles, SubtitleTrack{
				ID:          name,
				Name:        name,
				Language:    attributes["LANGUAGE"],
				Forced:      strings.EqualFold(attributes["FORCED"], "YES"),
				PlaylistURL: resolved,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return MovieStream{}, fmt.Errorf("scan HLS master: %w", err)
	}
	return stream, nil
}

func parseAttributes(value string) map[string]string {
	attributes := make(map[string]string)
	for len(value) > 0 {
		value = strings.TrimLeft(value, " ,\t")
		equals := strings.IndexByte(value, '=')
		if equals < 1 {
			break
		}
		key := strings.TrimSpace(value[:equals])
		value = value[equals+1:]

		var field string
		if strings.HasPrefix(value, `"`) {
			value = value[1:]
			end := strings.IndexByte(value, '"')
			if end < 0 {
				break
			}
			field = value[:end]
			value = value[end+1:]
		} else {
			end := strings.IndexByte(value, ',')
			if end < 0 {
				field = strings.TrimSpace(value)
				value = ""
			} else {
				field = strings.TrimSpace(value[:end])
				value = value[end+1:]
			}
		}
		attributes[key] = field
	}
	return attributes
}

func resolveReference(base *url.URL, reference string) string {
	target, err := url.Parse(reference)
	if err != nil {
		return reference
	}
	return base.ResolveReference(target).String()
}

func mediaPlaylistDuration(body string) (uint64, bool) {
	var total float64
	found := false
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		value := strings.TrimPrefix(line, "#EXTINF:")
		if comma := strings.IndexByte(value, ','); comma >= 0 {
			value = value[:comma]
		}
		duration, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || duration < 0 {
			continue
		}
		total += duration
		found = true
	}
	if !found {
		return 0, false
	}
	return uint64(math.Round(total)), true
}

func firstVariantURL(playlistURL, body string) (string, bool) {
	base, err := url.Parse(playlistURL)
	if err != nil {
		return "", false
	}
	wantsURI := false
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			wantsURI = true
		case wantsURI && line != "" && !strings.HasPrefix(line, "#"):
			reference, parseErr := url.Parse(line)
			if parseErr != nil {
				return "", false
			}
			resolved := base.ResolveReference(reference)
			if resolved.RawQuery == "" {
				resolved.RawQuery = base.RawQuery
			}
			return resolved.String(), true
		}
	}
	return "", false
}
