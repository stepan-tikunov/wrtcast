// Package marking marks the recap, the intro and the credits of the
// episodes the primary player marks none of, with Claude (see
// internal/marker), in the background: of the titles first played since,
// the episode played and the two after it, one at a time.
package marking

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"title-api/internal/provider"
)

// What became of an episode.
const (
	stateDone        = "done"
	stateNoSubtitles = "no_subtitles"
	stateFailed      = "failed"
)

const schema = `
	CREATE TABLE IF NOT EXISTS marking_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS marking_titles (title TEXT PRIMARY KEY, since INTEGER NOT NULL);
	CREATE TABLE IF NOT EXISTS marking_episodes (
		title TEXT NOT NULL, season INTEGER NOT NULL, episode TEXT NOT NULL,
		state TEXT NOT NULL, sections TEXT, duration REAL,
		attempts INTEGER NOT NULL DEFAULT 0, retry_at INTEGER, error TEXT,
		model TEXT, cost REAL, result TEXT, updated INTEGER NOT NULL,
		PRIMARY KEY (title, season, episode)
	);`

// store keeps the settings, the titles first played since, and what became
// of each episode, in title-api's state database.
type store struct {
	db *sql.DB
}

func openStore(ctx context.Context, db *sql.DB) (*store, error) {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("initialize marking state: %w", err)
	}
	return &store{db: db}, nil
}

func (s *store) setting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM marking_settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// setSetting keeps a setting; an empty value removes it.
func (s *store) setSetting(ctx context.Context, key, value string) error {
	if value == "" {
		_, err := s.db.ExecContext(ctx, "DELETE FROM marking_settings WHERE key = ?", key)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO marking_settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *store) addTitle(ctx context.Context, title string) error {
	_, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO marking_titles (title, since) VALUES (?, ?)", title, time.Now().Unix())
	return err
}

func (s *store) hasTitle(ctx context.Context, title string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM marking_titles WHERE title = ?", title).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// episode is what became of an episode, if it was tried.
type episode struct {
	state    string
	sections []provider.Section
	duration float64
	attempts int
	retryAt  time.Time
}

// section is a section as kept.
type section struct {
	Kind      string  `json:"kind"`
	Start     float64 `json:"start"`
	End       float64 `json:"end"`
	Skippable bool    `json:"skippable"`
}

var kindNames = map[provider.SectionKind]string{
	provider.SectionRecap:   "recap",
	provider.SectionIntro:   "intro",
	provider.SectionCredits: "credits",
}

func (s *store) episode(ctx context.Context, title string, season int32, number string) (episode, bool, error) {
	var result episode
	var sections sql.NullString
	var duration sql.NullFloat64
	var retryAt sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT state, sections, duration, attempts, retry_at FROM marking_episodes
		WHERE title = ? AND season = ? AND episode = ?`, title, season, number,
	).Scan(&result.state, &sections, &duration, &result.attempts, &retryAt)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	result.duration = duration.Float64
	if retryAt.Valid {
		result.retryAt = time.Unix(retryAt.Int64, 0)
	}
	if sections.Valid {
		var kept []section
		if err := json.Unmarshal([]byte(sections.String), &kept); err != nil {
			return result, false, err
		}
		for _, x := range kept {
			for kind, name := range kindNames {
				if name == x.Kind {
					result.sections = append(result.sections, provider.Section{Kind: kind, Start: x.Start, End: x.End, Skippable: x.Skippable})
				}
			}
		}
	}
	return result, true, nil
}

// done keeps the sections of an episode marked on a video of duration
// seconds, with what the marking found (result) and cost.
func (s *store) done(ctx context.Context, e provider.UnmarkedEpisode, sections []provider.Section, duration float64, model string, cost float64, result any) error {
	kept := make([]section, 0, len(sections))
	for _, x := range sections {
		kept = append(kept, section{Kind: kindNames[x.Kind], Start: x.Start, End: x.End, Skippable: x.Skippable})
	}
	sectionsText, _ := json.Marshal(kept)
	resultText, _ := json.Marshal(result)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO marking_episodes (title, season, episode, state, sections, duration, model, cost, result, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (title, season, episode) DO UPDATE SET state = excluded.state,
			sections = excluded.sections, duration = excluded.duration, error = NULL, retry_at = NULL,
			model = excluded.model, cost = excluded.cost, result = excluded.result, updated = excluded.updated`,
		e.Title, e.Season, e.Episode, stateDone, string(sectionsText), duration, model, cost, string(resultText), time.Now().Unix())
	return err
}

// tried keeps that an episode was not marked: state, and why.
func (s *store) tried(ctx context.Context, e provider.UnmarkedEpisode, state string, reason error, retryAt time.Time) error {
	var retry sql.NullInt64
	if !retryAt.IsZero() {
		retry = sql.NullInt64{Int64: retryAt.Unix(), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO marking_episodes (title, season, episode, state, attempts, retry_at, error, updated)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?)
		ON CONFLICT (title, season, episode) DO UPDATE SET state = excluded.state,
			attempts = attempts + 1, retry_at = excluded.retry_at, error = excluded.error, updated = excluded.updated`,
		e.Title, e.Season, e.Episode, state, retry, reason.Error(), time.Now().Unix())
	return err
}
