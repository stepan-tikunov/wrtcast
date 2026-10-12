package provider

import (
	"context"
	"errors"
	"fmt"
	stdhtml "html"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	nethtml "golang.org/x/net/html"
)

var ErrMediaTypeUnknown = errors.New("media type could not be determined")

var titleDurationPattern = regexp.MustCompile(`(?s)["']?duration["']?\s*:\s*["']?([0-9]+(?:\.[0-9]+)?)["']?`)

type TitleMetadata struct {
	TitleID         string
	Title           string
	OriginalTitle   string
	Year            string
	Description     string
	PosterURL       string
	MediaType       MediaType
	Quality         string
	Countries       []string
	Genres          []string
	Directors       []string
	Actors          []string
	Ratings         []Rating
	DurationSeconds uint64
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

	metadata, err := parseTitleMetadata(movieHTML, c.providerURL())
	if err != nil {
		return TitleMetadata{}, fmt.Errorf("parse title metadata: %w", err)
	}
	metadata.TitleID = externalMovieID
	metadata.MediaType, metadata.DurationSeconds, err = c.mediaTypeFromEmbeds(ctx, current, embedURLs)
	if err != nil {
		// The primary players may be down while the alternative ones on the
		// same page still work; their catalog tells movies from series.
		mediaType, alternativeErr := c.mediaTypeFromAlternativePlayers(ctx, movieHTML)
		if alternativeErr != nil {
			return TitleMetadata{}, errors.Join(err, alternativeErr)
		}
		metadata.MediaType = mediaType
	}
	return metadata, nil
}

func (c *Client) mediaTypeFromAlternativePlayers(ctx context.Context, movieHTML []byte) (MediaType, error) {
	loadErrors := make([]error, 0)
	for _, playerURL := range alternativePlayerURLs(movieHTML, c.providerURL().Scheme) {
		player, err := c.loadAlternativePlayer(ctx, playerURL)
		if err != nil {
			loadErrors = append(loadErrors, fmt.Errorf("%s: %w", playerURL, err))
			continue
		}
		if player.catalog.isSeries() {
			return MediaTypeSeries, nil
		}
		return MediaTypeMovie, nil
	}
	if len(loadErrors) > 0 {
		return MediaTypeUnspecified, fmt.Errorf("inspect alternative players: %w", errors.Join(loadErrors...))
	}
	return MediaTypeUnspecified, ErrMediaTypeUnknown
}

func (c *Client) mediaTypeFromEmbeds(
	ctx context.Context,
	current *session,
	embedURLs []string,
) (MediaType, uint64, error) {
	hasMovie := false
	var durationSeconds uint64
	fetchErrors := make([]error, 0)
	for _, embedURL := range embedURLs {
		embedHTML, _, err := c.get(ctx, current, embedURL, c.providerURL().String()+"/")
		if err != nil {
			fetchErrors = append(fetchErrors, fmt.Errorf("%s: %w", embedURL, err))
			continue
		}
		if containsSeriesPlaylist(embedHTML) {
			return MediaTypeSeries, 0, nil
		}
		hlsMatch := hlsPattern.FindSubmatch(embedHTML)
		if len(hlsMatch) == 2 {
			hasMovie = true
			duration := playerDuration(embedHTML)
			if duration == 0 {
				duration, _ = c.durationFromEmbedHLS(ctx, current, embedHTML, hlsMatch[1])
			}
			if duration > durationSeconds {
				durationSeconds = duration
			}
		}
	}
	if hasMovie {
		return MediaTypeMovie, durationSeconds, nil
	}
	if len(fetchErrors) > 0 {
		return MediaTypeUnspecified, 0, fmt.Errorf("inspect player embeds: %w", errors.Join(fetchErrors...))
	}
	return MediaTypeUnspecified, 0, ErrMediaTypeUnknown
}

func playerDuration(embedHTML []byte) uint64 {
	match := titleDurationPattern.FindSubmatch(embedHTML)
	if len(match) != 2 {
		return 0
	}
	duration, err := strconv.ParseFloat(string(match[1]), 64)
	if err != nil || duration <= 0 {
		return 0
	}
	return uint64(math.Round(duration))
}

func (c *Client) durationFromEmbedHLS(
	ctx context.Context,
	current *session,
	embedHTML []byte,
	escapedHLSURL []byte,
) (uint64, error) {
	hlsURL, err := decodeJSString(string(escapedHLSURL))
	if err != nil {
		return 0, fmt.Errorf("decode duration HLS URL: %w", err)
	}
	marker, err := parseDynamicMarker(embedHTML)
	if err != nil {
		return 0, err
	}
	return c.hlsDuration(
		ctx,
		current,
		appendDynamicMarker(stdhtml.UnescapeString(hlsURL), marker),
		0,
	)
}

func (c *Client) hlsDuration(
	ctx context.Context,
	current *session,
	playlistURL string,
	depth int,
) (uint64, error) {
	if depth > 2 {
		return 0, errors.New("HLS duration playlist nesting is too deep")
	}
	body, _, err := c.get(ctx, current, playlistURL, "")
	if err != nil {
		return 0, fmt.Errorf("fetch HLS duration playlist: %w", err)
	}
	if duration, ok := mediaPlaylistDuration(string(body)); ok {
		return duration, nil
	}
	variantURL, ok := firstVariantURL(playlistURL, string(body))
	if !ok {
		return 0, errors.New("HLS duration playlist has no media segments or variants")
	}
	return c.hlsDuration(ctx, current, variantURL, depth+1)
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
