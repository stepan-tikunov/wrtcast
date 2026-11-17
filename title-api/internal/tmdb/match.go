package tmdb

import (
	"context"
	"strings"
	"unicode"
)

// Title is a title of title-api, as the provider's page tells of it.
type Title struct {
	ID            string `json:"-"`
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title,omitempty"`
	Year          string `json:"year,omitempty"`
	// KindMovie, KindTV, or empty when the provider's page doesn't tell.
	Kind        string   `json:"type,omitempty"`
	Countries   []string `json:"countries,omitempty"`
	Directors   []string `json:"directors,omitempty"`
	Actors      []string `json:"actors,omitempty"`
	Description string   `json:"description,omitempty"`
}

func (t Title) kinds() []string {
	if t.Kind != "" {
		return []string{t.Kind}
	}
	return []string{KindMovie, KindTV}
}

// candidates are what TMDB's search finds of the title by its original
// name and its Russian one, each title once, those of its kind.
func (c *client) candidates(ctx context.Context, t Title) ([]Candidate, error) {
	var result []Candidate
	type key struct {
		id   uint64
		kind string
	}
	seen := map[key]bool{}
	for _, query := range []string{t.OriginalTitle, t.Title} {
		if strings.TrimSpace(query) == "" {
			continue
		}
		for _, kind := range t.kinds() {
			found, err := c.search(ctx, kind, query, "")
			if err != nil {
				return nil, err
			}
			for _, x := range found {
				if k := (key{x.ID, x.Kind}); !seen[k] {
					seen[k] = true
					result = append(result, x)
				}
			}
		}
	}
	return result, nil
}

// exact is the one candidate that is the title by its name and year, if
// there is just one.
func exact(t Title, candidates []Candidate) (Candidate, bool) {
	names := map[string]bool{}
	for _, name := range []string{t.Title, t.OriginalTitle} {
		if key := normalize(name); key != "" {
			names[key] = true
		}
	}
	year := yearOf(t.Year)
	var found []Candidate
	for _, x := range candidates {
		if year == "" || x.Year() != year || t.Kind != "" && x.Kind != t.Kind {
			continue
		}
		if names[normalize(x.Title)] || names[normalize(x.OriginalTitle)] {
			found = append(found, x)
		}
	}
	if len(found) != 1 {
		return Candidate{}, false
	}
	return found[0], true
}

// normalize is a name as names are compared: letters and digits alone, in
// lower case, ё as е.
func normalize(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r == 'ё':
			b.WriteRune('е')
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// yearOf is the first year in the provider's year, such as "2006-2013".
func yearOf(value string) string {
	for i := 0; i+4 <= len(value); i++ {
		digits := value[i : i+4]
		if strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
			return digits
		}
	}
	return ""
}
