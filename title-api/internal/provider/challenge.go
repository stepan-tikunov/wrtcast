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
	"sync/atomic"
	"time"

	"golang.org/x/net/publicsuffix"
)

// The CDN binds signed media URLs to the User-Agent used to request the embed
// page. Generate them for the Chromecast receiver, which downloads the HLS
// playlists and segments directly from the CDN.
const defaultUserAgent = "Mozilla/5.0 (X11; Linux armv7l) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/90.0.4430.225 Safari/537.36 CrKey/1.56.500000"

var ErrInvalidTitleID = errors.New("invalid movie ID")

type Config struct {
	BaseURL          string
	MediaBaseURL     string
	RefreshInterval  time.Duration
	HTTPTimeout      time.Duration
	UserAgent        string
	URLStore         ProviderURLStore
	FingerprintStore PlayerFingerprintStore
}

type session struct {
	client      *http.Client
	generation  uint64
	mu          sync.RWMutex
	cookies     map[string]map[string]string
	cookieOrder map[string][]string
}

type Client struct {
	bootstrapURL    *url.URL
	baseURL         atomic.Pointer[url.URL]
	domainPrefix    string
	refreshInterval time.Duration
	httpTimeout     time.Duration
	userAgent       string
	mediaBaseURL    string
	masters         *masterStore
	urlStore        ProviderURLStore

	fingerprintStore PlayerFingerprintStore
	fingerprintMu    sync.Mutex
	fingerprint      string

	// The Chrome the alternative player sees (see browser), the quality
	// its pages start with (the last one played, as the page stores it),
	// and its playback sockets (see playbackSession), until Close.
	browser         *browser
	pageTiming      pageTiming
	qualityMu       sync.Mutex
	quality         string
	sessionTiming   sessionTiming
	telemetryTiming telemetryTiming
	sessionsCtx     context.Context
	stopSessions    context.CancelFunc
	sessionsMu      sync.Mutex
	sessionsClosed  bool
	sessions        sync.WaitGroup

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
	if config.MediaBaseURL == "" {
		config.MediaBaseURL = "http://localhost:50052"
	}
	mediaBaseURL, err := url.Parse(config.MediaBaseURL)
	if err != nil || mediaBaseURL.Scheme == "" || mediaBaseURL.Host == "" {
		return nil, fmt.Errorf("parse media base URL: %w", err)
	}

	// The provider moves between subdomains; its IDs stay those of the site.
	digest := sha1.Sum([]byte(registrableDomain(baseURL)))
	client := &Client{
		bootstrapURL:     baseURL,
		domainPrefix:     hex.EncodeToString(digest[:])[:8],
		refreshInterval:  config.RefreshInterval,
		httpTimeout:      config.HTTPTimeout,
		userAgent:        config.UserAgent,
		mediaBaseURL:     strings.TrimRight(mediaBaseURL.String(), "/"),
		masters:          newMasterStore(6*time.Hour, 4096),
		urlStore:         config.URLStore,
		fingerprintStore: config.FingerprintStore,
		browser:          newBrowser(false),
		pageTiming:       defaultPageTiming,
		sessionTiming:    defaultSessionTiming,
		telemetryTiming:  defaultTelemetryTiming,
	}
	client.sessionsCtx, client.stopSessions = context.WithCancel(context.Background())
	client.baseURL.Store(baseURL)
	return client, nil
}

// preferQuality remembers the quality played, which the next page starts
// with, as the player keeps it in the page's storage.
func (c *Client) preferQuality(quality string) {
	c.qualityMu.Lock()
	c.quality = quality
	c.qualityMu.Unlock()
}

func (c *Client) preferredQuality() string {
	c.qualityMu.Lock()
	defer c.qualityMu.Unlock()
	return c.quality
}

// Close closes the playback sockets, each with a close frame, and waits
// for them.
func (c *Client) Close() {
	c.sessionsMu.Lock()
	c.sessionsClosed = true
	c.sessionsMu.Unlock()
	c.stopSessions()
	c.sessions.Wait()
}

func (c *Client) DomainPrefix() string {
	return c.domainPrefix
}

func (c *Client) ExternalTitleID(siteMovieID string) string {
	return c.domainPrefix + siteMovieID
}

func (c *Client) ProviderURL() string {
	return c.providerURL().String()
}

func (c *Client) Start(ctx context.Context) error {
	if c.urlStore != nil {
		stored, err := c.urlStore.Load(ctx)
		if err != nil {
			return fmt.Errorf("load remembered provider URL: %w", err)
		}
		if stored != "" {
			remembered, parseErr := parseProviderURL(stored)
			if parseErr == nil && sameProvider(c.bootstrapURL, remembered) {
				c.baseURL.Store(remembered)
			}
		}
	}
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
	active := c.providerURL()
	candidates := []*url.URL{active}
	if active.String() != c.bootstrapURL.String() {
		candidates = append(candidates, c.bootstrapURL)
	}

	var refreshErrors []error
	for _, candidateURL := range candidates {
		candidate, resolvedURL, err := c.refreshFrom(ctx, candidateURL)
		if err != nil {
			refreshErrors = append(refreshErrors, fmt.Errorf("%s: %w", candidateURL, err))
			continue
		}

		c.mu.Lock()
		c.nextGeneration++
		candidate.generation = c.nextGeneration
		c.session = candidate
		c.mu.Unlock()

		c.baseURL.Store(resolvedURL)
		if c.urlStore != nil {
			if err := c.urlStore.Save(ctx, resolvedURL.String()); err != nil {
				return fmt.Errorf("remember provider URL: %w", err)
			}
		}
		return nil
	}
	return errors.Join(refreshErrors...)
}

func (c *Client) refreshFrom(ctx context.Context, initialURL *url.URL) (*session, *url.URL, error) {
	candidate := &session{
		cookies:     make(map[string]map[string]string),
		cookieOrder: make(map[string][]string),
	}
	candidate.client = &http.Client{
		Timeout: c.httpTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if request.Response != nil {
				candidate.absorbSetCookies(request.Response.Request.URL.Host, request.Response.Header.Values("Set-Cookie"))
			}
			if cookies := candidate.cookieHeader(request.URL.Host); cookies != "" {
				request.Header.Set("Cookie", cookies)
			} else {
				request.Header.Del("Cookie")
			}
			return nil
		},
	}

	challengeURL := initialURL.ResolveReference(&url.URL{Path: "/"})
	for attempt := 0; attempt < 4; attempt++ {
		body, finalURL, err := c.get(ctx, candidate, challengeURL.String(), "")
		if err != nil {
			return nil, nil, fmt.Errorf("refresh anti-bot session: %w", err)
		}
		resolvedURL := providerOrigin(finalURL)
		if !sameProvider(c.bootstrapURL, resolvedURL) {
			return nil, nil, fmt.Errorf("provider redirected outside %s to %s", c.bootstrapURL.Hostname(), resolvedURL)
		}
		if !isChallenge(body) {
			return candidate, resolvedURL, nil
		}
		challengeURL = resolvedURL.ResolveReference(&url.URL{Path: "/"})

		parameters := candidate.cookieValue(finalURL.Host, "__js_p_")
		parts := strings.Split(parameters, ",")
		if len(parts) < 2 {
			return nil, nil, errors.New("anti-bot response has no valid __js_p_ cookie")
		}
		code, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("parse anti-bot code: %w", err)
		}
		hash, err := challengeHash(ctx, code)
		if err != nil {
			return nil, nil, err
		}

		candidate.setCookie(finalURL.Host, "__jhash_", strconv.Itoa(hash))
		candidate.setCookie(finalURL.Host, "__jua_", encodeURIComponent(c.userAgent))

		// The browser challenge waits one second before setting the cookies and
		// navigating. Preserve that timing; the server rejects instant replays.
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, nil, errors.New("anti-bot challenge was not accepted after 4 attempts")
}

func (c *Client) providerURL() *url.URL {
	return c.baseURL.Load()
}

func providerOrigin(source *url.URL) *url.URL {
	return &url.URL{Scheme: source.Scheme, Host: source.Host}
}

func parseProviderURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return nil, fmt.Errorf("parse provider URL %q: %w", value, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("provider URL %q has unsupported scheme", value)
	}
	return providerOrigin(parsed), nil
}

func sameProvider(first, second *url.URL) bool {
	if first == nil || second == nil {
		return false
	}
	firstDomain, firstErr := publicsuffix.EffectiveTLDPlusOne(first.Hostname())
	secondDomain, secondErr := publicsuffix.EffectiveTLDPlusOne(second.Hostname())
	if firstErr != nil || secondErr != nil {
		return strings.EqualFold(first.Hostname(), second.Hostname())
	}
	return strings.EqualFold(firstDomain, secondDomain)
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
	if cookies := current.cookieHeader(request.URL.Host); cookies != "" {
		// Set the header directly: net/http.Cookie.String quotes commas, while
		// provider's __js_p_ value must be sent byte-for-byte without quotes.
		request.Header.Set("Cookie", cookies)
	}
	if referer != "" {
		request.Header.Set("Referer", referer)
	}

	response, err := current.client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	current.absorbSetCookies(response.Request.URL.Host, response.Header.Values("Set-Cookie"))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Keep the start of the error page: it tells why the provider refused.
		body, _ := io.ReadAll(io.LimitReader(response.Body, 256<<10))
		return nil, response.Request.URL, &statusError{
			target: target,
			status: response.Status,
			code:   response.StatusCode,
			body:   body,
		}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, response.Request.URL, err
	}
	return body, response.Request.URL, nil
}

type statusError struct {
	target string
	status string
	code   int
	body   []byte
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GET %s returned %s", e.target, e.status)
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
	if cookies := current.cookieHeader(request.URL.Host); cookies != "" {
		request.Header.Set("Cookie", cookies)
	}
	if referer != "" {
		request.Header.Set("Referer", referer)
	}

	response, err := current.client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	current.absorbSetCookies(response.Request.URL.Host, response.Header.Values("Set-Cookie"))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.Request.URL, fmt.Errorf("POST %s returned %s", target, response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return nil, response.Request.URL, err
	}
	return body, response.Request.URL, nil
}

func (s *session) setCookie(host, name, value string) {
	s.mu.Lock()
	if s.cookies[host] == nil {
		s.cookies[host] = make(map[string]string)
	}
	if _, exists := s.cookies[host][name]; !exists {
		s.cookieOrder[host] = append(s.cookieOrder[host], name)
	}
	s.cookies[host][name] = value
	s.mu.Unlock()
}

func (s *session) cookieValue(host, name string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cookies[host][name]
}

func (s *session) cookieHeader(host string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	parts := make([]string, 0, len(s.cookieOrder[host]))
	for _, name := range s.cookieOrder[host] {
		parts = append(parts, name+"="+s.cookies[host][name])
	}
	return strings.Join(parts, "; ")
}

func (s *session) absorbSetCookies(host string, headers []string) {
	for _, header := range headers {
		pair := strings.SplitN(header, ";", 2)[0]
		name, value, ok := strings.Cut(pair, "=")
		if ok && strings.TrimSpace(name) != "" {
			s.setCookie(host, strings.TrimSpace(name), value)
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
	const (
		upperHexDigits = "0123456789ABCDEF"
		lowerHexDigits = "0123456789abcdef"
	)
	var builder strings.Builder
	for i := 0; i < len(value); i++ {
		character := value[i]
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("-_.~", rune(character)) {
			builder.WriteByte(character)
			continue
		}
		hexDigits := upperHexDigits
		if strings.ContainsRune("!'()*", rune(character)) {
			hexDigits = lowerHexDigits
		}
		builder.WriteByte('%')
		builder.WriteByte(hexDigits[character>>4])
		builder.WriteByte(hexDigits[character&0x0f])
	}
	return builder.String()
}
