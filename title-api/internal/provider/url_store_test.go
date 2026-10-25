package provider

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteProviderURLStorePersists(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", "title-api.db")
	store, err := OpenSQLiteProviderURLStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, "https://active.example"); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePlayerFingerprint(ctx, "fingerprint"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteProviderURLStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://active.example" {
		t.Fatalf("Load() = %q, want %q", got, "https://active.example")
	}
	if got, err := reopened.LoadPlayerFingerprint(ctx); err != nil || got != "fingerprint" {
		t.Fatalf("LoadPlayerFingerprint() = %q, %v", got, err)
	}
}
