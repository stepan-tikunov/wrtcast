package provider

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func TestChromeBrands(t *testing.T) {
	t.Parallel()
	for major, want := range map[int]string{
		120: `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`,
		124: `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`,
		131: `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
	} {
		if got := chromeBrands(major); got != want {
			t.Errorf("chromeBrands(%d) = %s, want %s", major, got, want)
		}
	}
	if !strings.Contains(chromeUserAgent, "(Windows NT 10.0; Win64; x64)") || !strings.Contains(chromeUserAgent, "Chrome/152.0.0.0 ") ||
		!strings.Contains(chromeBrandList, `"Google Chrome";v="152"`) {
		t.Fatalf("identity %q, %q", chromeUserAgent, chromeBrandList)
	}
}

// h2Capture is what an HTTP/2 server saw of the browser: the ClientHello's
// ALPN, the connection's first frames and each request's header fields in
// wire order.
type h2Capture struct {
	mu          sync.Mutex
	alpn        []string
	ciphers     []uint16
	settings    []http2.Setting
	window      uint32
	priority    http2.PriorityParam
	headers     [][]hpack.HeaderField
	connections int
}

// newH2Server is a TLS server that speaks HTTP/2 frame by frame, answering
// every request with 200 and a short body.
func newH2Server(t *testing.T, capture *h2Capture) string {
	t.Helper()
	certificate := httptest.NewUnstartedServer(nil)
	certificate.StartTLS()
	config := &tls.Config{
		Certificates: certificate.TLS.Certificates, NextProtos: []string{"h2"},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			capture.mu.Lock()
			capture.alpn, capture.ciphers = hello.SupportedProtos, hello.CipherSuites
			capture.mu.Unlock()
			return nil, nil
		},
	}
	certificate.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			capture.mu.Lock()
			capture.connections++
			capture.mu.Unlock()
			go serveH2(conn, capture)
		}
	}()
	return "https://" + listener.Addr().String()
}

func serveH2(conn net.Conn, capture *h2Capture) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(reader, preface); err != nil {
		return
	}
	framer := http2.NewFramer(conn, reader)
	_ = framer.WriteSettings()
	decoder := hpack.NewDecoder(65536, nil)
	var encoded strings.Builder
	encoder := hpack.NewEncoder(&encoded)
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			return
		}
		switch frame := frame.(type) {
		case *http2.SettingsFrame:
			if frame.IsAck() {
				continue
			}
			capture.mu.Lock()
			if capture.settings == nil {
				_ = frame.ForeachSetting(func(setting http2.Setting) error {
					capture.settings = append(capture.settings, setting)
					return nil
				})
			}
			capture.mu.Unlock()
			_ = framer.WriteSettingsAck()
		case *http2.WindowUpdateFrame:
			capture.mu.Lock()
			if frame.StreamID == 0 && capture.window == 0 {
				capture.window = frame.Increment
			}
			capture.mu.Unlock()
		case *http2.HeadersFrame:
			fields, _ := decoder.DecodeFull(frame.HeaderBlockFragment())
			capture.mu.Lock()
			capture.headers = append(capture.headers, fields)
			capture.priority = frame.Priority
			capture.mu.Unlock()
			encoded.Reset()
			_ = encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
			_ = encoder.WriteField(hpack.HeaderField{Name: "content-type", Value: "text/plain"})
			if len(fields) > 0 && fields[0].Value == http.MethodOptions {
				_ = encoder.WriteField(hpack.HeaderField{Name: "access-control-allow-origin", Value: "*"})
				_ = encoder.WriteField(hpack.HeaderField{Name: "access-control-allow-headers", Value: "accepts-controls, authorizations"})
			}
			_ = framer.WriteHeaders(http2.HeadersFrameParam{StreamID: frame.StreamID, BlockFragment: []byte(encoded.String()), EndHeaders: true})
			_ = framer.WriteData(frame.StreamID, true, []byte("ok"))
		}
	}
}

func fieldNames(fields []hpack.HeaderField) []string {
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, field.Name)
	}
	return names
}

// Over HTTP/2 the browser looks like Chrome to the server: its ALPN, its
// SETTINGS in Chrome's order, the connection window, the priority of its
// HEADERS, the pseudo-headers m,a,s,p and Chrome's header order, all on one
// connection.
func TestBrowserHTTP2Fingerprint(t *testing.T) {
	t.Parallel()
	var capture h2Capture
	origin := newH2Server(t, &capture)
	b := newBrowser(true)
	page, _ := url.Parse("https://player.example/?token_movie=abc&token=def")
	for _, path := range []string{"/seg-1.m4s", "/seg-2.m4s"} {
		response, err := b.do(context.Background(), browserRequest{
			kind: fetchXHR, url: origin + path, initiator: page, referer: page.String(),
			header: [][2]string{{"Accepts-Controls", "fp"}, {"Authorizations", "Bearer guard"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || string(body) != "ok" || response.Proto != "HTTP/2.0" {
			t.Fatalf("response %s %q", response.Status, body)
		}
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if !reflect.DeepEqual(capture.alpn, []string{"h2", "http/1.1"}) || len(capture.ciphers) < 10 ||
		capture.ciphers[0]&0x0f0f != 0x0a0a || capture.ciphers[1] != tls.TLS_AES_128_GCM_SHA256 {
		t.Errorf("ClientHello ALPN %v, ciphers %x", capture.alpn, capture.ciphers)
	}
	wantSettings := []http2.Setting{
		{ID: http2.SettingHeaderTableSize, Val: 65536}, {ID: http2.SettingEnablePush, Val: 0},
		{ID: http2.SettingInitialWindowSize, Val: 6291456}, {ID: http2.SettingMaxHeaderListSize, Val: 262144},
	}
	if !reflect.DeepEqual(capture.settings, wantSettings) || capture.window != 15663105 ||
		capture.priority != (http2.PriorityParam{StreamDep: 0, Exclusive: true, Weight: 255}) {
		t.Errorf("settings %v, window %d, priority %+v", capture.settings, capture.window, capture.priority)
	}
	// Preflights and requests alike, on one connection.
	if capture.connections != 1 || len(capture.headers) != 4 {
		t.Fatalf("%d connections, %d requests", capture.connections, len(capture.headers))
	}
	wantPreflight := []string{
		":method", ":authority", ":scheme", ":path", "accept", "access-control-request-method",
		"access-control-request-headers", "origin", "user-agent", "sec-fetch-mode", "sec-fetch-site", "sec-fetch-dest",
		"referer", "accept-encoding", "accept-language", "priority",
	}
	wantRequest := []string{
		":method", ":authority", ":scheme", ":path", "sec-ch-ua-platform", "accepts-controls", "authorizations",
		"user-agent", "accept", "sec-ch-ua", "sec-ch-ua-mobile", "origin", "sec-fetch-site", "sec-fetch-mode",
		"sec-fetch-dest", "referer", "accept-encoding", "accept-language", "priority",
	}
	for index, want := range [][]string{wantPreflight, wantRequest, wantPreflight, wantRequest} {
		if got := fieldNames(capture.headers[index]); !reflect.DeepEqual(got, want) {
			t.Errorf("request %d headers %v\nwant %v", index, got, want)
		}
	}
	values := map[string]string{}
	for _, field := range capture.headers[1] {
		values[field.Name] = field.Value
	}
	if values[":method"] != "GET" || values["origin"] != "https://player.example" || values["sec-fetch-site"] != "cross-site" ||
		values["sec-fetch-mode"] != "cors" || values["accept-encoding"] != chromeAcceptEncoding ||
		values["sec-ch-ua"] != chromeBrandList || values["sec-ch-ua-platform"] != `"Windows"` || values["priority"] != "u=1, i" {
		t.Errorf("request header values %v", values)
	}
	if got := capture.headers[0][6].Value; got != "accepts-controls,authorizations" {
		t.Errorf("Access-Control-Request-Headers %q", got)
	}
}

// A CORS request with the page's own headers is preflighted once per URL
// for the preflight's max-age; one that fails it is not sent. A
// subresource comes from the HTTP cache while fresh and is revalidated
// once it is not; only SameSite=None cookies of the player's frame are
// kept, and only same-origin requests send them.
func TestBrowserCORSCacheAndCookies(t *testing.T) {
	t.Parallel()
	var preflights, gets, revalidations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodOptions:
			preflights.Add(1)
			if request.URL.Path == "/refused" {
				response.WriteHeader(http.StatusForbidden)
				return
			}
			response.Header().Set("Access-Control-Allow-Origin", "*")
			response.Header().Set("Access-Control-Allow-Headers", "Accepts-Controls")
			response.Header().Set("Access-Control-Max-Age", "600")
		case request.URL.Path == "/app.js":
			gets.Add(1)
			response.Header().Set("Cache-Control", "max-age=3600")
			_, _ = io.WriteString(response, "bundle")
		case request.URL.Path == "/rmp.js":
			if request.Header.Get("If-None-Match") == `"v1"` {
				revalidations.Add(1)
				response.WriteHeader(http.StatusNotModified)
				return
			}
			gets.Add(1)
			response.Header().Set("Cache-Control", "no-cache")
			response.Header().Set("ETag", `"v1"`)
			_, _ = io.WriteString(response, "rmp")
		case request.URL.Path == "/page":
			http.SetCookie(response, &http.Cookie{Name: "third", Value: "1", SameSite: http.SameSiteNoneMode, Secure: true})
			http.SetCookie(response, &http.Cookie{Name: "lax", Value: "1"})
		default:
			gets.Add(1)
			if cookie := request.Header.Get("Cookie"); request.URL.Path == "/same" && cookie != "third=1" ||
				request.URL.Path != "/same" && cookie != "" {
				t.Errorf("%s with Cookie %q", request.URL.Path, cookie)
			}
		}
	}))
	defer server.Close()
	b := newBrowser(false)
	ctx := context.Background()
	self, _ := url.Parse(server.URL + "/page")
	other, _ := url.Parse("https://player.example/")
	get := func(kind fetchKind, initiator *url.URL, path string, header ...[2]string) (string, error) {
		response, err := b.do(ctx, browserRequest{kind: kind, url: server.URL + path, initiator: initiator, referer: initiator.String(), header: header})
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return string(body), err
	}
	if _, err := get(fetchNavigate, other, "/page"); err != nil {
		t.Fatal(err)
	}
	control := [2]string{"Accepts-Controls", "fp"}
	for _, path := range []string{"/a", "/a", "/b"} {
		if _, err := get(fetchXHR, other, path, control); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := get(fetchXHR, other, "/refused", control); !errors.Is(err, errCORS) {
		t.Fatalf("refused preflight: %v", err)
	}
	if _, err := get(fetchXHR, self, "/same", control); err != nil {
		t.Fatal(err)
	}
	if got := preflights.Load(); got != 3 {
		t.Errorf("%d preflights for /a twice, /b and /refused", got)
	}
	gets.Store(0)
	for range 2 {
		if body, err := get(fetchDeferredScript, self, "/app.js"); err != nil || body != "bundle" {
			t.Fatalf("app.js %q, %v", body, err)
		}
		if body, err := get(fetchScript, self, "/rmp.js"); err != nil || body != "rmp" {
			t.Fatalf("rmp.js %q, %v", body, err)
		}
	}
	if gets.Load() != 2 || revalidations.Load() != 1 {
		t.Errorf("%d fetches and %d revalidations of two scripts loaded twice", gets.Load(), revalidations.Load())
	}
}

// The player page loads as an iframe of the provider's page: a cross-site
// navigation with the provider's origin as Referer, then its stylesheet and
// scripts as subresources of it, before the playback request.
func TestAlternativePlayerPageLoad(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []string
	headers := map[string]http.Header{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		seen = append(seen, request.Method+" "+request.URL.Path)
		headers[request.URL.Path] = request.Header.Clone()
		mu.Unlock()
		switch request.URL.Path {
		case "/":
			_, _ = io.WriteString(response, `<meta name="viewporti" content="nonce">
<link rel="stylesheet" href="/build/app.css" integrity="sha384-x">
<script src="./js/rmp-vast.min.js?v=2.6"></script>
<script src="/build/app.js" defer integrity="sha384-y"></script>
<script>const userParam = { token: 'tok' }; const fileList = JSON.parse('{"type":"movie","active":{"id":7,"id_file":"8"},"all":{}}');</script>`)
		case "/bnsi/movies/7":
			_, _ = io.WriteString(response, `{"hlsSource":[{"label":"Dub","quality":{"480":"`+server.URL+`/480.m3u8"}}],"time":1}`)
		}
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: "https://go.example.pro", MediaBaseURL: "https://api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	streams, err := client.alternativeStreams(context.Background(), server.URL+"/?token_movie=abc&token=tok", 0, "")
	if err != nil || len(streams) != 1 {
		t.Fatalf("streams %+v, %v", streams, err)
	}
	waitFor(t, "the icon sprite", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return headers["/images/allplay.svg"] != nil
	})
	mu.Lock()
	defer mu.Unlock()
	if seen[0] != "GET /" || len(seen) != 6 || !strings.HasPrefix(seen[4]+seen[5], "POST /bnsi/movies/7") &&
		!strings.HasPrefix(seen[5], "POST /bnsi/movies/7") {
		t.Fatalf("requests %v", seen)
	}
	page := server.URL + "/?token_movie=abc&token=tok"
	for path, want := range map[string]map[string]string{
		"/": {"Sec-Fetch-Dest": "iframe", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "cross-site",
			"Referer": "https://go.example.pro/", "Upgrade-Insecure-Requests": "1", "Accept": navigationAccept, "Priority": "u=0, i"},
		"/build/app.css":      {"Sec-Fetch-Dest": "style", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Site": "same-origin", "Referer": page, "Priority": "u=0"},
		"/js/rmp-vast.min.js": {"Sec-Fetch-Dest": "script", "Sec-Fetch-Mode": "no-cors", "Referer": page, "Priority": "u=1"},
		"/build/app.js":       {"Sec-Fetch-Dest": "script", "Priority": ""},
		"/bnsi/movies/7":      {"Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "same-origin", "X-Requested-With": "XMLHttpRequest", "Origin": server.URL},
	} {
		for name, value := range want {
			if got := headers[path].Get(name); got != value {
				t.Errorf("%s %s = %q, want %q", path, name, got, value)
			}
		}
	}
	if (alternativeFile{ID: 7, IDFile: "8"}).statID() != 8 || (alternativeFile{ID: 7, IDFile: "0"}).statID() != 7 {
		t.Error("statID of a string id_file")
	}
}
