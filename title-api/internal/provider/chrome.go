package provider

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	// The browser's time zone (see browserTimeZone) in an image without
	// zoneinfo.
	_ "time/tzdata"

	fhttp "github.com/bogdanfinn/fhttp"
	"golang.org/x/net/publicsuffix"
)

// The alternative player's site, its CDN and its ad network see title-api as
// one desktop Chrome on Windows: Chrome's TLS ClientHello and HTTP/2
// settings (see browser), its User-Agent and client hints, and for each kind
// of request a page makes the headers Chrome sends, in Chrome's order. The
// main provider's source keeps its own client and the Chromecast's
// User-Agent, which the receiver fetches its links with.

// chromeMajor is the version of the TLS profile (see newBrowser), which the
// User-Agent and the client hints must name too.
const chromeMajor = 152

var (
	chromeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/" + strconv.Itoa(chromeMajor) + ".0.0.0 Safari/537.36"
	chromeBrandList = chromeBrands(chromeMajor)
)

const (
	chromePlatform       = `"Windows"`
	chromeAcceptLanguage = "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7"
	chromeAcceptEncoding = "gzip, deflate, br, zstd"
	navigationAccept     = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp," +
		"image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"
)

// chromeBrands is sec-ch-ua of a Chrome major version: its GREASE brand,
// and the order of the three, both follow from the version as Chromium
// makes them (GenerateBrandVersionList).
func chromeBrands(major int) string {
	chars := []string{" ", "(", ":", "-", ".", "/", ")", ";", "=", "?", "_"}
	versions := []string{"8", "99", "24"}
	orders := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	version := strconv.Itoa(major)
	brands := [3]string{
		fmt.Sprintf(`"Not%sA%sBrand";v="%s"`, chars[major%len(chars)], chars[(major+1)%len(chars)], versions[major%len(versions)]),
		`"Chromium";v="` + version + `"`,
		`"Google Chrome";v="` + version + `"`,
	}
	var list [3]string
	for index, brand := range brands {
		list[orders[major%len(orders)][index]] = brand
	}
	return strings.Join(list[:], ", ")
}

// fetchKind is what in the page makes a request, which decides its
// Sec-Fetch-Mode and -Dest, its Accept, its credentials and its priority.
type fetchKind int

const (
	fetchNavigate       fetchKind = iota // the player page, in its iframe
	fetchStyle                           // <link rel=stylesheet>
	fetchScript                          // a parser-blocking <script src>
	fetchDeferredScript                  // <script defer>
	fetchImage                           // the icon sprite of an SVG <use>
	fetchXHR                             // XMLHttpRequest: jQuery's, hls.js's
	fetchFetch                           // fetch()
	fetchProbe                           // fetch() with mode no-cors
	fetchBeacon                          // navigator.sendBeacon
	fetchTrack                           // <track> of a crossorigin <video>
	fetchPreflight                       // the CORS preflight of a request
)

// fetchSpec is a kind's request mode, destination and credentials mode,
// the Accept the browser gives it, and its priority: blink's load priority
// (a stylesheet VeryHigh, a blocking script and XHR High, a deferred
// script, an image and a track Low, a beacon VeryLow) as an urgency
// (HIGHEST 0, MEDIUM 1, LOWEST 3, IDLE 4), incremental but for scripts and
// stylesheets; Chrome leaves out what is the default (u=3, not
// incremental), so a deferred script has none.
type fetchSpec struct {
	mode, dest, credentials, accept, priority string
}

const imageAccept = "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8"

var fetchSpecs = map[fetchKind]fetchSpec{
	fetchNavigate:       {"navigate", "iframe", "include", navigationAccept, "u=0, i"},
	fetchStyle:          {"no-cors", "style", "include", "text/css,*/*;q=0.1", "u=0"},
	fetchScript:         {"no-cors", "script", "include", "*/*", "u=1"},
	fetchDeferredScript: {"no-cors", "script", "include", "*/*", ""},
	fetchImage:          {"same-origin", "image", "include", imageAccept, "i"},
	fetchXHR:            {"cors", "empty", "same-origin", "*/*", "u=1, i"},
	fetchFetch:          {"cors", "empty", "same-origin", "*/*", "u=1, i"},
	fetchProbe:          {"no-cors", "empty", "same-origin", "*/*", "u=1, i"},
	fetchBeacon:         {"no-cors", "empty", "include", "*/*", "u=4, i"},
	fetchTrack:          {"cors", "track", "same-origin", "*/*", "i"},
	fetchPreflight:      {"cors", "empty", "omit", "*/*", "u=1, i"},
}

// Chrome's header order. HTTP/1.1 puts Host and Connection first; HTTP/2
// drops them, and fhttp sends :method, :authority, :scheme and :path ahead
// as the profile orders them. The page's own headers (subresourceOrder's
// "page") go where Chrome puts them, after sec-ch-ua-platform.
var (
	navigationOrder = []string{
		"host", "connection", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "upgrade-insecure-requests",
		"user-agent", "accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest",
		"referer", "accept-encoding", "accept-language", "cookie", "if-none-match", "if-modified-since", "priority",
	}
	subresourceOrder = []string{
		"host", "connection", "content-length", "sec-ch-ua-platform", "page", "user-agent", "accept", "sec-ch-ua",
		"content-type", "sec-ch-ua-mobile", "origin", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest",
		"referer", "accept-encoding", "accept-language", "cookie", "if-none-match", "if-modified-since", "priority",
	}
	preflightOrder = []string{
		"host", "connection", "accept", "access-control-request-method", "access-control-request-headers",
		"origin", "user-agent", "sec-fetch-mode", "sec-fetch-site", "sec-fetch-dest", "referer",
		"accept-encoding", "accept-language", "priority",
	}
)

// chromeHeader is a header list built in Chrome's order.
type chromeHeader struct {
	header fhttp.Header
	order  []string
}

func newChromeHeader() *chromeHeader {
	return &chromeHeader{header: fhttp.Header{}}
}

// set adds a header, named as Chrome spells it on HTTP/1.1 (HTTP/2 writes
// every name in lower case).
func (h *chromeHeader) set(name, value string) {
	h.header[name] = []string{value}
	h.order = append(h.order, strings.ToLower(name))
}

// sorted is the header with the wire order: that of order, the page's
// headers in place of "page", in the order the page set them.
func (h *chromeHeader) sorted(order []string, page []string) fhttp.Header {
	var wire []string
	for _, name := range order {
		if name == "page" {
			wire = append(wire, page...)
		} else {
			wire = append(wire, name)
		}
	}
	h.header[fhttp.HeaderOrderKey] = wire
	return h.header
}

// fetchSite is Sec-Fetch-Site of a request to target from initiator.
func fetchSite(initiator, target *url.URL) string {
	switch {
	case sameOrigin(initiator, target):
		return "same-origin"
	case strings.EqualFold(initiator.Scheme, target.Scheme) && registrableDomain(initiator) == registrableDomain(target):
		return "same-site"
	default:
		return "cross-site"
	}
}

func sameOrigin(first, second *url.URL) bool {
	return strings.EqualFold(first.Scheme, second.Scheme) && strings.EqualFold(first.Hostname(), second.Hostname()) &&
		effectivePort(first) == effectivePort(second)
}

func effectivePort(target *url.URL) string {
	if port := target.Port(); port != "" {
		return port
	}
	switch strings.ToLower(target.Scheme) {
	case "https", "wss":
		return "443"
	default:
		return "80"
	}
}

// registrableDomain is the site of a host: its eTLD+1, or an address
// itself.
func registrableDomain(target *url.URL) string {
	host := strings.ToLower(target.Hostname())
	if net.ParseIP(host) != nil {
		return host
	}
	if domain, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return domain
	}
	return host
}

// serializedOrigin is an origin as Origin carries it.
func serializedOrigin(target *url.URL) string {
	origin := strings.ToLower(target.Scheme) + "://" + strings.ToLower(target.Hostname())
	if port := target.Port(); port != "" && port != effectivePort(&url.URL{Scheme: target.Scheme}) {
		origin += ":" + port
	}
	return origin
}

// safelistedHeader tells whether a header the page sets is CORS-safelisted,
// so needs no preflight.
func safelistedHeader(name, value string) bool {
	switch strings.ToLower(name) {
	case "accept", "accept-language", "content-language":
		return true
	case "content-type":
		mediaType := strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
		return mediaType == "application/x-www-form-urlencoded" || mediaType == "multipart/form-data" || mediaType == "text/plain"
	default:
		return false
	}
}
