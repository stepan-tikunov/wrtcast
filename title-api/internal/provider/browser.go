package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/publicsuffix"
)

// browser is the network stack of the Chrome title-api plays for the
// alternative player (see chrome.go): one for all its pages, as a browser
// has one, with a connection pool per host (HTTP/2 where ALPN offers it, at
// most six HTTP/1.1 connections otherwise), one TLS session cache, one
// cookie store, one HTTP cache and one CORS preflight cache. Requests go
// through the proxy of HTTPS_PROXY (or HTTP_PROXY), as net/http's would;
// without QUIC, which Chrome does not use through a proxy either.
type browser struct {
	insecure bool // tests: self-signed servers
	proxy    func(*url.URL) (*url.URL, error)

	mu      sync.Mutex
	clients map[string]tlsclient.HttpClient // by proxy URL, "" for none

	cookies    *browserCookies
	cache      *httpCache
	preflights *preflightCache
}

// chromeProfile is Chrome 152's: the ClientHello (with a pre-shared key
// once a session can be resumed, and the extensions in a new order on
// every connection, as Chrome shuffles them), the HTTP/2 SETTINGS, the
// connection's WINDOW_UPDATE and the pseudo-header order m,a,s,p. fhttp
// gives HEADERS frames the exclusive weight-256 priority Chrome gives its
// most urgent requests.
var chromeProfile = profiles.Chrome_152_PSK

func newBrowser(insecure bool) *browser {
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	return &browser{
		insecure:   insecure,
		proxy:      httpproxy.FromEnvironment().ProxyFunc(),
		clients:    map[string]tlsclient.HttpClient{},
		cookies:    &browserCookies{jar: jar},
		cache:      &httpCache{entries: map[string]*cacheEntry{}},
		preflights: &preflightCache{entries: map[string]preflightEntry{}},
	}
}

// client is the pool requests to target go through: through the proxy
// the environment names for it, if any.
func (b *browser) client(target *url.URL) (tlsclient.HttpClient, error) {
	proxyURL := ""
	if b.proxy != nil {
		lookup := *target
		switch lookup.Scheme {
		case "ws":
			lookup.Scheme = "http"
		case "wss":
			lookup.Scheme = "https"
		}
		proxy, err := b.proxy(&lookup)
		if err != nil {
			return nil, err
		}
		if proxy != nil {
			proxyURL = proxy.String()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if client := b.clients[proxyURL]; client != nil {
		return client, nil
	}
	idle := 5 * time.Minute
	options := []tlsclient.HttpClientOption{
		tlsclient.WithClientProfile(chromeProfile),
		tlsclient.WithRandomTLSExtensionOrder(),
		// No deadline for a request as a whole: segments stream for as
		// long as they take, and the caller's context bounds the rest.
		tlsclient.WithTimeoutSeconds(0),
		tlsclient.WithNotFollowRedirects(),
		tlsclient.WithDisableHttp3(),
		tlsclient.WithTransportOptions(&tlsclient.TransportOptions{
			MaxIdleConnsPerHost: 6, MaxConnsPerHost: 6, IdleConnTimeout: &idle,
		}),
	}
	if proxyURL != "" {
		options = append(options, tlsclient.WithProxyUrl(proxyURL))
	}
	if b.insecure {
		options = append(options, tlsclient.WithInsecureSkipVerify())
	}
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, fmt.Errorf("browser client: %w", err)
	}
	b.clients[proxyURL] = client
	return client, nil
}

// browserRequest is a request of a page.
type browserRequest struct {
	kind   fetchKind
	method string
	url    string
	// The document it comes from, whose origin is the initiator, and the
	// Referer: the player page's policy (no-referrer-when-downgrade) sends
	// its whole URL, the provider's page (strict-origin-when-cross-origin)
	// its origin.
	initiator *url.URL
	referer   string
	// What the page sets itself, in the order it does: Content-Type,
	// X-Requested-With, the CDN's Accepts-Controls and the like.
	header [][2]string
	body   []byte
	// A cross-origin XMLHttpRequest with withCredentials.
	withCredentials bool
}

// errCORS is a request Chrome would not let through: its preflight or
// its redirect failed the CORS checks. The page sees a network error.
var errCORS = errors.New("CORS check failed")

// do sends a request as Chrome would: the CORS preflight first when it
// needs one (see preflightCache), a cached copy of a subresource while it
// is fresh and a revalidation once it is not (see httpCache), cookies
// stored and sent where its credentials mode and SameSite allow, and
// redirects followed. The response is the final one, its body streaming.
func (b *browser) do(ctx context.Context, request browserRequest) (*http.Response, error) {
	if request.method == "" {
		request.method = http.MethodGet
	}
	target, err := url.Parse(request.url)
	if err != nil || target.Host == "" {
		return nil, fmt.Errorf("invalid URL %q", request.url)
	}
	site := fetchSite(request.initiator, target)
	tainted := false
	current := request.initiator
	for redirects := 0; ; redirects++ {
		response, err := b.fetch(ctx, request, target, site, tainted)
		if err != nil {
			return nil, err
		}
		location := response.Header.Get("Location")
		if !redirectStatus(response.StatusCode) || location == "" {
			return response, nil
		}
		next, err := target.Parse(location)
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		if redirects >= 20 {
			return nil, errors.New("too many redirects")
		}
		// A redirect from one foreign origin to another makes the Origin
		// of the rest "null".
		if !sameOrigin(target, next) && !sameOrigin(request.initiator, target) && current != nil {
			tainted = true
		}
		if site != "cross-site" {
			if nextSite := fetchSite(request.initiator, next); nextSite == "cross-site" || site == "same-origin" {
				site = nextSite
			}
		}
		if response.StatusCode == http.StatusSeeOther ||
			(response.StatusCode == http.StatusMovedPermanently || response.StatusCode == http.StatusFound) && request.method == http.MethodPost {
			request.method, request.body = http.MethodGet, nil
			request.header = withoutHeader(request.header, "Content-Type")
		}
		current, target = target, next
	}
}

func redirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

func withoutHeader(header [][2]string, name string) [][2]string {
	var kept [][2]string
	for _, field := range header {
		if !strings.EqualFold(field[0], name) {
			kept = append(kept, field)
		}
	}
	return kept
}

// fetch is one hop of do.
func (b *browser) fetch(ctx context.Context, request browserRequest, target *url.URL, site string, tainted bool) (*http.Response, error) {
	spec := fetchSpecs[request.kind]
	crossOrigin := !sameOrigin(request.initiator, target)
	origin := serializedOrigin(request.initiator)
	if tainted {
		origin = "null"
	}
	if spec.mode == "cors" && (crossOrigin || tainted) {
		if err := b.preflight(ctx, request, target, origin, site); err != nil {
			return nil, err
		}
	}
	credentials := spec.credentials == "include" || spec.credentials == "same-origin" && !crossOrigin || request.withCredentials
	cacheable := request.method == http.MethodGet && (request.kind == fetchStyle || request.kind == fetchScript || request.kind == fetchDeferredScript)
	var cached *cacheEntry
	if cacheable {
		var fresh bool
		if cached, fresh = b.cache.lookup(target.String(), time.Now()); fresh {
			return cached.response(target), nil
		}
	}

	header := newChromeHeader()
	header.set("Connection", "keep-alive")
	var page []string
	hints := trustworthy(target)
	contentType, accept := "", spec.accept
	for _, field := range request.header {
		switch strings.ToLower(field[0]) {
		case "content-type":
			contentType = field[1]
		case "accept":
			accept = field[1]
		default:
			header.set(field[0], field[1])
			page = append(page, strings.ToLower(field[0]))
		}
	}
	order := subresourceOrder
	if request.kind == fetchNavigate {
		order = navigationOrder
		if hints {
			header.set("sec-ch-ua", chromeBrandList)
			header.set("sec-ch-ua-mobile", "?0")
			header.set("sec-ch-ua-platform", chromePlatform)
		}
		header.set("Upgrade-Insecure-Requests", "1")
	} else if hints {
		header.set("sec-ch-ua-platform", chromePlatform)
		header.set("sec-ch-ua", chromeBrandList)
		header.set("sec-ch-ua-mobile", "?0")
	}
	header.set("User-Agent", chromeUserAgent)
	header.set("Accept", accept)
	if contentType != "" {
		header.set("Content-Type", contentType)
	}
	// Origin goes with every CORS request to another origin and with every
	// request but GET and HEAD.
	if request.method != http.MethodGet && request.method != http.MethodHead || spec.mode == "cors" && (crossOrigin || tainted) {
		header.set("Origin", origin)
	}
	header.set("Sec-Fetch-Site", site)
	header.set("Sec-Fetch-Mode", spec.mode)
	header.set("Sec-Fetch-Dest", spec.dest)
	if request.referer != "" {
		header.set("Referer", request.referer)
	}
	header.set("Accept-Encoding", chromeAcceptEncoding)
	header.set("Accept-Language", chromeAcceptLanguage)
	if credentials {
		if cookie := b.cookies.header(target); cookie != "" {
			header.set("Cookie", cookie)
		}
	}
	if cached != nil {
		if etag := cached.header.Get("ETag"); etag != "" {
			header.set("If-None-Match", etag)
		}
		if modified := cached.header.Get("Last-Modified"); modified != "" {
			header.set("If-Modified-Since", modified)
		}
	}
	if spec.priority != "" {
		header.set("Priority", spec.priority)
	}

	requested := time.Now()
	response, err := b.roundTrip(ctx, request.method, target, header.sorted(order, page), request.body)
	if err != nil {
		return nil, err
	}
	if credentials {
		b.cookies.store(target, response.Header)
	}
	if !cacheable {
		return response, nil
	}
	if cached != nil && response.StatusCode == http.StatusNotModified {
		response.Body.Close()
		return b.cache.refresh(target.String(), cached, response.Header, requested).response(target), nil
	}
	return b.cache.store(target.String(), response, requested)
}

// roundTrip sends one request through the pool of its host.
func (b *browser) roundTrip(ctx context.Context, method string, target *url.URL, header fhttp.Header, body []byte) (*http.Response, error) {
	client, err := b.client(target)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := fhttp.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return nil, err
	}
	request.Header = header
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		Status: response.Status, StatusCode: response.StatusCode, Proto: response.Proto,
		Header: http.Header(response.Header), Body: response.Body, ContentLength: response.ContentLength,
		Request: &http.Request{Method: method, URL: target},
	}, nil
}

// trustworthy tells whether target is a secure context, which client hints
// go to: https, or a loopback address.
func trustworthy(target *url.URL) bool {
	if strings.EqualFold(target.Scheme, "https") || strings.EqualFold(target.Scheme, "wss") {
		return true
	}
	host := strings.ToLower(target.Hostname())
	ip := net.ParseIP(host)
	return host == "localhost" || strings.HasSuffix(host, ".localhost") || ip != nil && ip.IsLoopback()
}

// preflightCache is Chrome's CORS preflight cache: what a preflight
// allowed, by origin and URL, for its Access-Control-Max-Age (5 seconds
// without one, two hours at most).
type preflightCache struct {
	mu      sync.Mutex
	entries map[string]preflightEntry
}

type preflightEntry struct {
	expires              time.Time
	methods, headers     map[string]bool
	anyMethod, anyHeader bool
}

const (
	defaultPreflightAge = 5 * time.Second
	maxPreflightAge     = 2 * time.Hour
)

// preflight makes sure a CORS request may go: from the cache, or with an
// OPTIONS request first when it sets a header that is not safelisted or
// uses a method that is not.
func (b *browser) preflight(ctx context.Context, request browserRequest, target *url.URL, origin, site string) error {
	var names []string
	for _, field := range request.header {
		if !safelistedHeader(field[0], field[1]) {
			names = append(names, strings.ToLower(field[0]))
		}
	}
	simpleMethod := request.method == http.MethodGet || request.method == http.MethodHead || request.method == http.MethodPost
	if simpleMethod && len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	key := origin + " " + target.String()
	now := time.Now()
	b.preflights.mu.Lock()
	entry, ok := b.preflights.entries[key]
	b.preflights.mu.Unlock()
	if ok && now.Before(entry.expires) && entry.allows(request.method, names, request.withCredentials) {
		return nil
	}
	header := newChromeHeader()
	header.set("Connection", "keep-alive")
	header.set("Accept", "*/*")
	header.set("Access-Control-Request-Method", request.method)
	if len(names) > 0 {
		header.set("Access-Control-Request-Headers", strings.Join(names, ","))
	}
	header.set("Origin", origin)
	header.set("User-Agent", chromeUserAgent)
	header.set("Sec-Fetch-Mode", "cors")
	header.set("Sec-Fetch-Site", site)
	header.set("Sec-Fetch-Dest", "empty")
	if request.referer != "" {
		header.set("Referer", request.referer)
	}
	header.set("Accept-Encoding", chromeAcceptEncoding)
	header.set("Accept-Language", chromeAcceptLanguage)
	header.set("Priority", fetchSpecs[fetchPreflight].priority)
	response, err := b.roundTrip(ctx, http.MethodOptions, target, header.sorted(preflightOrder, nil), nil)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	response.Body.Close()
	entry, err = parsePreflight(response, origin, request.withCredentials, now)
	if err == nil && !entry.allows(request.method, names, request.withCredentials) {
		err = fmt.Errorf("%w: preflight of %s does not allow %s %v", errCORS, target.Host, request.method, names)
	}
	if err != nil {
		return err
	}
	b.preflights.mu.Lock()
	for cachedKey, cached := range b.preflights.entries {
		if !now.Before(cached.expires) {
			delete(b.preflights.entries, cachedKey)
		}
	}
	b.preflights.entries[key] = entry
	b.preflights.mu.Unlock()
	return nil
}

// parsePreflight checks a preflight's answer as Chrome does: an ok
// status and the origin allowed.
func parsePreflight(response *http.Response, origin string, credentials bool, now time.Time) (preflightEntry, error) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return preflightEntry{}, fmt.Errorf("%w: preflight returned %s", errCORS, response.Status)
	}
	allowed := response.Header.Get("Access-Control-Allow-Origin")
	if allowed != origin && (allowed != "*" || credentials) {
		return preflightEntry{}, fmt.Errorf("%w: preflight allows origin %q", errCORS, allowed)
	}
	if credentials && response.Header.Get("Access-Control-Allow-Credentials") != "true" {
		return preflightEntry{}, fmt.Errorf("%w: preflight does not allow credentials", errCORS)
	}
	age := defaultPreflightAge
	if value := strings.TrimSpace(response.Header.Get("Access-Control-Max-Age")); value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
			age = min(time.Duration(seconds)*time.Second, maxPreflightAge)
		}
	}
	entry := preflightEntry{expires: now.Add(age), methods: map[string]bool{}, headers: map[string]bool{}}
	for _, value := range response.Header.Values("Access-Control-Allow-Methods") {
		for _, method := range strings.Split(value, ",") {
			if method = strings.TrimSpace(method); method == "*" {
				entry.anyMethod = true
			} else if method != "" {
				entry.methods[method] = true
			}
		}
	}
	for _, value := range response.Header.Values("Access-Control-Allow-Headers") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.ToLower(strings.TrimSpace(name)); name == "*" {
				entry.anyHeader = true
			} else if name != "" {
				entry.headers[name] = true
			}
		}
	}
	return entry, nil
}

func (entry preflightEntry) allows(method string, headers []string, credentials bool) bool {
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodPost &&
		!entry.methods[method] && !(entry.anyMethod && !credentials) {
		return false
	}
	for _, name := range headers {
		if !entry.headers[name] && !(entry.anyHeader && !credentials && name != "authorization") {
			return false
		}
	}
	return true
}

// browserCookies is the cookie store of the player's frame. The player
// page is an iframe of the provider's page, another site, so for cookies
// every request of it is cross-site: as in Chrome, only SameSite=None
// cookies, which must be Secure, are stored and sent. A loopback address
// counts as secure, as in Chrome.
type browserCookies struct {
	jar *cookiejar.Jar
}

func jarURL(target *url.URL) *url.URL {
	if target.Scheme == "http" && trustworthy(target) || target.Scheme == "wss" || target.Scheme == "ws" && trustworthy(target) {
		secure := *target
		secure.Scheme = "https"
		return &secure
	}
	if target.Scheme == "ws" {
		plain := *target
		plain.Scheme = "http"
		return &plain
	}
	return target
}

func (c *browserCookies) header(target *url.URL) string {
	var pairs []string
	for _, cookie := range c.jar.Cookies(jarURL(target)) {
		pairs = append(pairs, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(pairs, "; ")
}

func (c *browserCookies) store(target *url.URL, header http.Header) {
	var kept []*http.Cookie
	for _, cookie := range (&http.Response{Header: header}).Cookies() {
		if cookie.SameSite == http.SameSiteNoneMode && cookie.Secure {
			kept = append(kept, cookie)
		}
	}
	if len(kept) > 0 {
		c.jar.SetCookies(jarURL(target), kept)
	}
}

// httpCache is Chrome's HTTP cache for the page's subresources, in memory:
// a response is fresh for its max-age (else until Expires, else a tenth of
// the time since Last-Modified), is revalidated with If-None-Match and
// If-Modified-Since once stale or with no-cache, and is not kept with
// no-store or Vary: *.
type httpCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	size    int
}

type cacheEntry struct {
	status              int
	header              http.Header
	body                []byte
	requested, received time.Time
}

const (
	cacheEntryLimit = 8 << 20
	cacheLimit      = 64 << 20
)

func cacheDirectives(header http.Header) map[string]string {
	directives := map[string]string{}
	for _, value := range header.Values("Cache-Control") {
		for _, part := range strings.Split(value, ",") {
			name, argument, _ := strings.Cut(strings.TrimSpace(part), "=")
			if name != "" {
				directives[strings.ToLower(name)] = strings.Trim(argument, `"`)
			}
		}
	}
	return directives
}

// fresh tells whether the entry may be used without asking the server.
func (e *cacheEntry) fresh(now time.Time) bool {
	directives := cacheDirectives(e.header)
	if _, noCache := directives["no-cache"]; noCache {
		return false
	}
	date, err := http.ParseTime(e.header.Get("Date"))
	if err != nil {
		date = e.received
	}
	var lifetime time.Duration
	if maxAge, ok := directives["max-age"]; ok {
		seconds, _ := strconv.ParseInt(maxAge, 10, 64)
		lifetime = time.Duration(seconds) * time.Second
	} else if expires := e.header.Get("Expires"); expires != "" {
		if at, err := http.ParseTime(expires); err == nil {
			lifetime = at.Sub(date)
		}
	} else if modified, err := http.ParseTime(e.header.Get("Last-Modified")); err == nil && date.After(modified) {
		lifetime = date.Sub(modified) / 10
	}
	age := max(0, e.received.Sub(date))
	if seconds, err := strconv.ParseInt(e.header.Get("Age"), 10, 64); err == nil && seconds > 0 {
		age += time.Duration(seconds) * time.Second
	}
	age += now.Sub(e.received)
	return age < lifetime
}

func (e *cacheEntry) response(target *url.URL) *http.Response {
	return &http.Response{
		Status: strconv.Itoa(e.status) + " " + http.StatusText(e.status), StatusCode: e.status,
		Header: e.header.Clone(), Body: io.NopCloser(bytes.NewReader(e.body)), ContentLength: int64(len(e.body)),
		Request: &http.Request{Method: http.MethodGet, URL: target},
	}
}

// lookup is the entry for key, and whether it is fresh.
func (c *httpCache) lookup(key string, now time.Time) (*cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil {
		return nil, false
	}
	return entry, entry.fresh(now)
}

// refresh takes in a 304: the entry's headers are updated with its.
func (c *httpCache) refresh(key string, entry *cacheEntry, header http.Header, requested time.Time) *cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	updated := &cacheEntry{status: entry.status, header: entry.header.Clone(), body: entry.body, requested: requested, received: time.Now()}
	for name, values := range header {
		switch http.CanonicalHeaderKey(name) {
		case "Content-Length", "Content-Encoding", "Transfer-Encoding", "Content-Range":
		default:
			updated.header[name] = values
		}
	}
	if c.entries[key] == entry {
		c.entries[key] = updated
	}
	return updated
}

// store reads a response and keeps it if it may be.
func (c *httpCache) store(key string, response *http.Response, requested time.Time) (*http.Response, error) {
	directives := cacheDirectives(response.Header)
	_, noStore := directives["no-store"]
	if response.StatusCode != http.StatusOK || noStore || strings.Contains(response.Header.Get("Vary"), "*") {
		return response, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, cacheEntryLimit+1))
	response.Body.Close()
	if err != nil {
		return nil, err
	}
	header := response.Header.Clone()
	// fhttp hands the body over decoded.
	header.Del("Content-Encoding")
	header.Del("Content-Length")
	entry := &cacheEntry{status: response.StatusCode, header: header, body: body, requested: requested, received: time.Now()}
	if len(body) <= cacheEntryLimit {
		c.mu.Lock()
		if old := c.entries[key]; old != nil {
			c.size -= len(old.body)
		}
		c.entries[key] = entry
		c.size += len(body)
		for c.size > cacheLimit {
			var oldestKey string
			var oldest time.Time
			for cachedKey, cached := range c.entries {
				if oldestKey == "" || cached.received.Before(oldest) {
					oldestKey, oldest = cachedKey, cached.received
				}
			}
			c.size -= len(c.entries[oldestKey].body)
			delete(c.entries, oldestKey)
		}
		c.mu.Unlock()
	}
	return entry.response(response.Request.URL), nil
}

// dialWebSocket opens a ws:// or wss:// URL as Chrome opens a page's
// WebSocket: its own connection, with ALPN http/1.1 only, and Chrome's
// handshake headers, which offer permessage-deflate. ctx bounds the
// handshake only.
func (b *browser) dialWebSocket(ctx context.Context, rawURL, origin string) (*wsConn, error) {
	target, err := url.Parse(rawURL)
	if err != nil || target.Scheme != "ws" && target.Scheme != "wss" {
		return nil, fmt.Errorf("not a websocket URL: %q", rawURL)
	}
	client, err := b.client(target)
	if err != nil {
		return nil, err
	}
	address := net.JoinHostPort(target.Hostname(), effectivePort(target))
	var conn net.Conn
	if target.Scheme == "wss" {
		conn, err = client.GetTLSDialer()(ctx, "tcp", address)
	} else {
		conn, err = client.GetDialer().DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	ws, err := b.handshake(conn, target, origin)
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ws, nil
}

func (b *browser) handshake(conn net.Conn, target *url.URL, origin string) (*wsConn, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce)
	host := target.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := target.Port(); port != "" && port != effectivePort(&url.URL{Scheme: target.Scheme}) {
		host += ":" + port
	}
	var request strings.Builder
	fmt.Fprintf(&request, "GET %s HTTP/1.1\r\n", target.RequestURI())
	lines := [][2]string{
		{"Host", host}, {"Connection", "Upgrade"}, {"Pragma", "no-cache"}, {"Cache-Control", "no-cache"},
		{"User-Agent", chromeUserAgent}, {"Upgrade", "websocket"}, {"Origin", origin},
		{"Sec-WebSocket-Version", "13"}, {"Accept-Encoding", chromeAcceptEncoding},
		{"Accept-Language", chromeAcceptLanguage},
	}
	if cookie := b.cookies.header(target); cookie != "" {
		lines = append(lines, [2]string{"Cookie", cookie})
	}
	lines = append(lines, [2]string{"Sec-WebSocket-Key", key},
		[2]string{"Sec-WebSocket-Extensions", "permessage-deflate; client_max_window_bits"})
	for _, line := range lines {
		fmt.Fprintf(&request, "%s: %s\r\n", line[0], line[1])
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet, URL: target})
	if err != nil {
		return nil, err
	}
	b.cookies.store(target, response.Header)
	if response.StatusCode != http.StatusSwitchingProtocols {
		response.Body.Close()
		return nil, &alternativeStatusError{status: response.Status, code: response.StatusCode, vd: response.Header.Get("X-VD")}
	}
	digest := sha1.Sum([]byte(key + wsAcceptGUID))
	if !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") ||
		!headerHasToken(response.Header, "Connection", "upgrade") ||
		response.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(digest[:]) {
		return nil, errors.New("websocket upgrade: bad handshake reply")
	}
	ws := &wsConn{rwc: conn, br: reader}
	for _, value := range response.Header.Values("Sec-WebSocket-Extensions") {
		for _, extension := range strings.Split(value, ",") {
			name, _, _ := strings.Cut(extension, ";")
			switch strings.TrimSpace(name) {
			case "permessage-deflate":
				ws.inflate = &wsInflater{}
			case "":
			default:
				return nil, fmt.Errorf("websocket upgrade: extension %q not offered", extension)
			}
		}
	}
	return ws, nil
}
