package provider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The primary player's videos don't pass the relay: the router loads them
// from the CDN itself. Their audio is a rendition of its own, one per dub,
// so title-api loads the audio of the dub the TV plays, Ahead seconds ahead
// of it, as the router tells (FollowAudio), and gives each segment to the
// Sink: some 100 KB for 6 s, and the music is told before it plays. As
// with the relay's loading ahead: one at a time, a second apart; a refusal
// (403, 429, 451) or three failures in a row stop it for segmentsPause.

// How long a primary video's audio is followed after the router last told
// where the TV plays it.
const audioFollowIdle = 3 * time.Minute

// errNoAudio: the master has no such audio rendition, or its playlist
// can't be followed; no failure of the CDN's.
var errNoAudio = errors.New("no audio to follow")

// AudioPlay is what the router tells of a primary player's video the TV
// plays: the video (its Source the stream's name and the dub's), the
// master, the audio rendition the TV plays (its NAME), and where it plays
// and whether.
type AudioPlay struct {
	Video    Video
	Master   string
	Audio    string
	Position float64
	Playing  bool
}

type audioRun struct {
	play     AudioPlay
	told     time.Time // when play was told
	wake     chan struct{}
	segments []hlsSegment // of the audio rendition, once loaded
	failed   error        // its listing failed for good
	loaded   map[int]bool
}

func audioKey(v Video) string {
	return fmt.Sprintf("%s|%d|%s|%s", v.Title, v.Season, v.Episode, v.Source)
}

// FollowAudio tells where the TV plays a primary player's video: its audio
// is loaded ahead of there (see above).
func (c *Client) FollowAudio(play AudioPlay) error {
	cache := c.segments
	if cache == nil || cache.options.Ahead <= 0 || cache.options.Sink == nil {
		return errors.New("audio is not loaded ahead")
	}
	target, err := url.Parse(play.Master)
	if err != nil || target.Scheme != "https" && target.Scheme != "http" || target.Host == "" {
		return errors.New("invalid master")
	}
	key := audioKey(play.Video)
	cache.mu.Lock()
	if cache.audio == nil {
		cache.audio = map[string]*audioRun{}
	}
	run := cache.audio[key]
	fresh := run == nil
	if fresh {
		run = &audioRun{wake: make(chan struct{}, 1), loaded: map[int]bool{}}
		cache.audio[key] = run
	}
	if run.play.Master != play.Master || run.play.Audio != play.Audio {
		run.segments, run.failed = nil, nil
	}
	run.play, run.told = play, time.Now()
	cache.mu.Unlock()
	if fresh {
		go cache.followAudio(key, run)
	}
	select {
	case run.wake <- struct{}{}:
	default:
	}
	return nil
}

// followAudio loads the audio of a primary video ahead of where the TV
// plays it, until it is not told of for audioFollowIdle.
func (cache *segmentCache) followAudio(key string, run *audioRun) {
	c := cache.client
	ctx := c.sessionsCtx
	defer func() {
		cache.mu.Lock()
		delete(cache.audio, key)
		cache.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-run.wake:
		case <-time.After(5 * time.Second):
		}
		for {
			cache.mu.Lock()
			play, told := run.play, run.told
			idle, paused := time.Since(told) > audioFollowIdle, time.Now().Before(cache.paused)
			segments, failed := run.segments, run.failed
			cache.mu.Unlock()
			if idle {
				return
			}
			if paused || failed != nil {
				break
			}
			if segments == nil {
				segments, failed = cache.listAudio(ctx, play)
				cache.mu.Lock()
				// A failure of the CDN's is tried again on the next wake.
				if run.play.Master == play.Master && run.play.Audio == play.Audio && (failed == nil || errors.Is(failed, errNoAudio)) {
					run.segments, run.failed = segments, failed
				}
				cache.mu.Unlock()
				if failed != nil {
					log.Printf("segments: the audio of %s: %v", key, failed)
					if !errors.Is(failed, errNoAudio) {
						cache.failed(failed)
					}
					break
				}
			}
			at := play.Position
			if play.Playing {
				at += time.Since(told).Seconds()
			}
			index := -1
			for i, s := range segments {
				if s.start+s.duration <= at || s.start >= at+cache.options.Ahead {
					continue
				}
				cache.mu.Lock()
				loaded := run.loaded[i]
				cache.mu.Unlock()
				if loaded || cache.options.Wanted != nil && !cache.options.Wanted(play.Video, s.start) {
					continue
				}
				index = i
				break
			}
			if index < 0 {
				break
			}
			s := segments[index]
			init, data, err := cache.loadAudio(ctx, s)
			cache.failed(err)
			if err != nil {
				log.Printf("segments: the audio of %s at %.0f s: %v", key, s.start, err)
				// Listed anew next time: the links may be stale.
				cache.mu.Lock()
				run.segments = nil
				cache.mu.Unlock()
				break
			}
			cache.mu.Lock()
			run.loaded[index] = true
			cache.mu.Unlock()
			cache.options.Sink(Segment{Video: play.Video, Start: s.start, End: s.start + s.duration, Init: init, Data: data})
			select {
			case <-ctx.Done():
				return
			case <-time.After(aheadGap):
			}
		}
	}
}

// failed counts a failure of loading ahead (nil: a success), stopping it
// for segmentsPause on a refusal or segmentsFailures in a row.
func (cache *segmentCache) failed(err error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if err == nil {
		cache.failures = 0
		return
	}
	cache.failures++
	code := 0
	var refused *alternativeStatusError
	var status *statusError
	switch {
	case errors.As(err, &refused):
		code = refused.code
	case errors.As(err, &status):
		code = status.code
	}
	if code == http.StatusForbidden || code == http.StatusTooManyRequests ||
		code == http.StatusUnavailableForLegalReasons || cache.failures >= segmentsFailures {
		cache.paused = time.Now().Add(segmentsPause)
		log.Printf("segments: no loading ahead for %s: %v", segmentsPause, err)
	}
}

// listAudio lists the segments of the audio rendition named play.Audio of
// play's master.
func (cache *segmentCache) listAudio(ctx context.Context, play AudioPlay) ([]hlsSegment, error) {
	c := cache.client
	current, err := c.currentSession()
	if err != nil {
		return nil, err
	}
	body, _, err := c.get(ctx, current, play.Master, "")
	if err != nil {
		return nil, fmt.Errorf("master: %w", err)
	}
	rendition, err := audioRendition(play.Master, string(body), play.Audio)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoAudio, err)
	}
	body, _, err = c.get(ctx, current, rendition, "")
	if err != nil {
		return nil, fmt.Errorf("audio playlist: %w", err)
	}
	segments, err := parseMediaPlaylist(rendition, string(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoAudio, err)
	}
	return segments, nil
}

// audioRendition is the URL of a master's audio rendition named name.
func audioRendition(masterURL, body, name string) (string, error) {
	base, err := url.Parse(masterURL)
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			continue
		}
		attributes := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
		if attributes["TYPE"] == "AUDIO" && attributes["NAME"] == name && attributes["URI"] != "" {
			return resolveReference(base, attributes["URI"]), nil
		}
	}
	return "", fmt.Errorf("no audio rendition %q", name)
}

// loadAudio loads a segment of an audio rendition, and its init segment if
// any.
func (cache *segmentCache) loadAudio(ctx context.Context, s hlsSegment) ([]byte, []byte, error) {
	c := cache.client
	current, err := c.currentSession()
	if err != nil {
		return nil, nil, err
	}
	var init []byte
	if s.init != "" {
		cache.initsMu.Lock()
		init = cache.audioInits[s.init]
		cache.initsMu.Unlock()
		if init == nil {
			if init, _, err = c.get(ctx, current, s.init, ""); err != nil {
				return nil, nil, fmt.Errorf("init segment: %w", err)
			}
			cache.initsMu.Lock()
			if cache.audioInits == nil || len(cache.audioInits) > 64 {
				cache.audioInits = map[string][]byte{}
			}
			cache.audioInits[s.init] = init
			cache.initsMu.Unlock()
		}
	}
	data, _, err := c.get(ctx, current, s.url, "")
	return init, data, err
}
