package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	nethtml "golang.org/x/net/html"
)

var ErrMediaTypeUnknown = errors.New("media type could not be determined")

type TitleMetadata struct {
	MovieID       string
	Title         string
	OriginalTitle string
	Year          string
	Description   string
	PosterURL     string
	MediaType     MediaType
	Quality       string
	Countries     []string
	Genres        []string
	Directors     []string
	Actors        []string
	Ratings       []Rating
}

type Rating struct {
	Source string
	Value  float64
}

func (c *Client) TitleMetadata(ctx context.Context, externalMovieID string) (TitleMetadata, error) {
	current, movieHTML, embedURLs, err := c.movieDocument(ctx, externalMovieID)
	if err != nil {
		return TitleMetadata{}, err
	}

	metadata, err := parseTitleMetadata(movieHTML, c.baseURL)
	if err != nil {
		return TitleMetadata{}, fmt.Errorf("parse title metadata: %w", err)
	}
	metadata.MovieID = externalMovieID
	metadata.MediaType, err = c.mediaTypeFromEmbeds(ctx, current, embedURLs)
	if err != nil {
		return TitleMetadata{}, err
	}
	return metadata, nil
}

func (c *Client) mediaTypeFromEmbeds(
	ctx context.Context,
	current *session,
	embedURLs []string,
) (MediaType, error) {
	hasMovie := false
	fetchErrors := make([]error, 0)
	for _, embedURL := range embedURLs {
		embedHTML, _, err := c.get(ctx, current, embedURL, c.baseURL.String()+"/")
		if err != nil {
			fetchErrors = append(fetchErrors, fmt.Errorf("%s: %w", embedURL, err))
			continue
		}
		if containsSeriesPlaylist(embedHTML) {
			return MediaTypeSeries, nil
		}
		if hlsPattern.FindSubmatch(embedHTML) != nil {
			hasMovie = true
		}
	}
	if hasMovie {
		return MediaTypeMovie, nil
	}
	if len(fetchErrors) > 0 {
		return MediaTypeUnspecified, fmt.Errorf("inspect player embeds: %w", errors.Join(fetchErrors...))
	}
	return MediaTypeUnspecified, ErrMediaTypeUnknown
}

func parseTitleMetadata(body []byte, baseURL *url.URL) (TitleMetadata, error) {
	document, err := nethtml.Parse(strings.NewReader(string(body)))
	if err != nil {
		return TitleMetadata{}, err
	}

	metadata := TitleMetadata{
		Title:         labeledText(document, "Название:"),
		OriginalTitle: labeledText(document, "Оригинальное название:"),
		Year:          labeledText(document, "Год выхода:"),
		Description:   propertyText(document, "description"),
		Quality:       labeledText(document, "Качество:"),
		Countries:     propertyValues(document, "countryOfOrigin", ","),
		Genres:        propertyValues(document, "genre", "/"),
		Directors:     propertyValues(document, "director", ","),
		Actors:        propertyValues(document, "actors", ","),
		Ratings:       parseRatings(document),
	}
	poster := findElement(document, func(node *nethtml.Node) bool {
		return node.Data == "img" && hasClass(node.Parent, "fposter")
	})
	for _, name := range []string{"data-src", "data-lazy-src", "src"} {
		if value := strings.TrimSpace(attribute(poster, name)); value != "" && !strings.HasPrefix(value, "data:") {
			metadata.PosterURL = resolveURL(baseURL, value)
			break
		}
	}
	if metadata.Title == "" {
		return TitleMetadata{}, errors.New("title not found")
	}
	return metadata, nil
}

func labeledText(document *nethtml.Node, label string) string {
	item := findElement(document, func(node *nethtml.Node) bool {
		if node.Data != "li" {
			return false
		}
		first := findElement(node, func(candidate *nethtml.Node) bool { return candidate.Data == "span" })
		return strings.TrimSpace(textContent(first)) == label
	})
	if item == nil {
		return ""
	}
	var values []string
	for child := item.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == nethtml.ElementNode && child.Data == "span" && strings.TrimSpace(textContent(child)) == label {
			continue
		}
		value := strings.TrimSpace(textContent(child))
		if value != "" {
			values = append(values, value)
		}
	}
	return strings.Join(values, " ")
}

func propertyText(document *nethtml.Node, property string) string {
	node := findElement(document, func(node *nethtml.Node) bool {
		return attribute(node, "itemprop") == property
	})
	return strings.TrimSpace(textContent(node))
}

func propertyValues(document *nethtml.Node, property, separator string) []string {
	node := findElement(document, func(node *nethtml.Node) bool {
		return attribute(node, "itemprop") == property
	})
	if node == nil {
		return nil
	}
	values := make([]string, 0)
	for _, value := range strings.Split(textContent(node), separator) {
		value = strings.TrimSpace(value)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func parseRatings(document *nethtml.Node) []Rating {
	types := []struct {
		class  string
		source string
	}{
		{class: "frate-kp", source: "kinopoisk"},
		{class: "frate-imdb", source: "imdb"},
	}
	ratings := make([]Rating, 0, len(types))
	for _, ratingType := range types {
		node := findElement(document, func(node *nethtml.Node) bool {
			return hasClass(node, ratingType.class)
		})
		value, err := strconv.ParseFloat(strings.TrimSpace(textContent(node)), 64)
		if err == nil {
			ratings = append(ratings, Rating{Source: ratingType.source, Value: value})
		}
	}
	return ratings
}
