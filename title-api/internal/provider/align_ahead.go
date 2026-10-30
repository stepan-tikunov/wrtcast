package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"
)

// The next episode of a series is aligned while the one before it plays, so
// that its sections are on its video once it starts, also days later (the
// anchors are kept). Its streams are listed as for a playback, which loads
// a page of the alternative player and signs its links, and of the dub
// played, the lightest quality's master and media playlist and the
// fragments around where its playback would start (past the recap and the
// intro) are loaded: no socket and no view, as nothing plays. The CDN has
// blocked the home address before, so the first refusal of any of it
// stops the aligning ahead for a while.

const (
	// After a playback's first anchor the next episode is aligned this much
	// later, not among the playback's own first requests.
	aheadDelay = 2 * time.Minute
	// How long one aligning ahead may take, the listing included.
	aheadTimeout = 3 * time.Minute
	// The alternative video loaded ahead: from this much before where the
	// playback would start to this much after.
	aheadBefore = 3.0
	aheadAfter  = 9.0
	// Where a playback starts without a recap or an intro at the start.
	aheadStart = 60.0
	// After a refusal of the CDN, no aligning ahead for so long.
	aheadPause = 12 * time.Hour
)

// alignAhead aligns ref's episode of the dub of label ahead of its
// playback, unless it is aligned already; should that fail, the beginning
// of its primary video is decoded at least.
func (c *Client) alignAhead(ref sectionsReference, label string) {
	if ref.master == "" || len(ref.sections) == 0 || ffmpegPath() == "" {
		return
	}
	var known alignment
	if text := c.loadAligned(ref.key + "|" + strings.TrimSpace(label)); text != "" &&
		json.Unmarshal([]byte(text), &known) == nil && len(known.Anchors) > 0 {
		return
	}
	select {
	case <-c.sessionsCtx.Done():
		return
	case <-time.After(aheadDelay):
	}
	if err := c.alignEpisodeAhead(ref, label); err != nil {
		log.Printf("align %s|%s ahead: %v", ref.key, strings.TrimSpace(label), err)
		c.prefetchPrimary(ref)
	}
}

func (c *Client) alignEpisodeAhead(ref sectionsReference, label string) error {
	c.alignMu.Lock()
	paused := time.Now().Before(c.aheadPausedUntil)
	c.alignMu.Unlock()
	if paused {
		return errors.New("paused after a refusal of the CDN")
	}
	ctx, cancel := context.WithTimeout(c.sessionsCtx, aheadTimeout)
	defer cancel()
	streams, err := c.EpisodeStreams(ctx, ref.title, ref.season, ref.episode)
	if err != nil {
		return fmt.Errorf("list streams: %w", err)
	}
	source := c.alternativeSourceOf(streams, label)
	if source == nil {
		return errors.New("no source of the dub")
	}
	v := source.alignmentOf()
	if v == nil {
		return errors.New("the episode has no sections to align")
	}
	v.mu.Lock()
	if v.busy || len(v.known.Anchors) > 0 {
		v.mu.Unlock()
		return nil
	}
	v.busy = true
	start := v.aheadStart()
	v.mu.Unlock()

	run, init, duration, err := source.loadAhead(ctx, start-aheadBefore, start+aheadAfter)
	if err != nil {
		v.mu.Lock()
		v.busy = false
		v.mu.Unlock()
		var refused *alternativeStatusError
		if errors.As(err, &refused) {
			c.alignMu.Lock()
			c.aheadPausedUntil = time.Now().Add(aheadPause)
			c.alignMu.Unlock()
		}
		return err
	}
	if !v.match(run, init, duration, true) {
		return errors.New("no match")
	}
	return nil
}

// alternativeSourceOf is the alternative player's source of the dub of
// label among streams.
func (c *Client) alternativeSourceOf(streams []MovieStream, label string) *alternativeSource {
	prefix := c.mediaBaseURL + "/v1/media/"
	for _, stream := range streams {
		id, ok := strings.CutPrefix(stream.MasterPlaylistURL, prefix)
		if !ok {
			continue
		}
		entry, err := c.masters.entry(strings.TrimSuffix(id, "/master.m3u8"))
		if err == nil && entry.source != nil && strings.TrimSpace(entry.source.label) == strings.TrimSpace(label) {
			return entry.source
		}
	}
	return nil
}

// aheadStart is where a playback of the episode would start on the
// alternative video: past the skippable recap and intro it starts with, as
// the router starts it, moved as the prior predicts; else aheadStart.
// Under v.mu.
func (v *videoAlignment) aheadStart() float64 {
	end := 0.0
	for _, section := range v.ref.sections {
		if section.Kind == SectionCredits || !section.Skippable || section.Start > end+1 {
			break
		}
		end = section.End
	}
	if end == 0 {
		return aheadStart
	}
	if predicted, ok := v.prior.predicted(); ok {
		return predicted.at(end)
	}
	return end
}

// loadAhead loads the fragments of the source's lightest quality from low
// to high seconds, with their init segment (none for MPEG-TS), and tells
// the video's length. Nothing of it goes to the view: nothing plays.
func (source *alternativeSource) loadAhead(ctx context.Context, low, high float64) ([]tappedFragment, []byte, float64, error) {
	source.mu.Lock()
	qualities := source.qualities
	source.mu.Unlock()
	links := qualities[pickResolution(qualities, "0")]
	link := strings.TrimSpace(strings.Split(links, " or ")[0])
	if link == "" {
		return nil, nil, 0, errors.New("no quality")
	}
	body, err := source.quietGet(ctx, link)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("master: %w", err)
	}
	_, levelURL, err := firstVariant(link, body)
	if err != nil {
		return nil, nil, 0, err
	}
	body, err = source.quietGet(ctx, levelURL)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("media playlist: %w", err)
	}
	segments, err := parseMediaPlaylist(levelURL, string(body))
	if err != nil {
		return nil, nil, 0, err
	}
	last := segments[len(segments)-1]
	duration := last.start + last.duration
	low, high = math.Max(0, math.Min(low, duration-aheadBefore-aheadAfter)), math.Min(high, duration)
	var run []tappedFragment
	var init []byte
	for _, segment := range segments {
		if segment.start+segment.duration <= low || segment.start >= high {
			continue
		}
		if segment.init != "" && init == nil {
			if init, err = source.quietGet(ctx, segment.init); err != nil {
				return nil, nil, 0, fmt.Errorf("init segment: %w", err)
			}
		}
		data, err := source.quietGet(ctx, segment.url)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("fragment at %.0f s: %w", segment.start, err)
		}
		run = append(run, tappedFragment{start: segment.start, end: segment.start + segment.duration, init: segment.init, data: data})
	}
	if len(run) == 0 {
		return nil, nil, 0, fmt.Errorf("no fragment from %.0f to %.0f s", low, high)
	}
	return run, init, duration, nil
}

// quietGet loads a URL of the source's CDN as the page does, one after
// another, but tells no view of a failure.
func (source *alternativeSource) quietGet(ctx context.Context, rawURL string) ([]byte, error) {
	response, err := source.cdnRequest(ctx, rawURL, false)
	if err != nil {
		return nil, err
	}
	return readAlternativeResponse(response)
}
