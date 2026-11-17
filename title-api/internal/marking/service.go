package marking

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"title-api/internal/marker"
	"title-api/internal/provider"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// The models marking asks: Claude Haiku throughout, and Claude Sonnet the
// beginning of the episode, where Haiku tells the recap from the episode
// poorly.
var (
	model  = marker.Model{ID: "claude-haiku-5-5", Effort: anthropic.OutputConfigEffortHigh}
	stages = map[marker.Stage]marker.Model{
		marker.StageHead: {ID: "claude-sonnet-5-5", Effort: anthropic.OutputConfigEffortMedium},
	}
)

const (
	// Episodes waiting at most; the earliest told of go beyond.
	queueLimit = 9
	// How long marking an episode may take.
	markTimeout = 30 * time.Minute
	// The pause between two fetches of an episode's segments.
	fetchPause = 500 * time.Millisecond
	// An episode that failed is tried again so much later, four times as
	// late each time, maxAttempts times in all.
	retryAfter  = 30 * time.Minute
	maxAttempts = 3
)

// The subtitles marked by, in order; then any other.
var languages = []string{"en", "ru"}

// Settings are the settings of marking, the key told only by its end.
type Settings struct {
	Enabled bool
	KeySet  bool
	KeyHint string
	// Why the last marking failed, if it did.
	Error string
}

// ErrInvalidKey is a key Claude's API refused.
var ErrInvalidKey = errors.New("the Claude API refused the key")

// Source is where the episodes' videos and subtitles come from: the
// provider's client.
type Source interface {
	Fetch(ctx context.Context, rawURL string) ([]byte, error)
	VideoSegments(ctx context.Context, master string) ([]provider.VideoSegment, error)
}

// Service marks episodes in the background, one at a time.
type Service struct {
	store *store
	// Claude's API is asked directly, not through the proxy the provider
	// is reached by.
	http *http.Client

	mu    sync.Mutex
	queue []provider.UnmarkedEpisode
	wake  chan struct{}
}

func New(ctx context.Context, db *sql.DB) (*Service, error) {
	store, err := openStore(ctx, db)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Service{store: store, http: &http.Client{Transport: transport}, wake: make(chan struct{}, 1)}, nil
}

func (s *Service) claude(key string) anthropic.Client {
	return anthropic.NewClient(option.WithAPIKey(key), option.WithHTTPClient(s.http))
}

// Settings are the settings as kept.
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	var result Settings
	enabled, err := s.store.setting(ctx, "enabled")
	if err != nil {
		return result, err
	}
	key, err := s.store.setting(ctx, "key")
	if err != nil {
		return result, err
	}
	failure, err := s.store.setting(ctx, "error")
	if err != nil {
		return result, err
	}
	result.Enabled, result.KeySet, result.Error = enabled == "1", key != "", failure
	if len(key) >= 4 {
		result.KeyHint = key[len(key)-4:]
	}
	return result, nil
}

// Key is the Claude API key, if there is one, which matching with TMDB
// asks too.
func (s *Service) Key(ctx context.Context) (string, error) {
	return s.store.setting(ctx, "key")
}

// Update turns marking on or off, and keeps a key once Claude's API takes
// it, or removes it when empty; nil leaves either as it is.
func (s *Service) Update(ctx context.Context, enabled *bool, key *string) (Settings, error) {
	if key != nil {
		value := strings.TrimSpace(*key)
		if value != "" {
			client := s.claude(value)
			if _, err := client.Models.List(ctx, anthropic.ModelListParams{Limit: anthropic.Int(1)}); err != nil {
				var apiErr *anthropic.Error
				if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
					return Settings{}, ErrInvalidKey
				}
				return Settings{}, fmt.Errorf("check the key: %w", err)
			}
		}
		if err := s.store.setSetting(ctx, "key", value); err != nil {
			return Settings{}, err
		}
		if err := s.store.setSetting(ctx, "error", ""); err != nil {
			return Settings{}, err
		}
	}
	if enabled != nil {
		value := ""
		if *enabled {
			value = "1"
		}
		if err := s.store.setSetting(ctx, "enabled", value); err != nil {
			return Settings{}, err
		}
	}
	settings, err := s.Settings(ctx)
	if err == nil && (!settings.Enabled || !settings.KeySet) {
		s.mu.Lock()
		s.queue = nil
		s.mu.Unlock()
	}
	return settings, err
}

// Opened tells of a title played for the first time: its episodes are
// marked from then on.
func (s *Service) Opened(ctx context.Context, title string) {
	if err := s.store.addTitle(ctx, title); err != nil {
		log.Printf("marking %s: %v", title, err)
	}
}

// Sections are the sections marked of an episode and the duration of the
// video they are marked on, if it is marked.
func (s *Service) Sections(title string, season int32, number string) ([]provider.Section, float64) {
	e, ok, err := s.store.episode(context.Background(), title, season, number)
	if err != nil {
		log.Printf("marking %s: %v", label(title, season, number), err)
	}
	if !ok || e.state != stateDone {
		return nil, 0
	}
	return e.sections, e.duration
}

// Unmarked puts the episodes, of a title first played since, that are
// still to be marked first in the queue, while marking is on.
func (s *Service) Unmarked(episodes []provider.UnmarkedEpisode) {
	if len(episodes) == 0 {
		return
	}
	ctx := context.Background()
	settings, err := s.Settings(ctx)
	if err != nil || !settings.Enabled || !settings.KeySet {
		return
	}
	if ok, err := s.store.hasTitle(ctx, episodes[0].Title); err != nil || !ok {
		return
	}
	var wanted []provider.UnmarkedEpisode
	for _, e := range episodes {
		if s.due(ctx, e) {
			wanted = append(wanted, e)
		}
	}
	if len(wanted) == 0 {
		return
	}
	s.mu.Lock()
	queue := wanted
	for _, queued := range s.queue {
		if !contains(wanted, queued) {
			queue = append(queue, queued)
		}
	}
	s.queue = queue[:min(len(queue), queueLimit)]
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// due tells whether an episode is to be marked: it was not, or failed and
// its time to be tried again has come.
func (s *Service) due(ctx context.Context, e provider.UnmarkedEpisode) bool {
	kept, ok, err := s.store.episode(ctx, e.Title, e.Season, e.Episode)
	if err != nil {
		log.Printf("marking %s: %v", label(e.Title, e.Season, e.Episode), err)
		return false
	}
	return !ok || kept.state == stateFailed && kept.attempts < maxAttempts && time.Now().After(kept.retryAt)
}

func contains(list []provider.UnmarkedEpisode, e provider.UnmarkedEpisode) bool {
	for _, x := range list {
		if x.Title == e.Title && x.Season == e.Season && x.Episode == e.Episode {
			return true
		}
	}
	return false
}

// Run marks the episodes queued, one at a time, until ctx is done.
func (s *Service) Run(ctx context.Context, source Source) {
	for {
		s.mu.Lock()
		var e provider.UnmarkedEpisode
		queued := len(s.queue) > 0
		if queued {
			e, s.queue = s.queue[0], s.queue[1:]
		}
		s.mu.Unlock()
		if !queued {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
			continue
		}
		s.markEpisode(ctx, source, e)
	}
}

func (s *Service) markEpisode(ctx context.Context, source Source, e provider.UnmarkedEpisode) {
	name := label(e.Title, e.Season, e.Episode)
	settings, err := s.Settings(ctx)
	if err != nil || !settings.Enabled || !settings.KeySet || !s.due(ctx, e) {
		return
	}
	key, err := s.store.setting(ctx, "key")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, markTimeout)
	defer cancel()
	started := time.Now()
	log.Printf("marking %s", name)

	sections, duration, m, result, err := s.mark(ctx, source, s.claude(key), e)
	var apiErr *anthropic.Error
	switch {
	case errors.Is(err, errNoSubtitles):
		log.Printf("marking %s: no subtitles to mark by", name)
		s.keepTried(ctx, e, stateNoSubtitles, err, time.Time{})
		return
	// A refused key, a spent balance, a limit or an overloaded API is no
	// fault of the episode: the queue is dropped and the episodes are
	// tried with the next playback.
	case errors.As(err, &apiErr) && (apiErr.StatusCode != http.StatusBadRequest || strings.Contains(strings.ToLower(apiErr.Error()), "credit")):
		log.Printf("marking %s: %v", name, err)
		s.setError(ctx, fmt.Sprintf("Claude API: %d", apiErr.StatusCode))
		s.mu.Lock()
		s.queue = nil
		s.mu.Unlock()
		return
	case err != nil:
		log.Printf("marking %s: %v", name, err)
		kept, _, _ := s.store.episode(ctx, e.Title, e.Season, e.Episode)
		retryAt := time.Time{}
		if kept.attempts+1 < maxAttempts {
			retryAt = time.Now().Add(retryAfter << (2 * kept.attempts))
		}
		s.keepTried(ctx, e, stateFailed, err, retryAt)
		s.setError(ctx, err.Error())
		return
	}
	cost := 0.0
	for id, usage := range m.Usage() {
		cost += usage.Cost(id)
	}
	if err := s.store.done(ctx, e, sections, duration, model.ID, cost, result); err != nil {
		log.Printf("marking %s: %v", name, err)
		return
	}
	s.setError(ctx, "")
	var parts []string
	for _, x := range sections {
		parts = append(parts, fmt.Sprintf("%s %.2f–%.2f", kindNames[x.Kind], x.Start, x.End))
	}
	log.Printf("marking %s: %s in %s, $%.3f", name, strings.Join(parts, ", "), time.Since(started).Round(time.Second), cost)
}

var errNoSubtitles = errors.New("no full subtitles")

// mark marks an episode: its sections, the duration of its video, the
// marker that asked Claude and what it found.
func (s *Service) mark(ctx context.Context, source Source, client anthropic.Client, e provider.UnmarkedEpisode) ([]provider.Section, float64, *marker.Marker, marker.Result, error) {
	m := &marker.Marker{Client: client, Model: model, Stages: stages}
	segments, err := source.VideoSegments(ctx, e.Master)
	if err != nil {
		return nil, 0, m, marker.Result{}, err
	}
	duration := 0.0
	for _, segment := range segments {
		duration = max(duration, segment.Start+segment.Duration)
	}
	cues, err := subtitles(ctx, source, e.Subtitles, duration)
	if err != nil {
		return nil, 0, m, marker.Result{}, err
	}
	dir, err := os.MkdirTemp("", "marking-")
	if err != nil {
		return nil, 0, m, marker.Result{}, err
	}
	defer os.RemoveAll(dir)
	result, err := m.Mark(ctx, video(ctx, source, segments, dir), cues, duration)
	if err != nil {
		return nil, 0, m, result, err
	}
	for _, stage := range []string{"head", "tail"} {
		if reason, failed := result.Errors[stage]; failed {
			return nil, 0, m, result, fmt.Errorf("%s: %s", stage, reason)
		}
	}
	return sectionsOf(result, duration), duration, m, result, nil
}

// subtitles are the cues of the first full track, of the languages
// preferred first, forced ones left out.
func subtitles(ctx context.Context, source Source, tracks []provider.SubtitleTrack, duration float64) ([]marker.Cue, error) {
	rank := func(language string) int {
		for index, preferred := range languages {
			if strings.EqualFold(language, preferred) {
				return index
			}
		}
		return len(languages)
	}
	tracks = append([]provider.SubtitleTrack(nil), tracks...)
	sort.SliceStable(tracks, func(i, j int) bool { return rank(tracks[i].Language) < rank(tracks[j].Language) })
	for _, track := range tracks {
		if track.Forced || track.PlaylistURL == "" {
			continue
		}
		data, err := source.Fetch(ctx, track.PlaylistURL)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		cues, err := marker.ParseVTT(data)
		if err == nil && marker.Full(cues, duration) {
			return cues, nil
		}
	}
	return nil, errNoSubtitles
}

// video is the video of segments, each segment fetched once, one at a time
// and fetchPause apart, and kept in dir.
func video(ctx context.Context, source Source, segments []provider.VideoSegment, dir string) marker.Video {
	var mu sync.Mutex
	var last time.Time
	load := func(rawURL, file string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		path := filepath.Join(dir, file)
		if data, err := os.ReadFile(path); err == nil {
			return data, nil
		}
		if wait := fetchPause - time.Since(last); wait > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		data, err := source.Fetch(ctx, rawURL)
		last = time.Now()
		if err != nil {
			return nil, err
		}
		return data, os.WriteFile(path, data, 0o600)
	}
	result := marker.Video{}
	if len(segments) > 0 && segments[0].Init != "" {
		init := segments[0].Init
		result.Init = func() ([]byte, error) { return load(init, "init") }
	}
	for index, segment := range segments {
		file := fmt.Sprintf("%05d", index)
		result.Segments = append(result.Segments, marker.Segment{
			Start: segment.Start, Duration: segment.Duration,
			Load: func() ([]byte, error) { return load(segment.URL, file) },
		})
	}
	return result
}

// sectionsOf are the sections of what Mark found on a video of duration
// seconds.
func sectionsOf(result marker.Result, duration float64) []provider.Section {
	times := result.Times
	var list []provider.Section
	if end, ok := times["recap_end"]; ok && end > 0 {
		list = append(list, provider.Section{Kind: provider.SectionRecap, Start: 0, End: end, Skippable: true})
	}
	start, hasStart := times["intro_start"]
	end, hasEnd := times["intro_end"]
	if hasStart && hasEnd && end > start {
		list = append(list, provider.Section{Kind: provider.SectionIntro, Start: start, End: end, Skippable: true})
	}
	start, hasStart = times["credits_start"]
	end, hasEnd = times["credits_end"]
	if hasStart && hasEnd {
		end = min(end, duration)
		if end > start {
			list = append(list, provider.Section{Kind: provider.SectionCredits, Start: start, End: end, Skippable: result.Skippable})
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Start < list[j].Start })
	return list
}

func (s *Service) keepTried(ctx context.Context, e provider.UnmarkedEpisode, state string, reason error, retryAt time.Time) {
	if err := s.store.tried(context.WithoutCancel(ctx), e, state, reason, retryAt); err != nil {
		log.Printf("marking %s: %v", label(e.Title, e.Season, e.Episode), err)
	}
}

func (s *Service) setError(ctx context.Context, reason string) {
	if len(reason) > 300 {
		reason = strings.ToValidUTF8(reason[:300], "") + "…"
	}
	if err := s.store.setSetting(context.WithoutCancel(ctx), "error", reason); err != nil {
		log.Printf("marking: %v", err)
	}
}

func label(title string, season int32, episode string) string {
	return fmt.Sprintf("%s s%de%s", title, season, episode)
}
