package tmdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// Of what the provider's search finds, the titles a lookup opens at
	// most: each is a request of the provider's.
	lookupCandidates = 3
	// TMDB titles waiting to be looked for at most; the earliest asked go
	// beyond.
	lookupQueueLimit = 20
	// How long a lookup may take, Claude settling its candidates included.
	lookupTimeout = 5 * time.Minute
	// A title not found is looked for again so much later; one whose lookup
	// failed, sooner.
	notFoundAgain = 7 * 24 * time.Hour
	failedAgain   = 30 * time.Minute
)

// What a lookup of a TMDB title on the provider came to.
const (
	LookupPending  = "pending"
	LookupFound    = "found"
	LookupNotFound = "not_found"
)

// Lookups as kept: those not found and those that failed, to be looked for
// again once their time comes.
const (
	lookupFound    = "found"
	lookupNotFound = "not_found"
	lookupFailed   = "failed"
)

// ErrNoProvider is a lookup with no provider to look on.
var ErrNoProvider = errors.New("no provider to look for TMDB's titles on")

// Provider is the provider's site, as a TMDB title is looked for on it.
type Provider interface {
	// Search are the titles the provider's search finds.
	Search(ctx context.Context, query string) ([]Found, error)
	// Title is a title as its page on the provider tells.
	Title(ctx context.Context, id string) (Title, error)
}

// Found is a title the provider's search found.
type Found struct {
	ID    string
	Title string
	// Such as "2006-2013", or empty.
	Year string
}

// lookup is a TMDB title to be looked for on the provider.
type lookup struct {
	kind string
	id   uint64
}

func (l lookup) String() string { return l.kind + "/" + strconv.FormatUint(l.id, 10) }

// UseProvider has the service look for TMDB's titles on the provider.
func (s *Service) UseProvider(provider Provider) {
	s.mu.Lock()
	s.provider = provider
	s.mu.Unlock()
}

// Lookup tells whether a TMDB title is on the provider, and its title there
// if it is: one matched is found at once; else it is looked for in the
// background, if it was not or its time to be again has come, and waited
// for until ctx is done; pending, if the wait ends first.
func (s *Service) Lookup(ctx context.Context, kind string, id uint64) (string, string, error) {
	if _, err := s.keyed(ctx); err != nil {
		return "", "", err
	}
	s.mu.Lock()
	provider := s.provider
	s.mu.Unlock()
	if provider == nil {
		return "", "", ErrNoProvider
	}
	l := lookup{kind, id}
	queued := false
	for {
		titles, err := s.store.titleIDs(ctx, kind, []uint64{id})
		if err != nil {
			return "", "", err
		}
		if title := titles[id]; title != "" {
			return LookupFound, title, nil
		}
		s.mu.Lock()
		pending := s.lookingFor == l || slices.Contains(s.lookups, l)
		changed := s.changed
		s.mu.Unlock()
		if !pending {
			if queued {
				return LookupNotFound, "", nil
			}
			due, err := s.lookupDue(ctx, l)
			if err != nil {
				return "", "", err
			}
			if !due {
				return LookupNotFound, "", nil
			}
			s.queueLookup(l)
			queued = true
			continue
		}
		select {
		case <-ctx.Done():
			return LookupPending, "", nil
		case <-changed:
		}
	}
}

// lookupDue tells whether a TMDB title is to be looked for: it was not, or
// its time to be again has come.
func (s *Service) lookupDue(ctx context.Context, l lookup) (bool, error) {
	var retryAt sql.NullInt64
	err := s.store.db.QueryRowContext(ctx, `
		SELECT retry_at FROM tmdb_lookups WHERE kind = ? AND tmdb_id = ?`, l.kind, int64(l.id)).Scan(&retryAt)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return retryAt.Valid && !s.now().Before(time.Unix(retryAt.Int64, 0)), nil
}

func (s *Service) queueLookup(l lookup) {
	s.mu.Lock()
	queue := []lookup{l}
	for _, queued := range s.lookups {
		if queued != l {
			queue = append(queue, queued)
		}
	}
	s.lookups = queue[:min(len(queue), lookupQueueLimit)]
	s.mu.Unlock()
	select {
	case s.lookupWake <- struct{}{}:
	default:
	}
}

// runLookups looks for the TMDB titles queued, one at a time, until ctx is
// done.
func (s *Service) runLookups(ctx context.Context) {
	for {
		s.mu.Lock()
		var l lookup
		queued := len(s.lookups) > 0
		if queued {
			l, s.lookups = s.lookups[0], s.lookups[1:]
			s.lookingFor = l
		}
		provider := s.provider
		s.mu.Unlock()
		if !queued {
			select {
			case <-ctx.Done():
				return
			case <-s.lookupWake:
			}
			continue
		}
		s.lookUp(ctx, provider, l)
		s.mu.Lock()
		s.lookingFor = lookup{}
		s.notify()
		s.mu.Unlock()
	}
}

func (s *Service) lookUp(ctx context.Context, provider Provider, l lookup) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	started := time.Now()
	title, how, err := s.find(ctx, provider, l)
	state, retryAt := lookupFound, time.Time{}
	switch {
	case err != nil:
		log.Printf("tmdb lookup %s: %v", l, err)
		state, retryAt = lookupFailed, s.now().Add(failedAgain)
	case title == "":
		log.Printf("tmdb lookup %s: not on the provider, in %s", l, time.Since(started).Round(time.Second))
		state, retryAt = lookupNotFound, s.now().Add(notFoundAgain)
	default:
		log.Printf("tmdb lookup %s: %s by %s in %s", l, title, how, time.Since(started).Round(time.Second))
	}
	var retry sql.NullInt64
	if !retryAt.IsZero() {
		retry = sql.NullInt64{Int64: retryAt.Unix(), Valid: true}
	}
	_, err = s.store.db.ExecContext(context.WithoutCancel(ctx), `
		INSERT INTO tmdb_lookups (kind, tmdb_id, state, title, retry_at, updated) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind, tmdb_id) DO UPDATE SET state = excluded.state, title = excluded.title,
			retry_at = excluded.retry_at, updated = excluded.updated`,
		l.kind, int64(l.id), state, sql.NullString{String: title, Valid: title != ""}, retry, s.now().Unix())
	if err != nil {
		log.Printf("tmdb lookup %s: %v", l, err)
	}
}

// find is the provider's title of a TMDB title, or none: of what the
// provider's search finds by its Russian name and its original one, those
// of its year, the likeliest first, are matched with TMDB as titles opened
// are, until one is matched with it; how that one was.
func (s *Service) find(ctx context.Context, provider Provider, l lookup) (string, string, error) {
	tmdb, err := s.keyed(ctx)
	if err != nil {
		return "", "", err
	}
	details, err := s.details(ctx, tmdb, l.kind, l.id)
	if err != nil {
		return "", "", err
	}
	var found []Found
	seen := map[string]bool{}
	for _, query := range []string{details.Name, details.OriginalName} {
		query = strings.TrimSpace(query)
		if utf8.RuneCountInString(query) < 3 || seen["query "+normalize(query)] {
			continue
		}
		seen["query "+normalize(query)] = true
		results, err := provider.Search(ctx, query)
		if err != nil {
			return "", "", fmt.Errorf("search %q: %w", query, err)
		}
		for _, x := range results {
			if !seen[x.ID] {
				seen[x.ID] = true
				found = append(found, x)
			}
		}
	}
	for _, x := range likeliest(details, found) {
		title, how, err := s.matchFound(ctx, tmdb, provider, x)
		if err != nil {
			log.Printf("tmdb lookup %s: %s %q: %v", l, x.ID, x.Title, err)
			continue
		}
		if title.ID == l.id && title.Kind == l.kind {
			return x.ID, how, nil
		}
	}
	return "", "", nil
}

// likeliest are the titles found that may be a TMDB title, lookupCandidates
// at most: those of its year or one off, or of no year told; those named
// as it first, then those of its year.
func likeliest(details Details, found []Found) []Found {
	names := map[string]bool{normalize(details.Name): true, normalize(details.OriginalName): true}
	year, _ := strconv.Atoi(details.Year())
	rank := func(x Found) int {
		r := 0
		if !names[normalize(x.Title)] {
			r += 2
		}
		if y, _ := strconv.Atoi(yearOf(x.Year)); y != year {
			r++
		}
		return r
	}
	var result []Found
	for _, x := range found {
		y, err := strconv.Atoi(yearOf(x.Year))
		if err != nil || year == 0 || y >= year-1 && y <= year+1 {
			result = append(result, x)
		}
	}
	slices.SortStableFunc(result, func(a, b Found) int { return rank(a) - rank(b) })
	return result[:min(len(result), lookupCandidates)]
}

// matchFound is the TMDB title of a title the provider's search found (ID
// 0 if none), as kept if it was matched, else matched as one opened is and
// kept; how it was matched.
func (s *Service) matchFound(ctx context.Context, tmdb *client, provider Provider, x Found) (Match, string, error) {
	kept, ok, err := s.store.title(ctx, x.ID)
	if err != nil {
		return Match{}, "", err
	}
	switch {
	case ok && kept.state == stateMatched:
		return Match{ID: kept.id, Kind: kept.kind}, "its match", nil
	case ok && kept.state == stateNone:
		return Match{}, "", nil
	}
	t, err := provider.Title(ctx, x.ID)
	if err != nil {
		return Match{}, "", err
	}
	t.ID = x.ID
	found, how, reason, cost, err := s.match(ctx, tmdb, t)
	if err != nil {
		return Match{}, "", err
	}
	if err := s.store.matched(context.WithoutCancel(ctx), t.ID, found.ID, found.Kind, how, reason, cost); err != nil {
		return Match{}, "", err
	}
	return Match{ID: found.ID, Kind: found.Kind}, how, nil
}
