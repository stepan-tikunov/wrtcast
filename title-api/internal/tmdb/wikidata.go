package tmdb

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const wikidataURL = "https://www.wikidata.org/w/api.php"

var wikidataID = regexp.MustCompile(`^Q[0-9]+$`)

// wikidata asks Wikidata's API, as its policy asks, with a User-Agent of
// title-api's.
func (s *Service) wikidata(ctx context.Context, query url.Values, result any) error {
	query.Set("format", "json")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.wikidataURL+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "title-api/1 (https://github.com/stepan-tikunov/wrtcast)")
	client := &http.Client{Transport: s.http.Transport, Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("wikidata: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("wikidata %s: %s", query.Get("action"), response.Status)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result)
}

// Properties of Wikidata: a person's Kinopoisk ID and place of birth, a
// place's country.
const (
	kinopoiskPerson = "P2604"
	placeOfBirth    = "P19"
	country         = "P17"
)

// fromWikidata is what Wikidata has of someone that TMDB lacks.
type fromWikidata struct {
	// Their article, in Russian, else in English.
	Wikipedia string
	Kinopoisk string
	// Where they were born, in Russian: the place and its country.
	Place string
}

// personWikidata is what Wikidata has of someone by its ID of them.
func (s *Service) personWikidata(ctx context.Context, id string) (fromWikidata, error) {
	var result fromWikidata
	if !wikidataID.MatchString(id) {
		return result, errors.New("wikidata: bad ID " + id)
	}
	var entities struct {
		Entities map[string]struct {
			Sitelinks map[string]struct {
				URL string `json:"url"`
			} `json:"sitelinks"`
		} `json:"entities"`
	}
	if err := s.wikidata(ctx, url.Values{
		"action": {"wbgetentities"}, "ids": {id}, "props": {"sitelinks/urls"}, "sitefilter": {"ruwiki|enwiki"},
	}, &entities); err != nil {
		return result, err
	}
	links := entities.Entities[id].Sitelinks
	result.Wikipedia = cmp.Or(links["ruwiki"].URL, links["enwiki"].URL)
	kinopoisk, err := s.claim(ctx, id, kinopoiskPerson)
	if err != nil {
		return result, err
	}
	result.Kinopoisk, _ = kinopoisk.(string)
	result.Place, err = s.place(ctx, id)
	return result, err
}

// place is where someone was born, in Russian: "Чикаго, США"; empty when
// Wikidata doesn't know or has no Russian name of it.
func (s *Service) place(ctx context.Context, id string) (string, error) {
	place, err := s.claimedItem(ctx, id, placeOfBirth)
	if place == "" || err != nil {
		return "", err
	}
	ids := []string{place}
	if land, err := s.claimedItem(ctx, place, country); err != nil {
		return "", err
	} else if land != "" && land != place {
		ids = append(ids, land)
	}
	var entities struct {
		Entities map[string]struct {
			Labels map[string]struct {
				Value string `json:"value"`
			} `json:"labels"`
		} `json:"entities"`
	}
	if err := s.wikidata(ctx, url.Values{
		"action": {"wbgetentities"}, "ids": {strings.Join(ids, "|")}, "props": {"labels"}, "languages": {"ru"},
	}, &entities); err != nil {
		return "", err
	}
	var names []string
	for _, id := range ids {
		if name := entities.Entities[id].Labels["ru"].Value; name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 || entities.Entities[place].Labels["ru"].Value == "" {
		return "", nil
	}
	return strings.Join(names, ", "), nil
}

// claim is the value of an entity's first statement of a property, nil
// if it has none.
func (s *Service) claim(ctx context.Context, id, property string) (any, error) {
	var claims struct {
		Claims map[string][]struct {
			Mainsnak struct {
				Datavalue struct {
					Value any `json:"value"`
				} `json:"datavalue"`
			} `json:"mainsnak"`
		} `json:"claims"`
	}
	if err := s.wikidata(ctx, url.Values{"action": {"wbgetclaims"}, "entity": {id}, "property": {property}}, &claims); err != nil {
		return nil, err
	}
	for _, claim := range claims.Claims[property] {
		if value := claim.Mainsnak.Datavalue.Value; value != nil {
			return value, nil
		}
	}
	return nil, nil
}

// claimedItem is the entity an entity's first statement of a property is
// of: its ID, empty if it has none.
func (s *Service) claimedItem(ctx context.Context, id, property string) (string, error) {
	value, err := s.claim(ctx, id, property)
	item, _ := value.(map[string]any)
	found, _ := item["id"].(string)
	if !wikidataID.MatchString(found) {
		found = ""
	}
	return found, err
}
