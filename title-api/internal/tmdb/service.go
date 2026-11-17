package tmdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const (
	// Titles waiting at most; the earliest opened go beyond.
	queueLimit = 20
	// How long matching a title may take.
	matchTimeout = 5 * time.Minute
	// How long a title opened is looked for by name before its details are
	// told without it.
	quickTimeout = 2 * time.Second
	// A title that failed is tried again, opened so much later, four times
	// as late each time, maxAttempts times in all.
	retryAfter  = 30 * time.Minute
	maxAttempts = 3
)

// Settings are the settings of matching, the key told only by its end.
type Settings struct {
	KeySet  bool
	KeyHint string
	// Why the last match failed, if it did.
	Error string
}

// Match is the TMDB title of a title.
type Match struct {
	ID   uint64
	Kind string
}

// Service matches the titles opened with TMDB in the background, one at a
// time.
type Service struct {
	store *store
	cache *cache
	// TMDB's and Claude's APIs are asked directly, not through the proxy
	// the provider is reached by.
	http    *http.Client
	baseURL string
	// The Claude API key, of marking, if there is one.
	claudeKey func(context.Context) (string, error)

	mu    sync.Mutex
	queue []Title
	wake  chan struct{}
	// The title being matched, and what is closed once it is.
	current string
	changed chan struct{}
}

func New(ctx context.Context, db *sql.DB, claudeKey func(context.Context) (string, error)) (*Service, error) {
	store, err := openStore(ctx, db)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Service{
		store: store, cache: newCache(db), http: &http.Client{Transport: transport}, baseURL: apiURL,
		claudeKey: claudeKey, wake: make(chan struct{}, 1), changed: make(chan struct{}),
	}, nil
}

func (s *Service) tmdb(key string) *client {
	return &client{http: &http.Client{Transport: s.http.Transport, Timeout: time.Minute}, baseURL: s.baseURL, key: key}
}

// Settings are the settings as kept.
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	var result Settings
	key, err := s.store.setting(ctx, "key")
	if err != nil {
		return result, err
	}
	failure, err := s.store.setting(ctx, "error")
	if err != nil {
		return result, err
	}
	result.KeySet, result.Error = key != "", failure
	if len(key) >= 4 {
		result.KeyHint = key[len(key)-4:]
	}
	return result, nil
}

// Update keeps a key once TMDB takes it, or removes it when empty.
func (s *Service) Update(ctx context.Context, key string) (Settings, error) {
	key = strings.TrimSpace(key)
	if key != "" {
		if err := s.tmdb(key).check(ctx); err != nil {
			if errors.Is(err, ErrInvalidKey) {
				return Settings{}, err
			}
			return Settings{}, fmt.Errorf("check the key: %w", err)
		}
	}
	if err := s.store.setSetting(ctx, "key", key); err != nil {
		return Settings{}, err
	}
	if err := s.store.setSetting(ctx, "error", ""); err != nil {
		return Settings{}, err
	}
	if key == "" {
		s.mu.Lock()
		s.queue = nil
		s.notify()
		s.mu.Unlock()
	}
	return s.Settings(ctx)
}

// Match is the TMDB title of a title, once it is matched.
func (s *Service) Match(ctx context.Context, title string) (Match, bool) {
	kept, ok, err := s.store.title(ctx, title)
	if err != nil {
		log.Printf("tmdb %s: %v", title, err)
	}
	if !ok || kept.state != stateMatched {
		return Match{}, false
	}
	return Match{ID: kept.id, Kind: kept.kind}, true
}

// Opened tells of a title whose details were asked for, while there is a
// key and unless it was matched: TMDB's search is asked for it at once, for
// quickTimeout at most, so that its details tell it when its name and year
// settle it; else it is matched in the background (see WaitMatch).
func (s *Service) Opened(ctx context.Context, t Title) (Match, bool) {
	key, err := s.store.setting(ctx, "key")
	if err != nil || key == "" || !s.due(ctx, t.ID) {
		return Match{}, false
	}
	if found, ok := s.quick(ctx, key, t); ok {
		return found, true
	}
	s.mu.Lock()
	queue := []Title{t}
	for _, queued := range s.queue {
		if queued.ID != t.ID {
			queue = append(queue, queued)
		}
	}
	s.queue = queue[:min(len(queue), queueLimit)]
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return Match{}, false
}

// quick matches a title by name alone, if TMDB's search settles it soon.
func (s *Service) quick(ctx context.Context, key string, t Title) (Match, bool) {
	ctx, cancel := context.WithTimeout(ctx, quickTimeout)
	defer cancel()
	started := time.Now()
	candidates, err := s.tmdb(key).candidates(ctx, t)
	if err != nil {
		return Match{}, false // the background tells why
	}
	found, ok := exact(t, candidates)
	if !ok {
		return Match{}, false
	}
	if err := s.store.matched(context.WithoutCancel(ctx), t.ID, found.ID, found.Kind, byName, "", 0); err != nil {
		log.Printf("tmdb %s %q: %v", t.ID, t.Title, err)
		return Match{}, false
	}
	log.Printf("tmdb %s %q: %s/%d %q (%s) by %s at once in %s", t.ID, t.Title, found.Kind, found.ID,
		found.Title, found.Year(), byName, time.Since(started).Round(time.Millisecond))
	return Match{ID: found.ID, Kind: found.Kind}, true
}

// WaitMatch waits for a title queued or being matched to be, until ctx is
// done: its match, if it is matched, and whether it is still to be.
func (s *Service) WaitMatch(ctx context.Context, title string) (Match, bool, bool) {
	for {
		s.mu.Lock()
		pending := s.current == title || slices.ContainsFunc(s.queue, func(t Title) bool { return t.ID == title })
		changed := s.changed
		s.mu.Unlock()
		if match, ok := s.Match(ctx, title); ok || !pending {
			return match, ok, false
		}
		select {
		case <-ctx.Done():
			return Match{}, false, true
		case <-changed:
		}
	}
}

// notify wakes those waiting for a title to be matched; s.mu is held.
func (s *Service) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// due tells whether a title is to be matched: it was not tried, or failed
// and its time to be tried again has come.
func (s *Service) due(ctx context.Context, title string) bool {
	kept, ok, err := s.store.title(ctx, title)
	if err != nil {
		log.Printf("tmdb %s: %v", title, err)
		return false
	}
	return !ok || kept.state == stateFailed && kept.attempts < maxAttempts && time.Now().After(kept.retryAt)
}

// Run matches the titles queued, one at a time, until ctx is done.
func (s *Service) Run(ctx context.Context) {
	for {
		s.mu.Lock()
		var t Title
		queued := len(s.queue) > 0
		if queued {
			t, s.queue = s.queue[0], s.queue[1:]
			s.current = t.ID
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
		s.matchTitle(ctx, t)
		s.mu.Lock()
		s.current = ""
		s.notify()
		s.mu.Unlock()
	}
}

// errNoClaude is a title the search can't settle, with no Claude API key
// to settle it.
var errNoClaude = errors.New("TMDB's search can't tell which title it is, and there is no Claude API key to settle it")

func (s *Service) matchTitle(ctx context.Context, t Title) {
	key, err := s.store.setting(ctx, "key")
	if err != nil || key == "" || !s.due(ctx, t.ID) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, matchTimeout)
	defer cancel()
	started := time.Now()
	name := fmt.Sprintf("%s %q", t.ID, t.Title)

	found, how, reason, cost, err := s.match(ctx, s.tmdb(key), t)
	var apiErr *anthropic.Error
	switch {
	// Left untried, the title is tried when opened again.
	case errors.Is(err, errNoClaude):
		log.Printf("tmdb %s: %v", name, err)
		s.setError(ctx, "Поиск TMDB не нашёл тайтл однозначно, а ключа Claude нет")
		return
	// A refused key, a spent balance, a limit or an overloaded API is no
	// fault of the title: the queue is dropped and the titles are tried
	// when opened again.
	case errors.Is(err, ErrInvalidKey),
		errors.As(err, &apiErr) && (apiErr.StatusCode != http.StatusBadRequest || strings.Contains(strings.ToLower(apiErr.Error()), "credit")):
		log.Printf("tmdb %s: %v", name, err)
		if apiErr != nil {
			err = fmt.Errorf("Claude API: %d", apiErr.StatusCode)
		}
		s.setError(ctx, err.Error())
		s.mu.Lock()
		s.queue = nil
		s.mu.Unlock()
		return
	case err != nil:
		log.Printf("tmdb %s: %v", name, err)
		kept, _, _ := s.store.title(ctx, t.ID)
		retryAt := time.Time{}
		if kept.attempts+1 < maxAttempts {
			retryAt = time.Now().Add(retryAfter << (2 * kept.attempts))
		}
		if err := s.store.failed(context.WithoutCancel(ctx), t.ID, err, retryAt, cost); err != nil {
			log.Printf("tmdb %s: %v", name, err)
		}
		s.setError(ctx, err.Error())
		return
	}
	if err := s.store.matched(context.WithoutCancel(ctx), t.ID, found.ID, found.Kind, how, reason, cost); err != nil {
		log.Printf("tmdb %s: %v", name, err)
		return
	}
	s.setError(ctx, "")
	result := "none"
	if found.ID != 0 {
		result = fmt.Sprintf("%s/%d %q (%s)", found.Kind, found.ID, found.Title, found.Year())
	}
	log.Printf("tmdb %s: %s by %s in %s, $%.3f", name, result, how, time.Since(started).Round(time.Second), cost)
}

// match finds the TMDB title of a title, or none (ID 0): by name, else
// with Claude; how it was found, why, and the dollars it cost.
func (s *Service) match(ctx context.Context, tmdb *client, t Title) (Candidate, string, string, float64, error) {
	candidates, err := tmdb.candidates(ctx, t)
	if err != nil {
		return Candidate{}, "", "", 0, err
	}
	if found, ok := exact(t, candidates); ok {
		return found, byName, "", 0, nil
	}
	key, err := s.claudeKey(ctx)
	if err != nil {
		return Candidate{}, "", "", 0, err
	}
	if key == "" {
		return Candidate{}, "", "", 0, errNoClaude
	}
	claude := anthropic.NewClient(option.WithAPIKey(key), option.WithHTTPClient(s.http))
	result, cost, err := ask(ctx, claude, tmdb, t, candidates[:min(len(candidates), 15)])
	if err != nil {
		return Candidate{}, "", "", cost, err
	}
	if result.Kind != KindMovie && result.Kind != KindTV || result.ID == 0 {
		return Candidate{}, byClaude, result.Reason, cost, nil
	}
	// The title Claude named is checked to be on TMDB.
	found, err := tmdb.title(ctx, result.Kind, result.ID)
	if err != nil {
		return Candidate{}, "", "", cost, fmt.Errorf("Claude's answer %s/%d: %w", result.Kind, result.ID, err)
	}
	return found, byClaude, result.Reason, cost, nil
}

func (s *Service) setError(ctx context.Context, reason string) {
	if len(reason) > 300 {
		reason = strings.ToValidUTF8(reason[:300], "") + "…"
	}
	if err := s.store.setSetting(context.WithoutCancel(ctx), "error", reason); err != nil {
		log.Printf("tmdb: %v", err)
	}
}
