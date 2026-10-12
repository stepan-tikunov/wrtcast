package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

var ErrInvalidPosterURL = errors.New("invalid poster URL")

// Poster downloads a poster image from the provider site. The site serves
// images behind the same anti-bot challenge as its pages, so clients without
// the session cookies receive the challenge HTML instead of the image.
func (c *Client) Poster(ctx context.Context, rawURL string) ([]byte, string, error) {
	baseURL := c.providerURL()
	target, err := url.Parse(rawURL)
	if err != nil || target.Scheme != baseURL.Scheme ||
		!strings.EqualFold(target.Hostname(), baseURL.Hostname()) {
		return nil, "", fmt.Errorf("%w: must be on %s", ErrInvalidPosterURL, baseURL.Host)
	}

	current, err := c.currentSession()
	if err != nil {
		return nil, "", err
	}
	body, contentType, err := c.posterImage(ctx, current, target.String())
	if errors.Is(err, errAntiBotChallenge) {
		if refreshErr := c.refreshIfStale(ctx, current.generation); refreshErr != nil {
			return nil, "", fmt.Errorf("refresh rejected anti-bot session: %w", refreshErr)
		}
		current, err = c.currentSession()
		if err != nil {
			return nil, "", err
		}
		body, contentType, err = c.posterImage(ctx, current, target.String())
	}
	return body, contentType, err
}

func (c *Client) posterImage(ctx context.Context, current *session, target string) ([]byte, string, error) {
	body, _, err := c.get(ctx, current, target, c.providerURL().String()+"/")
	if err != nil {
		return nil, "", fmt.Errorf("fetch poster: %w", err)
	}
	if isChallenge(body) {
		return nil, "", errAntiBotChallenge
	}
	contentType := http.DetectContentType(body)
	if !strings.HasPrefix(contentType, "image/") {
		return nil, "", fmt.Errorf("fetch poster: unexpected content type %s", contentType)
	}
	return body, contentType, nil
}
