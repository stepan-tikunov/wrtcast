package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The sections are marked on the primary player's video, and the
// alternative player's of the same episode is another encode: it may begin
// a second or two earlier or later, end with a logo, or run at another
// frame rate. So its sections are moved by matching the two videos' frames
// (see matchOffset): a stretch of the alternative's that the receiver
// loads anyway, through the relay, against the primary's lightest variant
// around the same time, which title-api loads itself. Each match is an
// anchor, a primary time and the offset there; a first one is looked for
// as soon as the playback starts, more further on, apart from the others,
// so that the credits are moved by an anchor near them should the encodes
// drift or one be cut differently. The anchors are kept per episode and
// dub, also across restarts (AlignmentStore). The encodes of one release
// begin alike, so an episode of a series not aligned yet has its sections
// moved by the offset its last episodes found in the meantime, kept as
// long (see seriesPrior), and its own anchors replace that as its playback
// finds them. Once a playback found its first anchor, the beginning of the
// next episode's primary video is decoded ahead, so that one needs only
// its own frames.

const (
	// Seconds of the alternative's video an anchor is matched from, at
	// least: two of its usual fragments.
	alignWindow = 8.0
	// The offset of a first anchor is looked for this far either way, of
	// one that has a neighbour this far from that one's.
	alignSearch = 20.0
	alignNear   = 3.0
	// A further anchor is taken this far from the others, up to so many.
	alignApart   = 600.0
	alignAnchors = 3
	// Failed matches of a video before it is given up on until a restart.
	alignAttempts = 6
	// While a playback has no anchor yet, the router holds a skip back for
	// so long at most, and asks for the headers this often (see headers).
	alignPending    = 20 * time.Second
	pendingAlignTTL = 2 * time.Second
	// The bytes of a fragment kept for a match, at most.
	alignTapLimit = 32 << 20
	// Seconds of the next episode's primary video decoded ahead.
	alignPrefetch = 45.0
	// Matches and decodes at once.
	alignJobs = 2
	// How long one match may take, the primary's fetches included.
	alignTimeout = 90 * time.Second
	// The videos aligned and the primary videos decoded kept in memory, the
	// least recently used dropped beyond.
	alignKept   = 64
	primaryKept = 8
	// A primary video's decoded segments kept, the earliest dropped beyond.
	primarySegmentsKept = 48
)

// The frame rates the encodes of one video differ by, as the ratios of
// their lengths: the same frames played at 24, 25 or 24000/1001 a second.
var alignScales = []float64{1, 24.0 / 25, 25.0 / 24, 24000.0 / 1001 / 25, 25 / (24000.0 / 1001)}

// sectionsReference is the primary player's video the sections of a movie
// or an episode are marked on.
type sectionsReference struct {
	title    string
	key      string // the title, and the season and the episode of one
	master   string
	sections []Section
	duration float64
	// The next episode's, whose primary video is decoded ahead.
	next *sectionsReference
}

func referenceKey(titleID string, season int32, episode string) string {
	if episode == "" {
		return titleID
	}
	return titleID + "|" + strconv.Itoa(int(season)) + "|" + episode
}

// anchor is a time of the primary video and the offset of the
// alternative's there: alt = scale·primary + offset.
type anchor struct {
	Primary float64 `json:"p"`
	Offset  float64 `json:"o"`
}

// alignment is what is known of how an alternative video's times relate to
// the primary's: the ratio of their lengths (zero while not yet worked
// out) and the anchors, by primary time.
type alignment struct {
	Scale   float64  `json:"scale"`
	Anchors []anchor `json:"anchors"`
}

// at is the alternative's time of a primary time: the offsets of the
// anchors around it interpolated, the nearest one's beyond them.
func (a alignment) at(primary float64) float64 {
	anchors := a.Anchors
	offset := anchors[0].Offset
	switch last := len(anchors) - 1; {
	case primary >= anchors[last].Primary:
		offset = anchors[last].Offset
	case primary > anchors[0].Primary:
		next := sort.Search(len(anchors), func(index int) bool { return anchors[index].Primary >= primary })
		left, right := anchors[next-1], anchors[next]
		share := (primary - left.Primary) / (right.Primary - left.Primary)
		offset = left.Offset + share*(right.Offset-left.Offset)
	}
	return a.Scale*primary + offset
}

// sections are the reference's sections on the alternative video, of
// duration seconds (zero when unknown). A section from the very start
// still starts there, and one to the primary video's end ends with the
// alternative's, whatever logo either has there.
func (a alignment) sections(ref sectionsReference, duration float64) []Section {
	var result []Section
	for _, section := range ref.sections {
		start, end := a.at(section.Start), a.at(section.End)
		if section.Start <= 0.5 {
			start = 0
		}
		if duration > 0 && ref.duration > 0 && section.End >= ref.duration-1.5 {
			end = duration
		}
		start = math.Max(start, 0)
		if duration > 0 {
			end = math.Min(end, duration)
		}
		if end-start >= 1 {
			result = append(result, Section{Kind: section.Kind, Start: start, End: end, Skippable: section.Skippable})
		}
	}
	return result
}

// seriesPrior is how the alternative player's videos of a dub of a title
// relate to the primary's lately: the offsets the first anchors of its last
// few episodes found, oldest first, and the ratio of their lengths.
type seriesPrior struct {
	Scale   float64         `json:"scale"`
	Offsets []episodeOffset `json:"offsets"`
}

type episodeOffset struct {
	Episode string  `json:"episode"`
	Offset  float64 `json:"offset"`
}

const (
	// The episodes a prior keeps, and how far apart their offsets may be
	// for the latest to be trusted for another episode.
	priorEpisodes = 3
	priorSpread   = 1.0
)

func priorKey(ref sectionsReference, label string) string {
	return "prior:" + ref.title + "|" + strings.TrimSpace(label)
}

// predicted is the alignment of an episode not aligned yet: the latest
// episode's offset, if the episodes kept agree.
func (p seriesPrior) predicted() (alignment, bool) {
	if len(p.Offsets) == 0 || p.Scale <= 0 {
		return alignment{}, false
	}
	low, high := math.Inf(1), math.Inf(-1)
	for _, o := range p.Offsets {
		low, high = math.Min(low, o.Offset), math.Max(high, o.Offset)
	}
	if high-low > priorSpread {
		return alignment{}, false
	}
	return alignment{Scale: p.Scale, Anchors: []anchor{{Offset: p.Offsets[len(p.Offsets)-1].Offset}}}, true
}

// with is the prior with an episode's offset in place of any it had.
func (p seriesPrior) with(episode string, scale, offset float64) seriesPrior {
	var kept []episodeOffset
	for _, o := range p.Offsets {
		if o.Episode != episode {
			kept = append(kept, o)
		}
	}
	kept = append(kept, episodeOffset{Episode: episode, Offset: offset})
	if len(kept) > priorEpisodes {
		kept = kept[len(kept)-priorEpisodes:]
	}
	return seriesPrior{Scale: scale, Offsets: kept}
}

// lengthScale is the ratio of the alternative video's length to the
// primary's, as one of alignScales, the nearest within 2%; zero for none.
func lengthScale(alternative, primary float64) float64 {
	if alternative <= 0 || primary <= 0 {
		return 0
	}
	ratio, best := alternative/primary, 0.0
	for _, scale := range alignScales {
		if math.Abs(ratio-scale) <= 0.02 && (best == 0 || math.Abs(ratio-scale) < math.Abs(ratio-best)) {
			best = scale
		}
	}
	return best
}

// videoAlignment is the alignment of one dub of the alternative player's
// video of a movie or an episode with the primary's, and the fragments of
// it tapped from the relay for the next match.
type videoAlignment struct {
	client *Client
	key    string

	mu         sync.Mutex
	ref        sectionsReference
	known      alignment
	prior      seriesPrior // the title's, for a video not aligned yet
	busy       bool        // a match runs
	failures   int
	prefetched bool              // the next episode's primary video is decoded ahead
	inits      map[string][]byte // the init segments relayed, by URL
	lastInit   []byte
	tapped     []tappedFragment
	used       time.Time
}

// tappedFragment is a fragment relayed: its place in the playlist, its
// init segment's URL (none for MPEG-TS) and its bytes.
type tappedFragment struct {
	start, end float64
	init       string
	data       []byte
}

// alignmentFor is the alignment of the dub of label with ref's video, as
// kept in memory or in the store.
func (c *Client) alignmentFor(ref sectionsReference, label string) *videoAlignment {
	key := ref.key + "|" + strings.TrimSpace(label)
	c.alignMu.Lock()
	v := c.alignments[key]
	if v == nil {
		v = &videoAlignment{client: c, key: key, inits: map[string][]byte{}}
		c.alignments[key] = v
		evictLeastUsed(c.alignments, alignKept, func(v *videoAlignment) time.Time { return v.used })
	}
	c.alignMu.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	fresh := v.used.IsZero()
	if v.ref.next == nil || ref.next == nil || v.ref.next.key != ref.next.key {
		v.prefetched = false
	}
	v.ref, v.used = ref, time.Now()
	if fresh {
		if text := c.loadAligned(key); text != "" && json.Unmarshal([]byte(text), &v.known) != nil {
			v.known = alignment{}
		}
	}
	// Other episodes may have been aligned since.
	if len(v.known.Anchors) == 0 {
		v.prior = seriesPrior{}
		if text := c.loadAligned(priorKey(ref, label)); text != "" && json.Unmarshal([]byte(text), &v.prior) != nil {
			v.prior = seriesPrior{}
		}
	}
	return v
}

func (c *Client) loadAligned(key string) string {
	if c.alignmentStore == nil {
		return ""
	}
	text, err := c.alignmentStore.LoadAlignment(context.Background(), key)
	if err != nil {
		log.Printf("align %s: %v", key, err)
	}
	return text
}

func (c *Client) saveAligned(key string, value any) {
	if c.alignmentStore == nil {
		return
	}
	text, _ := json.Marshal(value)
	if err := c.alignmentStore.SaveAlignment(context.Background(), key, string(text)); err != nil {
		log.Printf("align %s: %v", key, err)
	}
}

func evictLeastUsed[V any](entries map[string]V, limit int, used func(V) time.Time) {
	for len(entries) > limit {
		var oldest string
		for key, entry := range entries {
			if oldest == "" || used(entry).Before(used(entries[oldest])) {
				oldest = key
			}
		}
		delete(entries, oldest)
	}
}

// alignStreams gives the alternative player's streams of a movie or an
// episode the alignment of their video with ref's.
func (c *Client) alignStreams(streams []MovieStream, ref sectionsReference) {
	if ref.master == "" || len(ref.sections) == 0 {
		return
	}
	prefix := c.mediaBaseURL + "/v1/media/"
	for _, stream := range streams {
		id, ok := strings.CutPrefix(stream.MasterPlaylistURL, prefix)
		if !ok {
			continue
		}
		entry, err := c.masters.entry(strings.TrimSuffix(id, "/master.m3u8"))
		if err != nil || entry.source == nil {
			continue
		}
		v := c.alignmentFor(ref, entry.source.label)
		entry.source.mu.Lock()
		entry.source.alignment = v
		entry.source.mu.Unlock()
	}
}

// state is the sections on the alternative video, of duration seconds,
// once there is an anchor, else as the title's prior predicts them; else
// whether an anchor is still looked for within alignPending of since, the
// playback's start. A playback of a video aligned before decodes the next
// episode's ahead too.
func (v *videoAlignment) state(duration float64, since time.Time) ([]Section, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.known.Anchors) > 0 && v.known.Scale > 0 {
		v.prefetchNext()
		return v.known.sections(v.ref, duration), false
	}
	if predicted, ok := v.prior.predicted(); ok {
		return predicted.sections(v.ref, duration), false
	}
	return nil, v.failures < alignAttempts && ffmpegPath() != "" && time.Since(since) < alignPending
}

// wants tells whether a fragment from start would serve a match: no match
// runs, more anchors are wanted, and none is near.
func (v *videoAlignment) wants(start float64) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.busy || len(v.known.Anchors) >= alignAnchors || v.failures >= alignAttempts || ffmpegPath() == "" {
		return false
	}
	scale := v.known.Scale
	if scale == 0 {
		scale = 1
	}
	for _, a := range v.known.Anchors {
		if math.Abs(scale*a.Primary+a.Offset-start) < alignApart {
			return false
		}
	}
	return true
}

func (v *videoAlignment) tapInit(rawURL string, data []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.inits[rawURL], v.lastInit = data, data
}

// tapFragment keeps a fragment relayed and, once the ones kept make a
// stretch of alignWindow seconds with it, runs a match of that stretch.
// duration is the alternative video's.
func (v *videoAlignment) tapFragment(fragment tappedFragment, duration float64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.busy {
		return
	}
	kept := []tappedFragment{fragment}
	for _, other := range v.tapped {
		if other.start != fragment.start && math.Abs(other.start-fragment.start) < 60 {
			kept = append(kept, other)
		}
	}
	sort.Slice(kept, func(left, right int) bool { return kept[left].start < kept[right].start })
	if len(kept) > 6 {
		kept = kept[len(kept)-6:]
	}
	v.tapped = kept
	run := fragmentRun(kept, fragment)
	if run[len(run)-1].end-run[0].start < alignWindow {
		return
	}
	var init []byte
	if fragment.init != "" {
		if init = v.inits[fragment.init]; init == nil {
			init = v.lastInit
		}
		if init == nil {
			return
		}
	}
	v.tapped, v.busy = nil, true
	go v.match(run, init, duration)
}

// fragmentRun is the run of fragments one after another, of one init
// segment, among kept (in order) that holds fragment.
func fragmentRun(kept []tappedFragment, fragment tappedFragment) []tappedFragment {
	at := sort.Search(len(kept), func(index int) bool { return kept[index].start >= fragment.start })
	first, last := at, at
	for first > 0 && math.Abs(kept[first-1].end-kept[first].start) < 0.1 && kept[first-1].init == fragment.init {
		first--
	}
	for last+1 < len(kept) && math.Abs(kept[last].end-kept[last+1].start) < 0.1 && kept[last+1].init == fragment.init {
		last++
	}
	return append([]tappedFragment(nil), kept[first:last+1]...)
}

// match finds an anchor in a stretch of fragments: their frames against
// the primary's around the same time, within alignSearch of no offset or
// alignNear of the nearest anchor's.
func (v *videoAlignment) match(run []tappedFragment, init []byte, duration float64) {
	c := v.client
	v.mu.Lock()
	ref, known := v.ref, v.known
	v.mu.Unlock()
	found, err := func() (anchor, error) {
		ctx, cancel := context.WithTimeout(c.sessionsCtx, alignTimeout)
		defer cancel()
		scale := known.Scale
		if scale == 0 && ref.duration <= 0 {
			scale = 1
		}
		if scale == 0 {
			if scale = lengthScale(duration, ref.duration); scale == 0 {
				return anchor{}, fmt.Errorf("the videos' lengths %.1f and %.1f differ by more than a frame rate", duration, ref.duration)
			}
		}
		start, end := run[0].start, run[len(run)-1].end
		center, radius := 0.0, alignSearch
		if len(known.Anchors) > 0 {
			nearest := known.Anchors[0]
			for _, a := range known.Anchors {
				if math.Abs(a.Primary-start/scale) < math.Abs(nearest.Primary-start/scale) {
					nearest = a
				}
			}
			center, radius = nearest.Offset, alignNear
		}
		data := append([]byte(nil), init...)
		for _, fragment := range run {
			data = append(data, fragment.data...)
		}
		if err := c.acquireAlignSlot(ctx); err != nil {
			return anchor{}, err
		}
		alt, err := decodeFrames(ctx, data, start)
		c.releaseAlignSlot()
		if err != nil {
			return anchor{}, fmt.Errorf("alternative frames: %w", err)
		}
		low := math.Max(0, (start-center-radius)/scale-0.5)
		primary, err := c.primaryVideo(ref).frames(ctx, low, (end-center+radius)/scale+0.5)
		if err != nil {
			return anchor{}, fmt.Errorf("primary frames: %w", err)
		}
		result, err := matchOffset(alt, primary, scale, center-radius, center+radius)
		if err != nil {
			return anchor{}, err
		}
		if !result.confident() {
			return anchor{}, fmt.Errorf("no match at %.0f s: best offset %+.2f s scores %.3f, the next best %.3f, over %d frames",
				start, result.offset, result.score, result.runnerUp, result.compared)
		}
		log.Printf("align %s: offset %+.2f s at %.0f s (score %.3f, the next best %.3f, %d frames)",
			v.key, result.offset, start, result.score, result.runnerUp, result.compared)
		known.Scale = scale
		return anchor{Primary: ((start+end)/2 - result.offset) / scale, Offset: result.offset}, nil
	}()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.busy = false
	if err != nil {
		v.failures++
		log.Printf("align %s: %v", v.key, err)
		return
	}
	first := len(v.known.Anchors) == 0
	v.known.Scale = known.Scale
	v.known.Anchors = append(v.known.Anchors, found)
	sort.Slice(v.known.Anchors, func(left, right int) bool { return v.known.Anchors[left].Primary < v.known.Anchors[right].Primary })
	c.saveAligned(v.key, v.known)
	if first {
		// As it is now: another episode may have been aligned meanwhile.
		key := priorKey(ref, strings.TrimPrefix(v.key, ref.key+"|"))
		var prior seriesPrior
		if text := c.loadAligned(key); text == "" || json.Unmarshal([]byte(text), &prior) != nil {
			prior = seriesPrior{}
		}
		c.saveAligned(key, prior.with(ref.key, known.Scale, found.Offset))
		v.prefetchNext()
	}
}

// prefetchNext decodes the beginning of the next episode's primary video,
// once per reference; under v.mu.
func (v *videoAlignment) prefetchNext() {
	if v.prefetched || v.ref.next == nil {
		return
	}
	v.prefetched = true
	go v.client.prefetchPrimary(*v.ref.next)
}

func (c *Client) acquireAlignSlot(ctx context.Context) error {
	select {
	case c.alignSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) releaseAlignSlot() { <-c.alignSlots }

// prefetchPrimary decodes the beginning of an episode's primary video, for
// the match its playback starts with.
func (c *Client) prefetchPrimary(ref sectionsReference) {
	if ref.master == "" || len(ref.sections) == 0 || ffmpegPath() == "" {
		return
	}
	ctx, cancel := context.WithTimeout(c.sessionsCtx, alignTimeout)
	defer cancel()
	if _, err := c.primaryVideo(ref).frames(ctx, 0, alignPrefetch); err != nil {
		log.Printf("align %s: decode ahead: %v", ref.key, err)
	}
}

// primaryVideo is the lightest variant of a primary player's video: its
// segments and the frames of those decoded.
type primaryVideo struct {
	client *Client
	key    string

	fetching sync.Mutex // one fetch at a time; guards the segments and the frames
	segments []hlsSegment
	decoded  map[int]frames

	mu     sync.Mutex
	master string
	used   time.Time
}

// hlsSegment is a segment of a media playlist: its URL, its place and its
// init segment's URL (none for MPEG-TS).
type hlsSegment struct {
	url             string
	start, duration float64
	init            string
}

func (c *Client) primaryVideo(ref sectionsReference) *primaryVideo {
	c.alignMu.Lock()
	defer c.alignMu.Unlock()
	video := c.primaries[ref.key]
	if video == nil {
		video = &primaryVideo{client: c, key: ref.key, decoded: map[int]frames{}}
		c.primaries[ref.key] = video
		evictLeastUsed(c.primaries, primaryKept, func(v *primaryVideo) time.Time { return v.lastUsed() })
	}
	video.mu.Lock()
	// The segments listed under an earlier master stay good for days.
	video.master, video.used = ref.master, time.Now()
	video.mu.Unlock()
	return video
}

func (video *primaryVideo) lastUsed() time.Time {
	video.mu.Lock()
	defer video.mu.Unlock()
	return video.used
}

// frames are the video's frames from low to high seconds, of the segments
// over them, each fetched and decoded once. A segment that fails to load
// has the playlist loaded again from the master, once.
func (video *primaryVideo) frames(ctx context.Context, low, high float64) (frames, error) {
	video.fetching.Lock()
	defer video.fetching.Unlock()
	c := video.client
	current, err := c.currentSession()
	if err != nil {
		return frames{}, err
	}
	reloaded := false
	if video.segments == nil {
		if err := video.load(ctx, current); err != nil {
			return frames{}, err
		}
		reloaded = true
	}
	var parts []frames
	inits := map[string][]byte{}
	for index, segment := range video.segments {
		if segment.start+segment.duration <= low || segment.start >= high {
			continue
		}
		if decoded, ok := video.decoded[index]; ok {
			parts = append(parts, decoded)
			continue
		}
		data, err := video.fetch(ctx, current, segment, inits)
		if err != nil && !reloaded && ctx.Err() == nil {
			if err = video.load(ctx, current); err != nil {
				return frames{}, err
			}
			reloaded = true
			segment = video.segments[min(index, len(video.segments)-1)]
			data, err = video.fetch(ctx, current, segment, inits)
		}
		if err != nil {
			return frames{}, err
		}
		if err := c.acquireAlignSlot(ctx); err != nil {
			return frames{}, err
		}
		decoded, err := decodeFrames(ctx, data, segment.start)
		c.releaseAlignSlot()
		if err != nil {
			return frames{}, fmt.Errorf("segment at %.0f s: %w", segment.start, err)
		}
		video.decoded[index] = decoded
		parts = append(parts, decoded)
	}
	for len(video.decoded) > primarySegmentsKept {
		earliest := -1
		for index := range video.decoded {
			if earliest < 0 || index < earliest {
				earliest = index
			}
		}
		delete(video.decoded, earliest)
	}
	if len(parts) == 0 {
		return frames{}, fmt.Errorf("no segment from %.0f to %.0f s", low, high)
	}
	return appendFrames(parts...), nil
}

// fetch loads a segment, its init segment first, if any.
func (video *primaryVideo) fetch(ctx context.Context, current *session, segment hlsSegment, inits map[string][]byte) ([]byte, error) {
	var data []byte
	if segment.init != "" {
		init, ok := inits[segment.init]
		if !ok {
			body, _, err := video.client.get(ctx, current, segment.init, "")
			if err != nil {
				return nil, fmt.Errorf("init segment: %w", err)
			}
			init, inits[segment.init] = body, body
		}
		data = append(data, init...)
	}
	body, _, err := video.client.get(ctx, current, segment.url, "")
	if err != nil {
		return nil, fmt.Errorf("segment at %.0f s: %w", segment.start, err)
	}
	return append(data, body...), nil
}

// load lists the segments of the master's lightest variant. Should they
// not be the ones listed before, the frames decoded go.
func (video *primaryVideo) load(ctx context.Context, current *session) error {
	c := video.client
	video.mu.Lock()
	master := video.master
	video.mu.Unlock()
	body, _, err := c.get(ctx, current, master, "")
	if err != nil {
		return fmt.Errorf("master: %w", err)
	}
	variant, err := lightestVariant(master, string(body))
	if err != nil {
		return err
	}
	body, _, err = c.get(ctx, current, variant, "")
	if err != nil {
		return fmt.Errorf("media playlist: %w", err)
	}
	segments, err := parseMediaPlaylist(variant, string(body))
	if err != nil {
		return err
	}
	same := len(segments) == len(video.segments)
	for index := 0; same && index < len(segments); index++ {
		same = math.Abs(segments[index].start-video.segments[index].start) < 0.01
	}
	if !same {
		video.decoded = map[int]frames{}
	}
	video.segments = segments
	return nil
}

// lightestVariant is the URL of a master's variant of the least bandwidth,
// of H.264 when the codecs are told. A media playlist is its own.
func lightestVariant(masterURL, body string) (string, error) {
	if strings.Contains(body, "#EXTINF") {
		return masterURL, nil
	}
	base, err := url.Parse(masterURL)
	if err != nil {
		return "", err
	}
	best, bestBandwidth := "", int64(math.MaxInt64)
	var attributes map[string]string
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			attributes = parseAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
		case attributes != nil && line != "" && !strings.HasPrefix(line, "#"):
			bandwidth, _ := strconv.ParseInt(attributes["BANDWIDTH"], 10, 64)
			codecs := attributes["CODECS"]
			if (codecs == "" || strings.Contains(codecs, "avc1") || strings.Contains(codecs, "avc3")) && bandwidth < bestBandwidth {
				best, bestBandwidth = resolveReference(base, line), bandwidth
			}
			attributes = nil
		}
	}
	if best == "" {
		return "", errors.New("master has no H.264 variant")
	}
	return best, nil
}

// parseMediaPlaylist lists a media playlist's segments; encrypted ones and
// byte ranges, which a segment fetched alone can't be decoded from, are
// not supported.
func parseMediaPlaylist(playlistURL, body string) ([]hlsSegment, error) {
	base, err := url.Parse(playlistURL)
	if err != nil {
		return nil, err
	}
	var segments []hlsSegment
	position, duration, init := 0.0, 0.0, ""
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			value, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			duration, _ = strconv.ParseFloat(strings.TrimSpace(value), 64)
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			if method := parseAttributes(strings.TrimPrefix(line, "#EXT-X-KEY:"))["METHOD"]; method != "" && method != "NONE" {
				return nil, fmt.Errorf("segments are encrypted (%s)", method)
			}
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE"):
			return nil, errors.New("segments are byte ranges")
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			attributes := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MAP:"))
			if attributes["BYTERANGE"] != "" {
				return nil, errors.New("the init segment is a byte range")
			}
			init = resolveReference(base, attributes["URI"])
		case line != "" && !strings.HasPrefix(line, "#"):
			segments = append(segments, hlsSegment{url: resolveReference(base, line), start: position, duration: duration, init: init})
			position += duration
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(segments) == 0 {
		return nil, errors.New("media playlist has no segments")
	}
	return segments, nil
}

// tapBody passes a body on as it is read and hands its bytes, up to
// alignTapLimit, to done once it was read to the end.
type tapBody struct {
	io.ReadCloser
	data []byte
	over bool
	once sync.Once
	done func([]byte)
}

func (body *tapBody) Read(buffer []byte) (int, error) {
	n, err := body.ReadCloser.Read(buffer)
	if !body.over {
		if len(body.data)+n > alignTapLimit {
			body.over, body.data = true, nil
		} else {
			body.data = append(body.data, buffer[:n]...)
		}
	}
	if err == io.EOF && !body.over {
		body.once.Do(func() { body.done(body.data) })
	}
	return n, err
}
