package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

const wikidataURL = "https://www.wikidata.org/w/api.php"

// Kinopoisk's ID of a person, a property of Wikidata.
const kinopoiskPerson = "P2604"

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

// personLinks are a person's article on Wikipedia, in Russian, else in
// English, and their ID on Kinopoisk, as Wikidata has them by its ID of
// them.
func (s *Service) personLinks(ctx context.Context, id string) (wikipedia, kinopoisk string, err error) {
	if !wikidataID.MatchString(id) {
		return "", "", errors.New("wikidata: bad ID " + id)
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
		return "", "", err
	}
	links := entities.Entities[id].Sitelinks
	wikipedia = links["ruwiki"].URL
	if wikipedia == "" {
		wikipedia = links["enwiki"].URL
	}
	var claims struct {
		Claims map[string][]struct {
			Mainsnak struct {
				Datavalue struct {
					Value any `json:"value"`
				} `json:"datavalue"`
			} `json:"mainsnak"`
		} `json:"claims"`
	}
	if err := s.wikidata(ctx, url.Values{
		"action": {"wbgetclaims"}, "entity": {id}, "property": {kinopoiskPerson},
	}, &claims); err != nil {
		return wikipedia, "", err
	}
	for _, claim := range claims.Claims[kinopoiskPerson] {
		if value, ok := claim.Mainsnak.Datavalue.Value.(string); ok && value != "" {
			return wikipedia, value, nil
		}
	}
	return wikipedia, "", nil
}
