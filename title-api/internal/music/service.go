// Package music tells what music plays in a film or an episode: the router
// sends the audio of each segment of a video it gets (the TV's, and those
// it loads ahead of the TV), the music service (music/ in the repository)
// scores each half second of it for music, speech and song by YAMNet, and
// of each stretch of music Shazam is asked about eight seconds, once, or
// twice if long and the first finds nothing; a stretch heard within the
// length of a track told before it, soon after, is that track's, not
// asked about again: a song cut by a dialogue. Shazam is asked one ask at
// a time, the latest video's first, as often as the music service lets
// it. What was told is kept for good: a video watched again shows its
// music at once.
package music

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	// Audio of a piece whose music is all below this is not kept: no ask
	// will want it.
	keepMusic = 0.3
	// Audio kept at most: for so long, and so much, the oldest gone first.
	audioAge   = 24 * time.Hour
	audioLimit = 1 << 30
	// Videos asked about: of audio received so recently.
	activeFor = 6 * time.Hour
	// How long Shazam isn't asked after the music service failed.
	failPause = time.Minute
	// An ask has at least this much audio, else it is kept as one that
	// found nothing.
	askMinimum = 6.0
)

// Video is a title's episode (0 and 0 for a movie) as one source (a
// player) serves it: the times of its audio are of that source's video.
type Video struct {
	Title           string
	Season, Episode int32
	Source          string
}

func (v Video) key() string {
	return fmt.Sprintf("%s|%d|%d|%s", v.Title, v.Season, v.Episode, v.Source)
}

// Service takes the audio of the videos played, and asks Shazam about
// their music in the background.
type Service struct {
	store  *store
	client *client
	dir    string
	wake   chan struct{}
}

// New is a Service asking the music service at url, keeping the audio of
// the videos in dir until it is asked about.
func New(ctx context.Context, db *sql.DB, url, dir string) (*Service, error) {
	store, err := openStore(ctx, db)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Service{store: store, client: newClient(url), dir: dir, wake: make(chan struct{}, 1)}, nil
}

// Audio takes audio of a video, from start to stop seconds of it (a
// segment's): YAMNet's look at it is kept, and the audio too if it has
// music. Audio taken before is not looked at again.
func (s *Service) Audio(ctx context.Context, v Video, start, stop float64, audio []byte) error {
	video := v.key()
	if done, err := s.store.hasAudio(ctx, video, start); err != nil || done {
		return err
	}
	frames, err := s.client.classify(ctx, audio)
	if err != nil {
		return err
	}
	loudest := 0.0
	for i := range frames {
		frames[i].T += start
		loudest = math.Max(loudest, frames[i].Music)
	}
	file := ""
	if loudest >= keepMusic {
		file = filepath.Join(s.dir, digest(video), strconv.FormatInt(int64(math.Round(start*1000)), 10)+".aac")
		if err := writeFile(file, audio); err != nil {
			return err
		}
	}
	if err := s.store.putAudio(ctx, video, start, stop, frames, file); err != nil {
		return err
	}
	if err := s.store.touch(ctx, v); err != nil {
		return err
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func digest(video string) string {
	sum := sha1.Sum([]byte(video))
	return hex.EncodeToString(sum[:8])
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + ".part"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// Track is a track heard in a video from Start to Stop seconds of it,
// Offset seconds into the track at Start.
type Track struct {
	Start    float64 `json:"start"`
	Stop     float64 `json:"stop"`
	Offset   float64 `json:"offset"`
	Key      string  `json:"key"`
	Title    string  `json:"title"`
	Artist   string  `json:"artist"`
	Cover    string  `json:"cover,omitempty"`
	URL      string  `json:"url,omitempty"`
	AppleURL string  `json:"apple_url,omitempty"`
	Spotify  string  `json:"spotify,omitempty"`
	Duration float64 `json:"duration,omitempty"`
}

// Result is the music of a video told by now: the tracks in its order,
// how many asks of Shazam are still to be made of what was received, and
// the seconds of its audio received.
type Result struct {
	Tracks   []Track `json:"tracks"`
	Pending  int     `json:"pending"`
	Received float64 `json:"received"`
}

// Tracks is the music of a video told by now.
func (s *Service) Tracks(ctx context.Context, v Video) (Result, error) {
	video := v.key()
	frames, received, err := s.store.frames(ctx, video)
	if err != nil {
		return Result{}, err
	}
	asks, err := s.store.asks(ctx, video)
	if err != nil {
		return Result{}, err
	}
	occurrences, wanted := plan(frames, asks)
	result := Result{Tracks: []Track{}, Pending: len(wanted), Received: math.Round(received*10) / 10}
	for _, o := range occurrences {
		m, err := s.store.track(ctx, o.Key)
		if err != nil {
			return Result{}, fmt.Errorf("track %s: %w", o.Key, err)
		}
		result.Tracks = append(result.Tracks, Track{
			Start: round(o.Start), Stop: round(o.Stop), Offset: round(o.Start - o.TrackStart),
			Key: m.Key, Title: m.Title, Artist: m.Artist, Cover: m.Cover, URL: m.URL,
			AppleURL: m.AppleURL, Spotify: m.Spotify, Duration: m.Duration,
		})
	}
	return result, nil
}

func round(x float64) float64 { return math.Round(x*10) / 10 }

// Run asks Shazam about the music of the videos played, one ask at a
// time, and trims the audio kept every hour, until ctx is done.
func (s *Service) Run(ctx context.Context) {
	trim := time.NewTicker(time.Hour)
	defer trim.Stop()
	s.trim(ctx)
	for {
		if ctx.Err() != nil {
			return
		}
		pause := s.next(ctx)
		if pause == 0 {
			continue
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-s.wake:
			timer.Stop()
		case <-trim.C:
			timer.Stop()
			s.trim(ctx)
		}
	}
}

// next makes the next ask, of the latest video that wants one: how long to
// wait before the next, 0 for none.
func (s *Service) next(ctx context.Context) time.Duration {
	videos, err := s.store.recent(ctx, time.Now().Add(-activeFor))
	if err != nil {
		log.Printf("music: %v", err)
		return failPause
	}
	for _, v := range videos {
		video := v.key()
		frames, _, err := s.store.frames(ctx, video)
		if err != nil {
			log.Printf("music %s: %v", video, err)
			continue
		}
		asks, err := s.store.asks(ctx, video)
		if err != nil {
			log.Printf("music %s: %v", video, err)
			continue
		}
		if _, wanted := plan(frames, asks); len(wanted) > 0 {
			return s.ask(ctx, video, wanted[0])
		}
	}
	// Nothing to ask: until audio comes.
	return time.Hour
}

// ask asks Shazam about askLength seconds of a video's audio from at (the
// pieces kept around it, whole): how long to wait before the next ask.
func (s *Service) ask(ctx context.Context, video string, at float64) time.Duration {
	pieces, err := s.store.pieces(ctx, video, at, at+askLength)
	if err != nil {
		log.Printf("music %s: %v", video, err)
		return failPause
	}
	var audio []byte
	var start, stop float64
	for _, p := range pieces {
		if len(audio) > 0 && math.Abs(p.Start-stop) > 0.1 {
			if stop > at+askMinimum {
				break
			}
			audio = nil
		}
		data, err := os.ReadFile(p.File)
		if err != nil {
			log.Printf("music %s: %v", video, err)
			audio = nil
			continue
		}
		if len(audio) == 0 {
			start = p.Start
		}
		audio = append(audio, data...)
		stop = p.Stop
	}
	a := ask{Start: at, Stop: at + askLength}
	var m *match
	if len(audio) > 0 && stop-math.Max(start, at) >= askMinimum {
		a.Start, a.Stop = start, stop
		m, err = s.client.recognize(ctx, audio)
		var limited *limitedError
		switch {
		case errors.As(err, &limited):
			log.Printf("music: %v", err)
			return max(limited.RetryAfter, 5*time.Second)
		case err != nil:
			log.Printf("music %s: %v", video, err)
			return failPause
		}
		if m != nil {
			a.Key, a.Offset, a.Duration = m.Key, m.Offset, m.Duration
		}
	}
	if err := s.store.putAsk(ctx, video, a, m); err != nil {
		log.Printf("music %s: %v", video, err)
		return failPause
	}
	return 0
}

// trim forgets the audio kept past audioAge or beyond audioLimit, the
// oldest first.
func (s *Service) trim(ctx context.Context) {
	files, at, err := s.store.files(ctx)
	if err != nil {
		log.Printf("music: %v", err)
		return
	}
	cutoff := time.Now().Add(-audioAge).Unix()
	var total int64
	for i := len(files) - 1; i >= 0; i-- {
		info, err := os.Stat(files[i])
		if err == nil {
			total += info.Size()
		}
		if err == nil && at[i] >= cutoff && total <= audioLimit {
			continue
		}
		_ = os.Remove(files[i])
		if err := s.store.forget(ctx, files[i]); err != nil {
			log.Printf("music: %v", err)
			return
		}
	}
}
