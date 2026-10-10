package provider

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The CDN binds signed media URLs to the User-Agent used to request the embed
// page. Generate them for the Chromecast receiver, which downloads the HLS
// playlists and segments directly from the CDN.
const defaultUserAgent = "Mozilla/5.0 (X11; Linux armv7l) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/90.0.4430.225 Safari/537.36 CrKey/1.56.500000"

var ErrInvalidMovieID = errors.New("invalid movie ID")

type Config struct {
	BaseURL         string
	RefreshInterval time.Duration
	HTTPTimeout     time.Duration
	UserAgent       string
}

type session struct {
	client      *http.Client
	generation  uint64
	mu          sync.RWMutex
	cookies     map[string]string
	cookieOrder []string
}

type Client struct {
	baseURL         *url.URL
	domainPrefix    string
	refreshInterval time.Duration
	httpTimeout     time.Duration
	userAgent       string

	mu             sync.RWMutex
	session        *session
	nextGeneration uint64
	refreshMu      sync.Mutex
}

func NewClient(config Config) (*Client, error) {
	baseURL, err := url.Parse(config.BaseURL)
	if err != nil || baseURL.Scheme == "" || baseURL.Hostname() == "" {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/")
	baseURL.RawQuery = ""
	baseURL.Fragment = ""

	if config.RefreshInterval <= 0 {
		config.RefreshInterval = 5 * time.Minute
	}
	if config.HTTPTimeout <= 0 {
		config.HTTPTimeout = 45 * time.Second
	}
	if config.UserAgent == "" {
		config.UserAgent = defaultUserAgent
	}

	digest := sha1.Sum([]byte(strings.ToLower(baseURL.Hostname())))
	return &Client{
		baseURL:         baseURL,
		domainPrefix:    hex.EncodeToString(digest[:])[:8],
		refreshInterval: config.RefreshInterval,
		httpTimeout:     config.HTTPTimeout,
		userAgent:       config.UserAgent,
	}, nil
}

func (c *Client) DomainPrefix() string {
	return c.domainPrefix
}

func (c *Client) ExternalMovieID(siteMovieID string) string {
	return c.domainPrefix + siteMovieID
}

func (c *Client) Start(ctx context.Context) error {
	if err := c.Refresh(ctx); err != nil {
		return err
	}
	go c.refreshLoop(ctx)
	return nil
}

func (c *Client) refreshLoop(ctx context.Context) {
	ticker := time.NewTicker(c.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.Refresh(ctx)
		}
	}
}

func (c *Client) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	return c.refreshLocked(ctx)
}

func (c *Client) refreshIfStale(ctx context.Context, generation uint64) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	current, err := c.currentSession()
	if err == nil && current.generation != generation {
		return nil
	}
	return c.refreshLocked(ctx)
}

func (c *Client) refreshLocked(ctx context.Context) error {
	candidate := &session{
		cookies: make(map[string]string),
	}
	candidate.client = &http.Client{
		Timeout: c.httpTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if request.Response != nil && request.Response.Request.URL.Hostname() == c.baseURL.Hostname() {
				candidate.absorbSetCookies(request.Response.Header.Values("Set-Cookie"))
			}
			if request.URL.Hostname() == c.baseURL.Hostname() {
				request.Header.Set("Cookie", candidate.cookieHeader())
			} else {
				request.Header.Del("Cookie")
			}
			return nil
		},
	}

	challengeURL := c.baseURL.ResolveReference(&url.URL{Path: "/"})
	for attempt := 0; attempt < 4; attempt++ {
		body, _, err := c.get(ctx, candidate, challengeURL.String(), "")
		if err != nil {
			return fmt.Errorf("refresh anti-bot session: %w", err)
		}
		if !isChallenge(body) {
			c.mu.Lock()
			c.nextGeneration++
			candidate.generation = c.nextGeneration
			c.session = candidate
			c.mu.Unlock()
			return nil
		}

		parameters := candidate.cookieValue("__js_p_")
		parts := strings.Split(parameters, ",")
		if len(parts) < 2 {
			return errors.New("anti-bot response has no valid __js_p_ cookie")
		}
		code, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return fmt.Errorf("parse anti-bot code: %w", err)
		}
		hash, err := challengeHash(ctx, code)
		if err != nil {
			return err
		}

		candidate.setCookie("__jhash_", strconv.Itoa(hash))
		candidate.setCookie("__jua_", encodeURIComponent(c.userAgent))

		// The browser challenge waits one second before setting the cookies and
		// navigating. Preserve that timing; the server rejects instant replays.
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("anti-bot challenge was not accepted after 4 attempts")
}

func (c *Client) currentSession() (*session, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.session == nil {
		return nil, errors.New("anti-bot session is not initialized")
	}
	return c.session, nil
}

func (c *Client) get(ctx context.Context, current *session, target, referer string) ([]byte, *url.URL, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("User-Agent", c.userAgent)
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/vnd.apple.mpegurl,*/*;q=0.8")
	request.Header.Set("Accept-Language", "en-US,en;q=0.8")
	if request.URL.Hostname() == c.baseURL.Hostname() {
		if cookies := current.cookieHeader(); cookies != "" {
			// Set the header directly: net/http.Cookie.String quotes commas, while
			// provider's __js_p_ value must be sent byte-for-byte without quotes.
			request.Header.Set("Cookie", cookies)
		}
	}
	if referer != "" {
		request.Header.Set("Referer", referer)
	}

	response, err := current.client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if response.Request.URL.Hostname() == c.baseURL.Hostname() {
		current.absorbSetCookies(response.Header.Values("Set-Cookie"))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.Request.URL, fmt.Errorf("GET %s returned %s", target, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, response.Request.URL, err
	}
	return body, response.Request.URL, nil
}

func (c *Client) postForm(
	ctx context.Context,
	current *session,
	target string,
	form url.Values,
	referer string,
) ([]byte, *url.URL, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("User-Agent", c.userAgent)
	request.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	request.Header.Set("Accept-Language", "en-US,en;q=0.8")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if request.URL.Hostname() == c.baseURL.Hostname() {
		if cookies := current.cookieHeader(); cookies != "" {
			request.Header.Set("Cookie", cookies)
		}
	}
	if referer != "" {
		request.Header.Set("Referer", referer)
	}

	response, err := current.client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if response.Request.URL.Hostname() == c.baseURL.Hostname() {
		current.absorbSetCookies(response.Header.Values("Set-Cookie"))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.Request.URL, fmt.Errorf("POST %s returned %s", target, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, response.Request.URL, err
	}
	return body, response.Request.URL, nil
}

func (s *session) setCookie(name, value string) {
	s.mu.Lock()
	if _, exists := s.cookies[name]; !exists {
		s.cookieOrder = append(s.cookieOrder, name)
	}
	s.cookies[name] = value
	s.mu.Unlock()
}

func (s *session) cookieValue(name string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cookies[name]
}

func (s *session) cookieHeader() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	parts := make([]string, 0, len(s.cookieOrder))
	for _, name := range s.cookieOrder {
		parts = append(parts, name+"="+s.cookies[name])
	}
	return strings.Join(parts, "; ")
}

func (s *session) absorbSetCookies(headers []string) {
	for _, header := range headers {
		pair := strings.SplitN(header, ";", 2)[0]
		name, value, ok := strings.Cut(pair, "=")
		if ok && strings.TrimSpace(name) != "" {
			s.setCookie(strings.TrimSpace(name), value)
		}
	}
}

func isChallenge(body []byte) bool {
	text := string(body)
	return strings.Contains(text, "get_jhash") && strings.Contains(text, "__js_p_")
}

func challengeHash(ctx context.Context, code int64) (int, error) {
	var x int64 = 123456789
	result := 0
	for i := int64(0); i < 1677696; i++ {
		if i&0xffff == 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			default:
			}
		}
		first := int32(x + code)
		second := int32(x + x%3 + x%17 + code)
		x = int64(first^second^int32(i)) % 16776960
		if x%117 == 0 {
			result = (result + 1) % 1111
		}
	}
	return result, nil
}

func encodeURIComponent(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var builder strings.Builder
	for i := 0; i < len(value); i++ {
		character := value[i]
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("-_.!~*'()", rune(character)) {
			builder.WriteByte(character)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(hexDigits[character>>4])
		builder.WriteByte(hexDigits[character&0x0f])
	}
	return builder.String()
}
