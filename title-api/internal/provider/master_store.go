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
	// The alternative player's source the master is of; nil for a ready
	// playlist.
	source *alternativeSource
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

func (store *masterStore) putSource(source *alternativeSource) string {
	return store.add(&storedMaster{resolve: source.resolve, source: source})
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
	entry, err := store.entry(id)
	if err != nil {
		return nil, err
	}
	body, err := entry.playlist(ctx)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), body...), nil
}

func (store *masterStore) entry(id string) (*storedMaster, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.cleanup(time.Now())
	entry, ok := store.entries[id]
	if !ok {
		return nil, ErrMasterNotFound
	}
	return entry, nil
}

// playlist resolves the master on first request. Concurrent requests for
// one master wait for a single resolution; a failed one is retried by the
// next request.
func (entry *storedMaster) playlist(ctx context.Context) ([]byte, error) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.body == nil {
		body, err := entry.resolve(ctx)
		if err != nil {
			return nil, err
		}
		entry.body, entry.resolve = body, nil
	}
	return entry.body, nil
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

// MediaHeaders are the headers the CDN requests of a media need now: the
// player's, with the edge_hash of its playback socket once there is one.
// The request keeps the socket open (see playbackSession) and reports the
// quality and the position the router plays. A master no one asked for yet
// is resolved first, as the master endpoint would: its CDN requests come
// from it, and the socket opens with it.
func (c *Client) MediaHeaders(ctx context.Context, id string, report PlaybackReport) (MediaHeaders, error) {
	entry, err := c.masters.entry(id)
	if err != nil {
		return MediaHeaders{}, err
	}
	if _, err := entry.playlist(ctx); err != nil {
		return MediaHeaders{}, err
	}
	if entry.source == nil {
		return MediaHeaders{TTL: mediaHeadersTTL}, nil
	}
	return entry.source.headers(report), nil
}

// EndMediaSession tells the playback socket of a media, if it has one open,
// that the playback ended, and closes it.
func (c *Client) EndMediaSession(id string) {
	if entry, err := c.masters.entry(id); err == nil && entry.source != nil {
		entry.source.end()
	}
}
