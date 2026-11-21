package provider

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// A quality's media playlist as served (see QualityPlaylist) names its
// fragments and init segments by short links of title-api's own,
// /v1/media/{id}/quality/{quality}/part/{index}/{name} and .../init/{index}/{name},
// not by the CDN's: its signed links run to some 2 KB each, and a playlist
// of 800 fragments of 1.7 MB took the router 2.5 s to rewrite and the
// receiver seconds to read. QualityPart tells the CDN's link of a short
// one, which is relayed as it always was (see RelayMedia); the CDN never
// sees the short links.

// shortPlaylist is a media playlist absorbed (see absorbPlaylist) with
// its fragments' and init segments' links made short, under prefix (the
// quality's, ending in /); it tells the init segments' CDN links in their
// order too.
func shortPlaylist(playlist []byte, fragments []string, prefix string) ([]byte, []string) {
	var out strings.Builder
	var inits []string
	initIndex := map[string]int{}
	next := 0
	for _, line := range strings.Split(strings.TrimSuffix(string(playlist), "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "#EXT-X-MAP:"):
			line = uriAttribute.ReplaceAllStringFunc(line, func(match string) string {
				link := uriAttribute.FindStringSubmatch(match)[1]
				index, ok := initIndex[link]
				if !ok {
					index = len(inits)
					initIndex[link] = index
					inits = append(inits, link)
				}
				return `URI="` + prefix + "init/" + strconv.Itoa(index) + "/" + partName(link, "init.mp4") + `"`
			})
		case strings.HasPrefix(trimmed, "#"):
		default:
			if next < len(fragments) && fragments[next] == trimmed {
				line = prefix + "part/" + strconv.Itoa(next) + "/" + partName(trimmed, "fragment")
			}
			next++
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return []byte(strings.TrimSuffix(out.String(), "\n") + "\n"), inits
}

var partNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// partName is the file name of a CDN link, for the short link's last part
// (the receiver and the router go by its extension), or fallback.
func partName(link, fallback string) string {
	target, err := url.Parse(link)
	if err != nil {
		return fallback
	}
	name := target.Path[strings.LastIndex(target.Path, "/")+1:]
	name = partNameUnsafe.ReplaceAllString(name, "")
	if name == "" || len(name) > 100 {
		return fallback
	}
	return name
}

// QualityPart is the CDN link of a short link of a quality's media
// playlist as last served: of its fragment index, or of its init segment
// index when init. ErrMasterNotFound for none, as after a restart, when
// the router asks for the master again.
func (c *Client) QualityPart(id, quality string, init bool, index int) (string, error) {
	entry, err := c.masters.entry(id)
	if err != nil {
		return "", err
	}
	if entry.source == nil {
		return "", ErrMasterNotFound
	}
	source := entry.source
	source.mu.Lock()
	level := source.levels[quality]
	source.mu.Unlock()
	if level == nil {
		return "", fmt.Errorf("%w: quality %q not loaded", ErrMasterNotFound, quality)
	}
	level.mu.Lock()
	defer level.mu.Unlock()
	links := level.fragments
	if init {
		links = level.inits
	}
	if index < 0 || index >= len(links) {
		return "", fmt.Errorf("%w: no part %d of quality %q", ErrMasterNotFound, index, quality)
	}
	return links[index], nil
}
