package tmdb

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// foundPerson is someone as TMDB's search finds them: their name as TMDB
// has it, often in English.
type foundPerson struct {
	ID          uint64 `json:"id"`
	Name        string `json:"name"`
	ProfilePath string `json:"profile_path"`
	KnownFor    string `json:"known_for_department"`
	Gender      int    `json:"gender"`
	Adult       bool   `json:"adult"`
}

type peopleResult struct {
	Page         int           `json:"page"`
	TotalPages   int           `json:"total_pages"`
	TotalResults int           `json:"total_results"`
	Results      []foundPerson `json:"results"`
}

// searchPeople is a page of the people TMDB finds by a name, in any of the
// names it has of them, the best known first.
func (c *client) searchPeople(ctx context.Context, query string, page int) (peopleResult, error) {
	var result peopleResult
	err := c.get(ctx, "/search/person", url.Values{
		"query": {query}, "language": {"ru-RU"}, "include_adult": {"false"}, "page": {strconv.Itoa(page)},
	}, &result)
	return result, err
}

// profession is what someone is known for in Russian, by TMDB's
// department; an actress for a woman.
func profession(department string, gender int) string {
	if department == "Acting" && gender == GenderFemale {
		return "Актриса"
	}
	return professions[department]
}

// PersonCard is what a search tells of someone: who they are, in Russian
// where TMDB has their name so, and the titles they are in or made.
type PersonCard struct {
	ID           uint64 `json:"id"`
	Name         string `json:"name"`
	OriginalName string `json:"original_name,omitempty"`
	Profile      string `json:"profile,omitempty"`
	Profession   string `json:"profession,omitempty"`
	Gender       int    `json:"gender,omitempty"`
	// Their titles as kind:id, the best known first.
	Titles []string `json:"titles,omitempty"`
	// The best known of them with a poster, knownForShown at most.
	KnownFor []Related `json:"known_for,omitempty"`
}

// How many titles a person found tells they are known for.
const knownForShown = 3

func (PersonCard) ttl(time.Time) time.Duration { return longTTL }

// cardOf is the card of someone of their details: what they are known for
// as their page has it, a series only if they were in 5 episodes of it or
// more.
func cardOf(p PersonDetails) PersonCard {
	card := PersonCard{
		ID: p.ID, Name: p.Name, OriginalName: p.OriginalName, Profile: p.Profile,
		Profession: p.Profession, Gender: p.Gender,
	}
	for _, c := range p.Credits {
		card.Titles = append(card.Titles, titleKey(c.Kind, c.ID))
		if len(card.KnownFor) < knownForShown && c.Poster != "" && !(c.Kind == KindTV && c.Episodes > 0 && c.Episodes < 5) {
			card.KnownFor = append(card.KnownFor, c.Related)
		}
	}
	return card
}

func titleKey(kind string, id uint64) string { return kind + ":" + strconv.FormatUint(id, 10) }

// personCardVersion changes the key a card is cached by when what is kept
// of it changes.
const personCardVersion = 1

// FoundPerson is someone a search found, with how many of their titles
// were watched.
type FoundPerson struct {
	PersonCard
	Watched int
}

// People is a page of the people a search found.
type People struct {
	Page, Pages, Total int
	People             []FoundPerson
}

// How many people's cards are fetched at once.
const cardFetches = 8

// SearchPeople is a page of the people TMDB finds by name (page from 1),
// those of whom titles were watched first, the most first: watched are
// TMDB's titles as kind:id, and title-api's titles, those matched. Each
// person's card is cached; one that can't be fetched is told as the search
// has them.
func (s *Service) SearchPeople(ctx context.Context, query string, page int, watched, titleIDs []string) (People, error) {
	tmdb, err := s.keyed(ctx)
	if err != nil {
		return People{}, err
	}
	found, err := tmdb.searchPeople(ctx, query, page)
	if err != nil {
		return People{}, err
	}
	seen := map[string]bool{}
	for _, key := range watched {
		seen[key] = true
	}
	matches, err := s.store.matches(ctx, titleIDs)
	if err != nil {
		log.Printf("tmdb matches: %v", err)
	}
	for _, key := range matches {
		seen[key] = true
	}

	var people []FoundPerson
	for _, p := range found.Results {
		if !p.Adult {
			people = append(people, FoundPerson{PersonCard: PersonCard{
				ID: p.ID, Name: strings.TrimSpace(p.Name), Profile: file(p.ProfilePath),
				Profession: profession(p.KnownFor, p.Gender), Gender: p.Gender,
			}})
		}
	}
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(cardFetches)
	for i := range people {
		id := people[i].ID
		group.Go(func() error {
			card, err := cached(groupCtx, s.cache, fmt.Sprintf("personcard:%d:v%d", id, personCardVersion), PersonCard.ttl,
				func(ctx context.Context) (PersonCard, error) {
					raw, err := tmdb.person(ctx, id)
					if err != nil {
						return PersonCard{}, err
					}
					return cardOf(pickPerson(raw)), nil
				})
			if err != nil {
				log.Printf("tmdb person card %d: %v", id, err)
				return nil
			}
			people[i].PersonCard = card
			return nil
		})
	}
	_ = group.Wait()
	for i := range people {
		for _, key := range people[i].Titles {
			if seen[key] {
				people[i].Watched++
			}
		}
	}
	slices.SortStableFunc(people, func(a, b FoundPerson) int { return -cmp.Compare(a.Watched, b.Watched) })
	return People{Page: found.Page, Pages: found.TotalPages, Total: found.TotalResults, People: people}, nil
}
