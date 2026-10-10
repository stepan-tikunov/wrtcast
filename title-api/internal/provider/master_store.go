package provider

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

type storedMaster struct {
	body      []byte
	expiresAt time.Time
}

type masterStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	limit   int
	entries map[string]storedMaster
}

func newMasterStore(ttl time.Duration, limit int) *masterStore {
	return &masterStore{ttl: ttl, limit: limit, entries: make(map[string]storedMaster)}
}

func (store *masterStore) put(body []byte) string {
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
	store.entries[id] = storedMaster{body: append([]byte(nil), body...), expiresAt: now.Add(store.ttl)}
	return id
}

func (store *masterStore) get(id string) ([]byte, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := time.Now()
	store.cleanup(now)
	entry, ok := store.entries[id]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), entry.body...), true
}

func (store *masterStore) cleanup(now time.Time) {
	for id, entry := range store.entries {
		if !entry.expiresAt.After(now) {
			delete(store.entries, id)
		}
	}
}

func (c *Client) MasterPlaylist(id string) ([]byte, bool) { return c.masters.get(id) }
