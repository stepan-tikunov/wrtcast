package tmdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// What became of a title.
const (
	stateMatched = "matched"
	// TMDB has no such title, as Claude found.
	stateNone   = "none"
	stateFailed = "failed"
)

// How a title was matched.
const (
	byName   = "name"
	byClaude = "claude"
)

const schema = `
	CREATE TABLE IF NOT EXISTS tmdb_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS tmdb_titles (
		title TEXT PRIMARY KEY, state TEXT NOT NULL, tmdb_id INTEGER, kind TEXT, how TEXT,
		attempts INTEGER NOT NULL DEFAULT 0, retry_at INTEGER, error TEXT,
		cost REAL, reason TEXT, updated INTEGER NOT NULL
	);`

// store keeps the key and what became of each title, in title-api's state
// database.
type store struct {
	db *sql.DB
}

func openStore(ctx context.Context, db *sql.DB) (*store, error) {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("initialize TMDB state: %w", err)
	}
	return &store{db: db}, nil
}

func (s *store) setting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM tmdb_settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// setSetting keeps a setting; an empty value removes it.
func (s *store) setSetting(ctx context.Context, key, value string) error {
	if value == "" {
		_, err := s.db.ExecContext(ctx, "DELETE FROM tmdb_settings WHERE key = ?", key)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tmdb_settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// kept is what became of a title, if it was tried.
type kept struct {
	state    string
	id       uint64
	kind     string
	attempts int
	retryAt  time.Time
}

func (s *store) title(ctx context.Context, title string) (kept, bool, error) {
	var result kept
	var id sql.NullInt64
	var kind sql.NullString
	var retryAt sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT state, tmdb_id, kind, attempts, retry_at FROM tmdb_titles WHERE title = ?`, title,
	).Scan(&result.state, &id, &kind, &result.attempts, &retryAt)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	result.id, result.kind = uint64(id.Int64), kind.String
	if retryAt.Valid {
		result.retryAt = time.Unix(retryAt.Int64, 0)
	}
	return result, true, nil
}

// matched keeps the TMDB title of a title, or that there is none (id 0),
// how it was found, why, and what it cost.
func (s *store) matched(ctx context.Context, title string, id uint64, kind, how, reason string, cost float64) error {
	state := stateMatched
	var tmdbID sql.NullInt64
	var tmdbKind sql.NullString
	if id == 0 {
		state = stateNone
	} else {
		tmdbID = sql.NullInt64{Int64: int64(id), Valid: true}
		tmdbKind = sql.NullString{String: kind, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tmdb_titles (title, state, tmdb_id, kind, how, cost, reason, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (title) DO UPDATE SET state = excluded.state, tmdb_id = excluded.tmdb_id,
			kind = excluded.kind, how = excluded.how, error = NULL, retry_at = NULL,
			cost = coalesce(cost, 0) + excluded.cost, reason = excluded.reason, updated = excluded.updated`,
		title, state, tmdbID, tmdbKind, how, cost, reason, time.Now().Unix())
	return err
}

// failed keeps that a title was not matched, why, when to try it again,
// and what the try cost.
func (s *store) failed(ctx context.Context, title string, reason error, retryAt time.Time, cost float64) error {
	var retry sql.NullInt64
	if !retryAt.IsZero() {
		retry = sql.NullInt64{Int64: retryAt.Unix(), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tmdb_titles (title, state, attempts, retry_at, error, cost, updated)
		VALUES (?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT (title) DO UPDATE SET state = excluded.state, attempts = attempts + 1,
			retry_at = excluded.retry_at, error = excluded.error,
			cost = coalesce(cost, 0) + excluded.cost, updated = excluded.updated`,
		title, stateFailed, retry, reason.Error(), cost, time.Now().Unix())
	return err
}
