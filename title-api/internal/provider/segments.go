package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The relay keeps the alternative player's segments it passes on (see
// RelayMedia) on disk, and loads those of the next Ahead seconds of the
// quality the receiver plays before it asks for them, one at a time, as the
// page's hls.js would with a longer buffer: the receiver then gets them
// from the disk, and each whole segment, the receiver's or loaded ahead,
// goes to the Sink once (the music of the video is told from it a minute
// before it plays). A segment is loaded from the CDN once whoever asks for
// it first. A refusal of the CDN (403, 429, 451) or three failures in a row
// stop the loading ahead for segmentsPause; the segments kept are served on.
const (
	// Between two segments loaded ahead.
	aheadGap = time.Second
	// The loading ahead of a video ends when the receiver asked for none
	// of its segments for so long.
	aheadIdle = 10 * time.Minute
	// After a refusal of the CDN or segmentsFailures failures in a row, no
	// loading ahead for so long.
	segmentsPause    = 6 * time.Hour
	segmentsFailures = 3
	// How long a request of the receiver waits for a segment being loaded
	// ahead before asking the CDN itself.
	aheadWait = 20 * time.Second
)

// Video is the movie (season 0, episode "") or episode of a title (by its
// id) an alternative source (Source, its stream's name) plays.
type Video struct {
	Title   string
	Season  int32
	Episode string
	Source  string
}

// Segment is a whole segment of a video: from Start to End seconds of it,
// its init segment (fMP4) and its bytes.
type Segment struct {
	Video      Video
	Start, End float64
	Init, Data []byte
}

// SegmentOptions set up the segments kept: in Dir, Limit bytes at most
// (the least used go first), Ahead seconds loaded ahead of the receiver (0
// none), each whole segment told to Sink (nil none), which must not block.
type SegmentOptions struct {
	Dir   string
	Limit int64
	Ahead float64
	Sink  func(Segment)
}

// KeepSegments has the relay keep the segments it passes on (see
// RelayMedia). What Dir held is removed.
func (c *Client) KeepSegments(options SegmentOptions) error {
	if err := os.RemoveAll(options.Dir); err != nil {
		return err
	}
	if err := os.MkdirAll(options.Dir, 0o755); err != nil {
		return err
	}
	c.segments = &segmentCache{client: c, options: options, entries: map[string]*keptSegment{},
		runs: map[*alternativeSource]*aheadRun{}}
	return nil
}

type segmentCache struct {
	client  *Client
	options SegmentOptions

	mu       sync.Mutex
	entries  map[string]*keptSegment
	total    int64
	serial   int64
	runs     map[*alternativeSource]*aheadRun
	paused   time.Time // no loading ahead until then
	failures int
}

// keptSegment is a segment on disk, or one being loaded until done closes
// (ok tells whether it was).
type keptSegment struct {
	path        string
	size        int64
	contentType string
	used        time.Time
	done        chan struct{}
	ok          bool
}

// segmentPlace is where a URL of a source is on its media playlists: the
// quality and the index of its fragment, or its init segment.
type segmentPlace struct {
	quality string
	index   int
	init    bool
	url     string
	start   float64
	end     float64
	initURL string
}

func (p segmentPlace) key(source *alternativeSource) string {
	if p.init {
		return source.id + "|" + p.quality + "|init"
	}
	return source.id + "|" + p.quality + "|" + strconv.Itoa(p.index)
}

// placeOf is where rawURL (as remapped) is on the source's media playlists
// loaded; false if on none.
func (source *alternativeSource) placeOf(rawURL string) (segmentPlace, bool) {
	fragment, isFragment := source.media.place(rawURL)
	source.media.mu.Lock()
	isInit := source.media.inits[rawURL]
	source.media.mu.Unlock()
	if !isFragment && !isInit {
		return segmentPlace{}, false
	}
	source.mu.Lock()
	levels := make(map[string]*qualityLevel, len(source.levels))
	for quality, level := range source.levels {
		levels[quality] = level
	}
	source.mu.Unlock()
	for quality, level := range levels {
		level.mu.Lock()
		fragments := level.fragments
		level.mu.Unlock()
		if isFragment {
			if fragment.index < len(fragments) && fragments[fragment.index] == rawURL {
				return segmentPlace{quality: quality, index: fragment.index, url: rawURL,
					start: fragment.start, end: fragment.end, initURL: fragment.init}, true
			}
			continue
		}
		if len(fragments) > 0 {
			if first, ok := source.media.place(fragments[0]); ok && first.init == rawURL {
				return segmentPlace{quality: quality, init: true, url: rawURL}, true
			}
		}
	}
	return segmentPlace{}, false
}

// RelayMedia is FetchMedia, but for the segments of an alternative
// source's media playlists kept (see KeepSegments): one kept, or being
// loaded ahead, is served from the disk, one not is kept as it passes on;
// a fragment the receiver asks for tells where it plays, which the loading
// ahead follows.
func (c *Client) RelayMedia(ctx context.Context, id, rawURL string, preview bool) (*http.Response, error) {
	cache := c.segments
	if cache == nil || preview {
		return c.FetchMedia(ctx, id, rawURL, preview)
	}
	entry, err := c.masters.entry(id)
	if err != nil || entry.source == nil {
		return c.FetchMedia(ctx, id, rawURL, preview)
	}
	source := entry.source
	target := rawURL
	if mapped := source.media.mapped(rawURL); mapped != "" {
		target = mapped
	}
	place, ok := source.placeOf(target)
	if !ok {
		return c.FetchMedia(ctx, id, rawURL, preview)
	}
	kind := "fragment"
	if place.init {
		kind = "init"
	} else {
		cache.played(source, place)
	}
	if response := cache.serve(ctx, place.key(source)); response != nil {
		source.tap(response, kind, target)
		return response, nil
	}
	response, err := c.FetchMedia(ctx, id, rawURL, preview)
	if err != nil || response.StatusCode != http.StatusOK {
		return response, err
	}
	cache.keep(source, place, response)
	return response, nil
}

// serve is the segment kept as key, waiting for one being loaded; nil if
// none.
func (cache *segmentCache) serve(ctx context.Context, key string) *http.Response {
	cache.mu.Lock()
	kept := cache.entries[key]
	cache.mu.Unlock()
	if kept == nil {
		return nil
	}
	select {
	case <-kept.done:
	case <-ctx.Done():
		return nil
	case <-time.After(aheadWait):
		return nil
	}
	if !kept.ok {
		return nil
	}
	file, err := os.Open(kept.path)
	if err != nil {
		return nil
	}
	cache.mu.Lock()
	kept.used = time.Now()
	cache.mu.Unlock()
	header := http.Header{}
	if kept.contentType != "" {
		header.Set("Content-Type", kept.contentType)
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: header, Body: file, ContentLength: kept.size}
}

// reserve is a new entry for key being loaded; nil if there is one.
func (cache *segmentCache) reserve(key string) *keptSegment {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if kept := cache.entries[key]; kept != nil {
		select {
		case <-kept.done:
			if kept.ok {
				return nil
			}
		default:
			return nil
		}
	}
	cache.serial++
	kept := &keptSegment{path: filepath.Join(cache.options.Dir, strconv.FormatInt(cache.serial, 10)), done: make(chan struct{})}
	cache.entries[key] = kept
	return kept
}

// finish ends the loading of an entry: kept with data's bytes if ok (and
// told to the sink), else dropped.
func (cache *segmentCache) finish(source *alternativeSource, place segmentPlace, kept *keptSegment, data []byte, ok bool) {
	key := place.key(source)
	if ok {
		if err := os.WriteFile(kept.path, data, 0o644); err != nil {
			log.Printf("segments: %v", err)
			ok = false
		}
	}
	cache.mu.Lock()
	kept.ok, kept.size, kept.used = ok, int64(len(data)), time.Now()
	if ok {
		cache.total += kept.size
		cache.evict()
	} else if cache.entries[key] == kept {
		delete(cache.entries, key)
	}
	cache.mu.Unlock()
	close(kept.done)
	if ok && !place.init && cache.options.Sink != nil {
		cache.tell(source, place, data)
	}
}

// evict removes the least used segments kept beyond the limit; under mu.
func (cache *segmentCache) evict() {
	for cache.total > cache.options.Limit {
		var oldest string
		for key, kept := range cache.entries {
			// Init segments, small, stay: each fragment wants its own.
			if !kept.ok || strings.HasSuffix(key, "|init") {
				continue
			}
			if oldest == "" || kept.used.Before(cache.entries[oldest].used) {
				oldest = key
			}
		}
		if oldest == "" {
			return
		}
		kept := cache.entries[oldest]
		_ = os.Remove(kept.path)
		cache.total -= kept.size
		delete(cache.entries, oldest)
	}
}

// tell gives the sink a whole fragment of a source, with its init segment
// if kept.
func (cache *segmentCache) tell(source *alternativeSource, place segmentPlace, data []byte) {
	value, ok := cache.client.videos.Load(source.id)
	if !ok {
		return
	}
	var init []byte
	if place.initURL != "" {
		cache.mu.Lock()
		kept := cache.entries[segmentPlace{quality: place.quality, init: true}.key(source)]
		cache.mu.Unlock()
		if kept == nil || !isDone(kept) || !kept.ok {
			return
		}
		var err error
		if init, err = os.ReadFile(kept.path); err != nil {
			return
		}
	}
	cache.options.Sink(Segment{Video: value.(Video), Start: place.start, End: place.end, Init: init, Data: data})
}

func isDone(kept *keptSegment) bool {
	select {
	case <-kept.done:
		return true
	default:
		return false
	}
}

// keep keeps a segment the receiver gets as its body passes on.
func (cache *segmentCache) keep(source *alternativeSource, place segmentPlace, response *http.Response) {
	kept := cache.reserve(place.key(source))
	if kept == nil {
		return
	}
	response.Body = &keptBody{ReadCloser: response.Body, done: func(data []byte, ok bool) {
		kept.contentType = response.Header.Get("Content-Type")
		cache.finish(source, place, kept, data, ok)
	}}
}

// keptBody gathers a body as it is read, and tells it once it is over:
// whole, or not if closed before its end.
type keptBody struct {
	io.ReadCloser
	data []byte
	once sync.Once
	done func([]byte, bool)
}

func (body *keptBody) Read(buffer []byte) (int, error) {
	n, err := body.ReadCloser.Read(buffer)
	body.data = append(body.data, buffer[:n]...)
	if err == io.EOF {
		body.once.Do(func() { body.done(body.data, true) })
	} else if err != nil {
		body.once.Do(func() { body.done(nil, false) })
	}
	return n, err
}

func (body *keptBody) Close() error {
	body.once.Do(func() { body.done(nil, false) })
	return body.ReadCloser.Close()
}

// aheadRun loads a source's segments ahead of the receiver: of the quality
// and from the fragment it asked for last.
type aheadRun struct {
	quality string
	index   int
	seen    time.Time
	wake    chan struct{}
}

// played tells that the receiver asked for a fragment of a source: the
// loading ahead goes on from there.
func (cache *segmentCache) played(source *alternativeSource, place segmentPlace) {
	if cache.options.Ahead <= 0 {
		return
	}
	cache.mu.Lock()
	run := cache.runs[source]
	fresh := run == nil
	if fresh {
		run = &aheadRun{wake: make(chan struct{}, 1)}
		cache.runs[source] = run
	}
	run.quality, run.index, run.seen = place.quality, place.index, time.Now()
	cache.mu.Unlock()
	if fresh {
		go cache.ahead(source, run)
	}
	select {
	case run.wake <- struct{}{}:
	default:
	}
}

// ahead loads the segments of a source ahead of the receiver, one at a
// time, until it asked for none for aheadIdle.
func (cache *segmentCache) ahead(source *alternativeSource, run *aheadRun) {
	ctx := cache.client.sessionsCtx
	defer func() {
		cache.mu.Lock()
		delete(cache.runs, source)
		cache.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-run.wake:
		case <-time.After(time.Minute):
		}
		for {
			cache.mu.Lock()
			idle, paused := time.Since(run.seen) > aheadIdle, time.Now().Before(cache.paused)
			quality, index := run.quality, run.index
			cache.mu.Unlock()
			if idle {
				return
			}
			if paused {
				break
			}
			place, ok := cache.nextAhead(source, quality, index)
			if !ok {
				break
			}
			err := cache.load(ctx, source, place)
			cache.mu.Lock()
			if err == nil {
				cache.failures = 0
			} else {
				cache.failures++
				var refused *alternativeStatusError
				code := 0
				if errors.As(err, &refused) {
					code = refused.code
				}
				if code == http.StatusForbidden || code == http.StatusTooManyRequests ||
					code == http.StatusUnavailableForLegalReasons || cache.failures >= segmentsFailures {
					cache.paused = time.Now().Add(segmentsPause)
					log.Printf("segments: no loading ahead for %s: %v", segmentsPause, err)
				}
			}
			cache.mu.Unlock()
			if err != nil {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(aheadGap):
			}
		}
	}
}

// nextAhead is the next segment to load ahead of the fragment index of the
// quality: the init segment if not kept, else the first fragment within
// Ahead seconds after it not kept or being loaded; false for none.
func (cache *segmentCache) nextAhead(source *alternativeSource, quality string, index int) (segmentPlace, bool) {
	source.mu.Lock()
	level := source.levels[quality]
	source.mu.Unlock()
	if level == nil {
		return segmentPlace{}, false
	}
	level.mu.Lock()
	fragments := level.fragments
	level.mu.Unlock()
	if index >= len(fragments) {
		return segmentPlace{}, false
	}
	played, ok := source.media.place(fragments[index])
	if !ok {
		return segmentPlace{}, false
	}
	has := func(p segmentPlace) bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		kept := cache.entries[p.key(source)]
		return kept != nil && (!isDone(kept) || kept.ok)
	}
	if played.init != "" {
		init := segmentPlace{quality: quality, init: true, url: played.init}
		if !has(init) {
			return init, true
		}
	}
	for i := index + 1; i < len(fragments); i++ {
		f, ok := source.media.place(fragments[i])
		if !ok || f.start >= played.end+cache.options.Ahead {
			break
		}
		p := segmentPlace{quality: quality, index: i, url: fragments[i], start: f.start, end: f.end, initURL: f.init}
		if !has(p) {
			return p, true
		}
	}
	return segmentPlace{}, false
}

// load loads a segment ahead from the CDN, as hls.js loads one (see
// FetchMedia): a fragment counts for the bandwidth estimate, a failure is
// told to the view, a block blocks the relay.
func (cache *segmentCache) load(ctx context.Context, source *alternativeSource, place segmentPlace) error {
	kept := cache.reserve(place.key(source))
	if kept == nil {
		return nil
	}
	kind := "fragment"
	if place.init {
		kind = "init"
	}
	data, contentType, err := func() ([]byte, string, error) {
		if vd, blocked := source.media.blocked(); blocked {
			return nil, "", &alternativeStatusError{status: "403 Forbidden", code: http.StatusForbidden, vd: vd}
		}
		started := time.Now()
		response, err := source.cdnRequest(ctx, place.url, false)
		if err != nil {
			source.loadFailed(kind, place.url, 0, started, err)
			return nil, "", err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			vd := response.Header.Get("X-VD")
			if response.StatusCode == http.StatusForbidden && blockedVD(vd) {
				source.media.block(vd)
			}
			refused := &alternativeStatusError{status: response.Status, code: response.StatusCode, vd: vd}
			source.loadFailed(kind, place.url, response.StatusCode, started, refused)
			return nil, "", refused
		}
		first := time.Now()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			source.loadFailed(kind, place.url, 0, started, err)
			return nil, "", fmt.Errorf("segment %s: %w", strings.Split(place.url, "?")[0], err)
		}
		if !place.init {
			source.media.fragmentLoaded(place.url, int64(len(data)), time.Since(first))
		}
		return data, response.Header.Get("Content-Type"), nil
	}()
	kept.contentType = contentType
	cache.finish(source, place, kept, data, err == nil)
	return err
}
