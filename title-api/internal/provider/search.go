package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

var (
	moviePathPattern = regexp.MustCompile(`/([0-9]+)-[^/]+\.html$`)
	yearPattern      = regexp.MustCompile(`\b(?:19|20)[0-9]{2}\b`)
)

var ErrInvalidSearchQuery = errors.New("invalid search query")

type SearchResult struct {
	TitleID   string
	Title     string
	Year      string
	PosterURL string
}

func (c *Client) Search(ctx context.Context, query string) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if utf8.RuneCountInString(query) < 3 {
		return nil, fmt.Errorf("%w: query must contain at least 3 characters", ErrInvalidSearchQuery)
	}

	current, err := c.currentSession()
	if err != nil {
		return nil, err
	}
	body, err := c.searchPage(ctx, current, query)
	if errors.Is(err, errAntiBotChallenge) {
		if refreshErr := c.refreshIfStale(ctx, current.generation); refreshErr != nil {
			return nil, fmt.Errorf("refresh rejected anti-bot session: %w", refreshErr)
		}
		current, err = c.currentSession()
		if err != nil {
			return nil, err
		}
		body, err = c.searchPage(ctx, current, query)
	}
	if err != nil {
		return nil, err
	}

	results, err := parseSearchResults(body, c.baseURL, c.domainPrefix)
	if err != nil {
		return nil, fmt.Errorf("parse search results: %w", err)
	}
	return results, nil
}

func (c *Client) searchPage(ctx context.Context, current *session, query string) ([]byte, error) {
	target := c.baseURL.ResolveReference(&url.URL{Path: "/search-result/"})
	form := url.Values{
		"do":           {"search"},
		"subaction":    {"search"},
		"story":        {query},
		"search_start": {"0"},
		"full_search":  {"0"},
		"result_from":  {"1"},
	}
	body, _, err := c.postForm(ctx, current, target.String(), form, c.baseURL.String()+"/")
	if err != nil {
		return nil, fmt.Errorf("fetch search results: %w", err)
	}
	if isChallenge(body) {
		return nil, errAntiBotChallenge
	}
	return body, nil
}

func parseSearchResults(body []byte, baseURL *url.URL, domainPrefix string) ([]SearchResult, error) {
	document, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	results := make([]SearchResult, 0)
	forEachElement(document, func(node *html.Node) {
		if !hasClass(node, "th-item") {
			return
		}
		anchor := findElement(node, func(candidate *html.Node) bool {
			return candidate.Data == "a" && movieSiteID(attribute(candidate, "href")) != ""
		})
		if anchor == nil {
			return
		}
		siteID := movieSiteID(attribute(anchor, "href"))
		titleNode := findElement(node, func(candidate *html.Node) bool {
			return hasClass(candidate, "th-title")
		})
		title := strings.TrimSpace(textContent(titleNode))
		if title == "" {
			image := findElement(node, func(candidate *html.Node) bool { return candidate.Data == "img" })
			title = strings.TrimSpace(attribute(image, "alt"))
		}

		yearNode := findElement(node, func(candidate *html.Node) bool {
			return hasClass(candidate, "th-year") || hasClass(candidate, "year")
		})
		year := yearPattern.FindString(textContent(yearNode))
		if year == "" {
			year = yearPattern.FindString(textContent(node))
		}

		poster := ""
		image := findElement(node, func(candidate *html.Node) bool { return candidate.Data == "img" })
		for _, name := range []string{"data-src", "data-lazy-src", "src"} {
			if value := strings.TrimSpace(attribute(image, name)); value != "" && !strings.HasPrefix(value, "data:") {
				poster = resolveURL(baseURL, value)
				break
			}
		}

		results = append(results, SearchResult{
			TitleID:   domainPrefix + siteID,
			Title:     title,
			Year:      year,
			PosterURL: poster,
		})
	})
	return results, nil
}

func movieSiteID(link string) string {
	parsed, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return ""
	}
	match := moviePathPattern.FindStringSubmatch(parsed.Path)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func resolveURL(baseURL *url.URL, reference string) string {
	parsed, err := url.Parse(reference)
	if err != nil {
		return ""
	}
	return baseURL.ResolveReference(parsed).String()
}

func forEachElement(node *html.Node, visit func(*html.Node)) {
	if node == nil {
		return
	}
	if node.Type == html.ElementNode {
		visit(node)
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		forEachElement(child, visit)
	}
}

func findElement(node *html.Node, predicate func(*html.Node) bool) *html.Node {
	if node == nil {
		return nil
	}
	if node.Type == html.ElementNode && predicate(node) {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := findElement(child, predicate); found != nil {
			return found
		}
	}
	return nil
}

func hasClass(node *html.Node, className string) bool {
	for _, current := range strings.Fields(attribute(node, "class")) {
		if current == className {
			return true
		}
	}
	return false
}

func attribute(node *html.Node, name string) string {
	if node == nil {
		return ""
	}
	for _, item := range node.Attr {
		if item.Key == name {
			return item.Val
		}
	}
	return ""
}

func textContent(node *html.Node) string {
	if node == nil {
		return ""
	}
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			builder.WriteString(current.Data)
			builder.WriteByte(' ')
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.Join(strings.Fields(builder.String()), " ")
}
