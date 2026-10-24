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
//
// Only the path and query of rawURL are used: the image is always fetched
// from the current provider host. The provider rotates subdomains, so saved
// poster links go stale, and no other host can be reached through here.
func (c *Client) Poster(ctx context.Context, rawURL string) ([]byte, string, error) {
	source, err := url.Parse(rawURL)
	if err != nil || (source.Scheme != "http" && source.Scheme != "https") ||
		strings.Trim(source.Path, "/") == "" {
		return nil, "", fmt.Errorf("%w: %q", ErrInvalidPosterURL, rawURL)
	}
	target := providerOrigin(c.providerURL())
	target.Path, target.RawPath, target.RawQuery = source.Path, source.RawPath, source.RawQuery

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
