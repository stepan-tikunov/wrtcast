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
	// ErrNotFound is a title or a person TMDB doesn't have.
	ErrNotFound = errors.New("TMDB has no such title or person")
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
// of a title changes: 2 keeps the whole cast, not 12 with photos; 3 the
// people's IDs.
const detailsVersion = 3

// details are a title on TMDB as cached.
func (s *Service) details(ctx context.Context, tmdb *client, kind string, id uint64) (Details, error) {
	details, err := cached(ctx, s.cache, detailsKey(kind, id), Details.ttl, fetchDetails(tmdb, kind, id))
	if errors.Is(err, errNotFound) {
		return Details{}, ErrNotFound
	}
	return details, err
}

func detailsKey(kind string, id uint64) string {
	return fmt.Sprintf("%s:%d:v%d", kind, id, detailsVersion)
}

func fetchDetails(tmdb *client, kind string, id uint64) func(context.Context) (Details, error) {
	return func(ctx context.Context) (Details, error) {
		raw, err := tmdb.details(ctx, kind, id)
		if err != nil {
			return Details{}, err
		}
		return pickDetails(kind, raw), nil
	}
}

// Upcoming is what TMDB has of a series of title-api's (its status and
// next episode), and its match: as kept, never waited for. Its details
// missing or past their time are fetched in the background, for the next
// time. A title matched with a movie has its match alone, as has a series
// with no key or its details not kept; one not matched, nothing.
func (s *Service) Upcoming(ctx context.Context, title string) (Match, *Series, bool) {
	match, ok := s.Match(ctx, title)
	if !ok || match.Kind != KindTV {
		return match, nil, ok
	}
	tmdb, err := s.keyed(ctx)
	if err != nil {
		return match, nil, true
	}
	details, ok := stored(ctx, s.cache, detailsKey(match.Kind, match.ID), Details.ttl,
		fetchDetails(tmdb, match.Kind, match.ID), true)
	if !ok {
		return match, nil, true
	}
	return match, details.Series, true
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
	self := Related{ID: details.ID, Kind: details.Kind}
	related := []*Related{&self}
	for i := range details.Recommendations {
		related = append(related, &details.Recommendations[i])
	}
	if details.Collection != nil {
		for i := range details.Collection.Parts {
			related = append(related, &details.Collection.Parts[i])
		}
	}
	s.relatedTitleIDs(ctx, related)
	details.TitleID = self.TitleID
}

// relatedTitleIDs tells title-api's titles of those of TMDB's that are
// matched.
func (s *Service) relatedTitleIDs(ctx context.Context, related []*Related) {
	ids := map[string][]uint64{}
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
	for _, r := range related {
		r.TitleID = titles[r.Kind][r.ID]
	}
}

// personVersion changes the key a person is cached by when what is kept
// of them changes: 2 has their place of birth in Russian and their
// biography cleaned.
const personVersion = 2

// Person is someone on TMDB by their ID, as cached, with their pages that
// Wikidata knows, and title-api's titles of those they are in.
func (s *Service) Person(ctx context.Context, id uint64) (PersonDetails, error) {
	tmdb, err := s.keyed(ctx)
	if err != nil {
		return PersonDetails{}, err
	}
	person, err := cached(ctx, s.cache, fmt.Sprintf("person:%d:v%d", id, personVersion), PersonDetails.ttl,
		func(ctx context.Context) (PersonDetails, error) {
			raw, err := tmdb.person(ctx, id)
			if err != nil {
				return PersonDetails{}, err
			}
			person := pickPerson(raw)
			if person.Wikidata != "" {
				found, err := s.personWikidata(ctx, person.Wikidata)
				if err != nil {
					log.Printf("tmdb person %d: %v", id, err)
					person.Partial = true
				}
				person.Wikipedia, person.Kinopoisk = found.Wikipedia, found.Kinopoisk
				person.PlaceOfBirth = cmp.Or(found.Place, person.PlaceOfBirth)
			}
			return person, nil
		})
	if errors.Is(err, errNotFound) {
		return PersonDetails{}, ErrNotFound
	}
	if err != nil {
		return PersonDetails{}, err
	}
	related := make([]*Related, len(person.Credits))
	for i := range person.Credits {
		related[i] = &person.Credits[i].Related
	}
	s.relatedTitleIDs(ctx, related)
	return person, nil
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
