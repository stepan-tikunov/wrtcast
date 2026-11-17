package provider

import (
	"context"
	"errors"
	"time"
)

// A series' episodes are kept this long (see Series): its page and the
// main page's badges ask the provider for them no more often, however
// often they are opened.
const seriesTTL = 5 * time.Minute

// seriesFetchTimeout bounds a fetch of a series' episodes, which goes on
// once the request that started it gives up, to be kept for the next one.
const seriesFetchTimeout = 2 * time.Minute

type seriesEntry struct {
	seasons []Season
	// ErrNotSeries, kept as well: a movie stays one.
	err     error
	fetched time.Time
}

// Series are a series' episodes on the provider by season, as fetched
// within seriesTTL. A series is fetched once at a time, and the fetch
// goes on when the caller that started it gives up. The seasons are
// shared: they are not to be changed.
func (c *Client) Series(ctx context.Context, externalMovieID string) ([]Season, error) {
	c.seriesMu.Lock()
	entry, ok := c.series[externalMovieID]
	c.seriesMu.Unlock()
	if ok && time.Since(entry.fetched) < seriesTTL {
		return entry.seasons, entry.err
	}
	result := c.seriesFlight.DoChan(externalMovieID, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), seriesFetchTimeout)
		defer cancel()
		seasons, err := c.fetchSeries(ctx, externalMovieID)
		if err == nil || errors.Is(err, ErrNotSeries) {
			c.keepSeries(externalMovieID, seriesEntry{seasons: seasons, err: err, fetched: time.Now()})
		}
		return seasons, err
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-result:
		seasons, _ := r.Val.([]Season)
		return seasons, r.Err
	}
}

// keepSeries keeps a series' episodes, forgetting those kept too long.
func (c *Client) keepSeries(externalMovieID string, entry seriesEntry) {
	c.seriesMu.Lock()
	defer c.seriesMu.Unlock()
	for id, kept := range c.series {
		if time.Since(kept.fetched) >= seriesTTL {
			delete(c.series, id)
		}
	}
	c.series[externalMovieID] = entry
}
