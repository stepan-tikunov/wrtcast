package music

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// client asks the music service (music/ in the repository).
type client struct {
	url  string
	http *http.Client
}

func newClient(url string) *client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &client{url: url, http: &http.Client{Transport: transport, Timeout: time.Minute}}
}

// limitedError is the music service declining to ask Shazam: over its
// limit or after Shazam's 429 (for RetryAfter), or with Shazam off.
type limitedError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *limitedError) Error() string {
	return fmt.Sprintf("music service: Shazam %s (retry after %s)", e.Reason, e.RetryAfter)
}

func (c *client) post(ctx context.Context, path string, audio []byte, result any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+path, bytes.NewReader(audio))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("music service: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("music service: %w", err)
	}
	if response.StatusCode == http.StatusServiceUnavailable {
		var limited struct {
			Error      string   `json:"error"`
			RetryAfter *float64 `json:"retry_after"`
		}
		if json.Unmarshal(body, &limited) == nil && limited.Error != "" {
			after := 10 * time.Minute
			if limited.RetryAfter != nil {
				after = time.Duration(*limited.RetryAfter * float64(time.Second))
			}
			return &limitedError{Reason: limited.Error, RetryAfter: after}
		}
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("music service: %s: %s", response.Status, bytes.TrimSpace(body))
	}
	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("music service: %w", err)
	}
	return nil
}

// classify is YAMNet's frames of audio, their T from its start, with
// their MFCCs.
func (c *client) classify(ctx context.Context, audio []byte) ([]frame, error) {
	var result struct {
		Frames   [][4]float64 `json:"frames"`
		Features [][]float32  `json:"features"`
	}
	if err := c.post(ctx, "/v1/classify", audio, &result); err != nil {
		return nil, err
	}
	frames := make([]frame, len(result.Frames))
	for i, f := range result.Frames {
		frames[i] = frame{T: f[0], Music: f[1], Speech: f[2], Song: f[3]}
		if i < len(result.Features) && len(result.Features[i]) == mfccs {
			frames[i].MFCC = result.Features[i]
		}
	}
	return frames, nil
}

// match is a track Shazam told, as the music service answers it.
type match struct {
	Key      string  `json:"key"`
	Title    string  `json:"title"`
	Artist   string  `json:"artist"`
	Cover    string  `json:"cover,omitempty"`
	URL      string  `json:"url,omitempty"`
	ISRC     string  `json:"isrc,omitempty"`
	Apple    string  `json:"apple,omitempty"`
	AppleURL string  `json:"apple_url,omitempty"`
	Spotify  string  `json:"spotify,omitempty"`
	Offset   float64 `json:"offset"`
	Duration float64 `json:"duration,omitempty"`
}

// recognize is the track Shazam tells the audio is, nil if none.
func (c *client) recognize(ctx context.Context, audio []byte) (*match, error) {
	var result struct {
		Match *match `json:"match"`
	}
	if err := c.post(ctx, "/v1/recognize", audio, &result); err != nil {
		return nil, err
	}
	if result.Match != nil && result.Match.Key == "" {
		return nil, errors.New("music service: a match without a key")
	}
	return result.Match, nil
}
