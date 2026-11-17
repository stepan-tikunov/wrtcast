// Package tmdb matches title-api's titles with TMDB (themoviedb.org): in
// the background, each title the first time its details are asked for, by
// TMDB's search, and with Claude when the search finds no title or several
// that fit.
package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const apiURL = "https://api.themoviedb.org/3"

// ErrInvalidKey is a key TMDB refused.
var ErrInvalidKey = errors.New("TMDB refused the key")

// The kinds of titles on TMDB, as its paths name them.
const (
	KindMovie = "movie"
	KindTV    = "tv"
)

// client asks TMDB's API with a key: an API key (v3), or a read access
// token (v4), which is a JWT.
type client struct {
	http    *http.Client
	baseURL string
	key     string
}

func (c *client) get(ctx context.Context, path string, query url.Values, result any) error {
	if query == nil {
		query = url.Values{}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	if strings.Contains(c.key, ".") {
		request.Header.Set("Authorization", "Bearer "+c.key)
	} else {
		query.Set("api_key", c.key)
	}
	request.URL.RawQuery = query.Encode()
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		// The error's URL may carry the key.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("TMDB %s: %w", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("TMDB %s: %w", path, err)
	}
	switch {
	case response.StatusCode == http.StatusUnauthorized:
		return ErrInvalidKey
	case response.StatusCode == http.StatusNotFound:
		return fmt.Errorf("TMDB %s: %w", path, errNotFound)
	case response.StatusCode != http.StatusOK:
		return fmt.Errorf("TMDB %s: %s", path, response.Status)
	}
	return json.Unmarshal(body, result)
}

var errNotFound = errors.New("not found")

// check tells whether TMDB takes the key.
func (c *client) check(ctx context.Context) error {
	var result struct {
		Success bool `json:"success"`
	}
	if err := c.get(ctx, "/authentication", nil, &result); err != nil {
		return err
	}
	if !result.Success {
		return ErrInvalidKey
	}
	return nil
}

// Candidate is a title on TMDB, its names in Russian where TMDB has them.
type Candidate struct {
	ID            uint64   `json:"id"`
	Kind          string   `json:"type"`
	Title         string   `json:"title"`
	OriginalTitle string   `json:"original_title"`
	Date          string   `json:"date,omitempty"`
	Countries     []string `json:"countries,omitempty"`
	Overview      string   `json:"overview,omitempty"`
	Votes         int      `json:"votes"`
}

// Year is the year the title came out, if TMDB knows.
func (c Candidate) Year() string {
	if len(c.Date) >= 4 {
		return c.Date[:4]
	}
	return ""
}

// item is a movie or a TV show as TMDB's search and details have it.
type item struct {
	ID            uint64   `json:"id"`
	Title         string   `json:"title"`
	Name          string   `json:"name"`
	OriginalTitle string   `json:"original_title"`
	OriginalName  string   `json:"original_name"`
	ReleaseDate   string   `json:"release_date"`
	FirstAirDate  string   `json:"first_air_date"`
	OriginCountry []string `json:"origin_country"`
	Overview      string   `json:"overview"`
	VoteCount     int      `json:"vote_count"`
}

func (i item) candidate(kind string) Candidate {
	if kind == KindTV {
		i.Title, i.OriginalTitle, i.ReleaseDate = i.Name, i.OriginalName, i.FirstAirDate
	}
	overview := []rune(i.Overview)
	if len(overview) > 300 {
		overview = append(overview[:300], '…')
	}
	return Candidate{
		ID: i.ID, Kind: kind, Title: i.Title, OriginalTitle: i.OriginalTitle, Date: i.ReleaseDate,
		Countries: i.OriginCountry, Overview: string(overview), Votes: i.VoteCount,
	}
}

// search is the first page of what TMDB finds of query among the titles
// of kind, from the year given (empty for any).
func (c *client) search(ctx context.Context, kind, query, year string) ([]Candidate, error) {
	values := url.Values{"query": {query}, "language": {"ru-RU"}, "include_adult": {"false"}}
	if year != "" {
		if kind == KindTV {
			values.Set("first_air_date_year", year)
		} else {
			values.Set("year", year)
		}
	}
	var result struct {
		Results []item `json:"results"`
	}
	if err := c.get(ctx, "/search/"+kind, values, &result); err != nil {
		return nil, err
	}
	candidates := make([]Candidate, 0, len(result.Results))
	for _, x := range result.Results {
		candidates = append(candidates, x.candidate(kind))
	}
	return candidates, nil
}

// title is a title on TMDB by its kind and ID.
func (c *client) title(ctx context.Context, kind string, id uint64) (Candidate, error) {
	var result item
	err := c.get(ctx, "/"+kind+"/"+strconv.FormatUint(id, 10), url.Values{"language": {"ru-RU"}}, &result)
	return result.candidate(kind), err
}
