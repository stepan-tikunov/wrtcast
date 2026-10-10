package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

var ErrMasterNotFound = errors.New("master playlist not found")

// storedMaster is either a ready playlist or a recipe for one: alternative
// sources are only resolved when a player asks for them, so listing the
// sources of a title costs no CDN requests.
type storedMaster struct {
	mu        *sync.Mutex
	body      []byte
	resolve   func(context.Context) ([]byte, error)
	expiresAt time.Time
}

type masterStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	limit   int
	entries map[string]*storedMaster
}

func newMasterStore(ttl time.Duration, limit int) *masterStore {
	return &masterStore{ttl: ttl, limit: limit, entries: make(map[string]*storedMaster)}
}

func (store *masterStore) put(body []byte) string {
	return store.add(&storedMaster{body: append([]byte(nil), body...)})
}

func (store *masterStore) putLazy(resolve func(context.Context) ([]byte, error)) string {
	return store.add(&storedMaster{resolve: resolve})
}

func (store *masterStore) add(entry *storedMaster) string {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := time.Now()
	store.cleanup(now)
	for len(store.entries) >= store.limit {
		var oldestID string
		var oldest time.Time
		for id, entry := range store.entries {
			if oldestID == "" || entry.expiresAt.Before(oldest) {
				oldestID, oldest = id, entry.expiresAt
			}
		}
		delete(store.entries, oldestID)
	}
	random := make([]byte, 16)
	_, _ = rand.Read(random)
	id := hex.EncodeToString(random)
	entry.mu = &sync.Mutex{}
	entry.expiresAt = now.Add(store.ttl)
	store.entries[id] = entry
	return id
}

func (store *masterStore) get(ctx context.Context, id string) ([]byte, error) {
	store.mu.Lock()
	store.cleanup(time.Now())
	entry, ok := store.entries[id]
	store.mu.Unlock()
	if !ok {
		return nil, ErrMasterNotFound
	}
	// Concurrent requests for one master wait for a single resolution; a
	// failed one is retried by the next request.
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.body == nil {
		body, err := entry.resolve(ctx)
		if err != nil {
			return nil, err
		}
		entry.body, entry.resolve = body, nil
	}
	return append([]byte(nil), entry.body...), nil
}

func (store *masterStore) cleanup(now time.Time) {
	for id, entry := range store.entries {
		if !entry.expiresAt.After(now) {
			delete(store.entries, id)
		}
	}
}

func (c *Client) MasterPlaylist(ctx context.Context, id string) ([]byte, error) {
	return c.masters.get(ctx, id)
}
