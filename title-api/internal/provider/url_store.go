package provider

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const providerURLKey = "provider_url"

type ProviderURLStore interface {
	Load(context.Context) (string, error)
	Save(context.Context, string) error
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
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM state WHERE key = ?", providerURLKey).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load provider URL: %w", err)
	}
	return value, nil
}

func (s *SQLiteProviderURLStore) Save(ctx context.Context, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, providerURLKey, value)
	if err != nil {
		return fmt.Errorf("save provider URL: %w", err)
	}
	return nil
}

func (s *SQLiteProviderURLStore) Close() error {
	return s.db.Close()
}
