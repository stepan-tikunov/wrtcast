package provider

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const (
	providerURLKey       = "provider_url"
	playerFingerprintKey = "player_fingerprint"
)

type ProviderURLStore interface {
	Load(context.Context) (string, error)
	Save(context.Context, string) error
}

// PlayerFingerprintStore remembers the device fingerprint the alternative
// player identifies us by: a new one on every start looks to its CDN like a
// new device behind the same address.
type PlayerFingerprintStore interface {
	LoadPlayerFingerprint(context.Context) (string, error)
	SavePlayerFingerprint(context.Context, string) error
}

type SQLiteProviderURLStore struct {
	db *sql.DB
}

func OpenSQLiteProviderURLStore(ctx context.Context, path string) (*SQLiteProviderURLStore, error) {
	if path == "" {
		return nil, fmt.Errorf("SQLite state path is empty")
	}
	if directory := filepath.Dir(path); directory != "." {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return nil, fmt.Errorf("create SQLite state directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite state: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &SQLiteProviderURLStore{db: db}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS state (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize SQLite state: %w", err)
	}
	return store, nil
}

func (s *SQLiteProviderURLStore) Load(ctx context.Context) (string, error) {
	value, err := s.load(ctx, providerURLKey)
	if err != nil {
		return "", fmt.Errorf("load provider URL: %w", err)
	}
	return value, nil
}

func (s *SQLiteProviderURLStore) Save(ctx context.Context, value string) error {
	if err := s.save(ctx, providerURLKey, value); err != nil {
		return fmt.Errorf("save provider URL: %w", err)
	}
	return nil
}

func (s *SQLiteProviderURLStore) LoadPlayerFingerprint(ctx context.Context) (string, error) {
	value, err := s.load(ctx, playerFingerprintKey)
	if err != nil {
		return "", fmt.Errorf("load player fingerprint: %w", err)
	}
	return value, nil
}

func (s *SQLiteProviderURLStore) SavePlayerFingerprint(ctx context.Context, value string) error {
	if err := s.save(ctx, playerFingerprintKey, value); err != nil {
		return fmt.Errorf("save player fingerprint: %w", err)
	}
	return nil
}

func (s *SQLiteProviderURLStore) load(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM state WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (s *SQLiteProviderURLStore) save(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	return err
}

func (s *SQLiteProviderURLStore) Close() error {
	return s.db.Close()
}
