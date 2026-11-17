package tmdb

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

var (
	// ErrNoKey is a request for TMDB's data with no key set.
	ErrNoKey = errors.New("no TMDB key is set")
	// ErrNotFound is a title TMDB doesn't have.
	ErrNotFound = errors.New("TMDB has no such title")
)

// countryNames are the countries TMDB names otherwise than the provider.
var countryNames = map[string]string{"US": "США", "KR": "Южная Корея", "KP": "КНДР"}

// keyed is TMDB's client with the key kept.
func (s *Service) keyed(ctx context.Context) (*client, error) {
	key, err := s.store.setting(ctx, "key")
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, ErrNoKey
	}
	return s.tmdb(key), nil
}

// Details are a title on TMDB by its kind and ID, whether title-api has it
// or not: as cached, its collection's parts with it, the countries named
// in Russian, and title-api's titles of it and of the titles it lists.
func (s *Service) Details(ctx context.Context, kind string, id uint64) (Details, error) {
	tmdb, err := s.keyed(ctx)
	if err != nil {
		return Details{}, err
	}
	details, err := s.details(ctx, tmdb, kind, id)
	if err != nil {
		return Details{}, err
	}
	if details.CollectionID != 0 {
		collection, err := cached(ctx, s.cache, fmt.Sprintf("collection:%d", details.CollectionID), Collection.ttl,
			func(ctx context.Context) (Collection, error) {
				raw, err := tmdb.collection(ctx, details.CollectionID)
				return pickCollection(raw), err
			})
		if err != nil {
			log.Printf("tmdb collection %d: %v", details.CollectionID, err)
		} else {
			details.Collection = &collection
		}
	}
	names, err := cached(ctx, s.cache, "countries:ru",
		func(map[string]string, time.Time) time.Duration { return longTTL }, tmdb.countries)
	if err != nil {
		log.Printf("tmdb countries: %v", err)
	}
	for i, code := range details.Countries {
		if name := cmp.Or(countryNames[code], names[code]); name != "" {
			details.Countries[i] = name
		}
	}
	s.titleIDs(ctx, &details)
	return details, nil
}

// detailsVersion changes the key Details are cached by when what is kept
// of a title changes: 2 keeps the whole cast, not 12 with photos.
const detailsVersion = 2

// details are a title on TMDB as cached.
func (s *Service) details(ctx context.Context, tmdb *client, kind string, id uint64) (Details, error) {
	details, err := cached(ctx, s.cache, fmt.Sprintf("%s:%d:v%d", kind, id, detailsVersion), Details.ttl,
		func(ctx context.Context) (Details, error) {
			raw, err := tmdb.details(ctx, kind, id)
			if err != nil {
				return Details{}, err
			}
			return pickDetails(kind, raw), nil
		})
	if errors.Is(err, errNotFound) {
		return Details{}, ErrNotFound
	}
	return details, err
}

// Cast are the people in a title on TMDB, as billed, as Details keep them.
func (s *Service) Cast(ctx context.Context, kind string, id uint64) ([]Person, error) {
	tmdb, err := s.keyed(ctx)
	if err != nil {
		return nil, err
	}
	details, err := s.details(ctx, tmdb, kind, id)
	return details.Cast, err
}

// titleIDs tells title-api's titles of a title and of those it lists, of
// them that are matched.
func (s *Service) titleIDs(ctx context.Context, details *Details) {
	related := []*Related{}
	for i := range details.Recommendations {
		related = append(related, &details.Recommendations[i])
	}
	if details.Collection != nil {
		for i := range details.Collection.Parts {
			related = append(related, &details.Collection.Parts[i])
		}
	}
	ids := map[string][]uint64{details.Kind: {details.ID}}
	for _, r := range related {
		ids[r.Kind] = append(ids[r.Kind], r.ID)
	}
	titles := map[string]map[uint64]string{}
	for kind, list := range ids {
		found, err := s.store.titleIDs(ctx, kind, list)
		if err != nil {
			log.Printf("tmdb titles: %v", err)
		}
		titles[kind] = found
	}
	details.TitleID = titles[details.Kind][details.ID]
	for _, r := range related {
		r.TitleID = titles[r.Kind][r.ID]
	}
}

// Season is a season of a series on TMDB, as cached; one TMDB doesn't
// have has no episodes.
func (s *Service) Season(ctx context.Context, id uint64, number int32) (Season, error) {
	tmdb, err := s.keyed(ctx)
	if err != nil {
		return Season{}, err
	}
	return cached(ctx, s.cache, fmt.Sprintf("%s:%d:s%d", KindTV, id, number), Season.ttl,
		func(ctx context.Context) (Season, error) {
			today := s.cache.now().Format(time.DateOnly)
			raw, err := tmdb.season(ctx, id, number, today)
			return pickSeason(raw, today), err
		})
}
