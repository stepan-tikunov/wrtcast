package tmdb

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// How many of someone's portraits and of the pictures they are tagged in
// Photos keep at most.
const (
	profilesKept = 8
	taggedKept   = 12
)

// Photos are the pictures of someone on TMDB that their face can be told
// by: their portraits, the most voted first, and the pictures of titles
// they are tagged in, those of stills first. Files are of /v1/tmdb/images.
type Photos struct {
	Profiles []string      `json:"profiles,omitempty"`
	Tagged   []TaggedPhoto `json:"tagged,omitempty"`
}

// TaggedPhoto is a picture of a title someone is tagged in, which shows
// others as well, mostly.
type TaggedPhoto struct {
	File string `json:"file"`
	// The title's kind (movie or tv) and TMDB ID.
	Kind    string `json:"kind"`
	MediaID uint64 `json:"media_id"`
	// backdrop, still or poster.
	Type   string `json:"type"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func (Photos) ttl(time.Time) time.Duration { return longTTL }

type rawTagged struct {
	image
	MediaType string `json:"media_type"`
	ImageType string `json:"image_type"`
	Media     struct {
		ID uint64 `json:"id"`
	} `json:"media"`
}

// photos are someone's portraits and the first page of the pictures they
// are tagged in.
func (c *client) photos(ctx context.Context, id uint64) (Photos, error) {
	path := "/person/" + strconv.FormatUint(id, 10)
	var profiles struct {
		Profiles []image `json:"profiles"`
	}
	if err := c.get(ctx, path+"/images", nil, &profiles); err != nil {
		return Photos{}, err
	}
	var tagged struct {
		Results []rawTagged `json:"results"`
	}
	if err := c.get(ctx, path+"/tagged_images", url.Values{"page": {"1"}}, &tagged); err != nil && !errors.Is(err, errNotFound) {
		return Photos{}, err
	}
	slices.SortStableFunc(profiles.Profiles, func(a, b image) int {
		return cmp.Or(cmp.Compare(b.VoteCount, a.VoteCount), cmp.Compare(b.VoteAverage, a.VoteAverage))
	})
	var result Photos
	for _, p := range profiles.Profiles {
		if len(result.Profiles) < profilesKept && p.FilePath != "" {
			result.Profiles = append(result.Profiles, file(p.FilePath))
		}
	}
	// Stills show people as they are on the screen; posters are drawn
	// over more often than not.
	rank := map[string]int{"still": 0, "backdrop": 1, "poster": 2}
	slices.SortStableFunc(tagged.Results, func(a, b rawTagged) int {
		return cmp.Compare(rank[a.ImageType], rank[b.ImageType])
	})
	for _, t := range tagged.Results {
		if len(result.Tagged) < taggedKept && t.FilePath != "" && (t.MediaType == KindMovie || t.MediaType == KindTV) {
			result.Tagged = append(result.Tagged, TaggedPhoto{
				File: file(t.FilePath), Kind: t.MediaType, MediaID: t.Media.ID, Type: t.ImageType,
				Width: t.Width, Height: t.Height,
			})
		}
	}
	return result, nil
}

// Photos are someone's pictures on TMDB to tell their face by, as cached.
func (s *Service) Photos(ctx context.Context, id uint64) (Photos, error) {
	tmdb, err := s.keyed(ctx)
	if err != nil {
		return Photos{}, err
	}
	photos, err := cached(ctx, s.cache, fmt.Sprintf("photos:%d", id), Photos.ttl, func(ctx context.Context) (Photos, error) {
		return tmdb.photos(ctx, id)
	})
	if errors.Is(err, errNotFound) {
		return Photos{}, ErrNotFound
	}
	return photos, err
}

// EpisodeCast are the people of an episode on TMDB: of the series' cast
// those in it, then its guest stars, each as billed.
type EpisodeCast struct {
	People []Person `json:"people,omitempty"`
}

func (EpisodeCast) ttl(time.Time) time.Duration { return 7 * 24 * time.Hour }

// EpisodeCast are the people TMDB tells are in an episode of a series, as
// cached: those of its cast first, then its guest stars; an episode TMDB
// doesn't have has none.
func (s *Service) EpisodeCast(ctx context.Context, id uint64, season, episode int32) ([]Person, error) {
	tmdb, err := s.keyed(ctx)
	if err != nil {
		return nil, err
	}
	people, err := cached(ctx, s.cache, fmt.Sprintf("%s:%d:s%d:e%d:cast", KindTV, id, season, episode), EpisodeCast.ttl,
		func(ctx context.Context) (EpisodeCast, error) {
			var raw struct {
				Cast       []castMember `json:"cast"`
				GuestStars []castMember `json:"guest_stars"`
			}
			err := tmdb.get(ctx, fmt.Sprintf("/tv/%d/season/%d/episode/%d/credits", id, season, episode),
				url.Values{"language": {"ru-RU"}}, &raw)
			if errors.Is(err, errNotFound) {
				return EpisodeCast{}, nil
			}
			var result EpisodeCast
			seen := map[uint64]bool{}
			for _, member := range append(raw.Cast, raw.GuestStars...) {
				if member.ID == 0 || seen[member.ID] {
					continue
				}
				seen[member.ID] = true
				result.People = append(result.People, Person{
					ID: member.ID, Name: member.Name, Character: strings.TrimSpace(member.Character), Profile: file(member.ProfilePath),
				})
			}
			return result, err
		})
	return people.People, err
}
