package tmdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"
)

// How long a fetch for the cache may take.
const fetchTimeout = time.Minute

// cache keeps what was chosen from TMDB's answers in the state database,
// by keys such as movie:550, tv:1405, tv:1405:s2 and collection:726871.
// An entry past its time is served at once and fetched again in the
// background; an entry missing is fetched while the caller waits. A key
// is fetched once at a time.
type cache struct {
	db  *sql.DB
	now func() time.Time

	mu      sync.Mutex
	flights map[string]*flight
}

type flight struct {
	done chan struct{}
	data []byte
	err  error
}

func newCache(db *sql.DB) *cache {
	return &cache{db: db, now: time.Now, flights: map[string]*flight{}}
}

func (c *cache) read(ctx context.Context, key string) ([]byte, time.Time, bool, error) {
	var data string
	var fetched int64
	err := c.db.QueryRowContext(ctx, "SELECT json, fetched FROM tmdb_cache WHERE key = ?", key).Scan(&data, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, err
	}
	return []byte(data), time.Unix(fetched, 0), true, nil
}

func (c *cache) write(ctx context.Context, key string, data []byte) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO tmdb_cache (key, json, fetched) VALUES (?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET json = excluded.json, fetched = excluded.fetched`,
		key, string(data), c.now().Unix())
	return err
}

// fetch fetches a key and keeps it, unless it is being fetched already.
func (c *cache) fetch(key string, fetch func(context.Context) ([]byte, error)) *flight {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f := c.flights[key]; f != nil {
		return f
	}
	f := &flight{done: make(chan struct{})}
	c.flights[key] = f
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		f.data, f.err = fetch(ctx)
		if f.err == nil {
			if err := c.write(ctx, key, f.data); err != nil {
				log.Printf("tmdb cache %s: %v", key, err)
			}
		}
		c.mu.Lock()
		delete(c.flights, key)
		c.mu.Unlock()
		close(f.done)
	}()
	return f
}

// cached is the value of a key as the cache has it, fresh for ttl of it.
func cached[T any](
	ctx context.Context, c *cache, key string,
	ttl func(T, time.Time) time.Duration,
	fetch func(context.Context) (T, error),
) (T, error) {
	if value, ok := stored(ctx, c, key, ttl, fetch, false); ok {
		return value, nil
	}
	var value T
	f := c.fetch(key, fetchJSON(fetch))
	select {
	case <-ctx.Done():
		return value, ctx.Err()
	case <-f.done:
	}
	if f.err != nil {
		return value, f.err
	}
	err := json.Unmarshal(f.data, &value)
	return value, err
}

// stored is the value of a key as the cache has it, fetched again in the
// background once past its time; missing, it is fetched in the
// background as well if fetchMissing, never waited for.
func stored[T any](
	ctx context.Context, c *cache, key string,
	ttl func(T, time.Time) time.Duration,
	fetch func(context.Context) (T, error),
	fetchMissing bool,
) (T, bool) {
	var value T
	data, fetched, ok, err := c.read(ctx, key)
	if err != nil {
		log.Printf("tmdb cache %s: %v", key, err)
	}
	ok = ok && json.Unmarshal(data, &value) == nil
	if ok && c.now().Sub(fetched) < ttl(value, c.now()) || !ok && !fetchMissing {
		return value, ok
	}
	f := c.fetch(key, fetchJSON(fetch))
	go func() {
		<-f.done
		if f.err != nil {
			log.Printf("tmdb cache %s: %v", key, f.err)
		}
	}()
	return value, ok
}

func fetchJSON[T any](fetch func(context.Context) (T, error)) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		value, err := fetch(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}
}
