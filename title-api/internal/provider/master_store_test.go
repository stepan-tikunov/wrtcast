package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMasterStoreExpiresAndEvicts(t *testing.T) {
	ctx := context.Background()
	store := newMasterStore(20*time.Millisecond, 1)
	first := store.put([]byte("first"))
	second := store.put([]byte("second"))
	if _, err := store.get(ctx, first); !errors.Is(err, ErrMasterNotFound) {
		t.Fatalf("oldest master was not evicted: %v", err)
	}
	if body, err := store.get(ctx, second); err != nil || string(body) != "second" {
		t.Fatalf("second master = %q, %v", body, err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := store.get(ctx, second); !errors.Is(err, ErrMasterNotFound) {
		t.Fatalf("master did not expire: %v", err)
	}
}

func TestMasterStoreResolvesLazilyOnce(t *testing.T) {
	ctx := context.Background()
	store := newMasterStore(time.Hour, 4)
	calls := 0
	id := store.putLazy(func(context.Context) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("rejected")
		}
		return []byte("master"), nil
	})
	if calls != 0 {
		t.Fatal("master resolved before it was requested")
	}
	if _, err := store.get(ctx, id); err == nil {
		t.Fatal("failed resolution was not reported")
	}
	for range 2 {
		if body, err := store.get(ctx, id); err != nil || string(body) != "master" {
			t.Fatalf("master = %q, %v", body, err)
		}
	}
	if calls != 2 {
		t.Fatalf("resolved %d times, want 2", calls)
	}
}
