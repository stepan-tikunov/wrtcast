package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	titlev1 "title-api/api/title/v1"
	"title-api/internal/grpcserver"
	"title-api/internal/provider"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

const (
	defaultBaseURL     = "https://go.\x6c\x6f\x72\x64\x66\x69\x6c\x6d5.pro"
	defaultGRPCAddr    = ":50051"
	defaultHTTPAddr    = ":8080"
	defaultStateDBPath = "title-api-state.db"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	baseURL := envOrDefault("MOVIE_PROVIDER_BASE_URL", defaultBaseURL)
	providerUserAgent := os.Getenv("MOVIE_PROVIDER_USER_AGENT")
	mediaBaseURL := envOrDefault("TITLE_API_PUBLIC_URL", "http://localhost:50052")
	stateDBPath := envOrDefault("TITLE_API_STATE_DB", defaultStateDBPath)
	grpcAddress := envOrDefault("GRPC_ADDR", defaultGRPCAddr)
	httpAddress := envOrDefault("HTTP_ADDR", defaultHTTPAddr)
	urlStore, err := provider.OpenSQLiteProviderURLStore(ctx, stateDBPath)
	if err != nil {
		return err
	}
	defer urlStore.Close()

	client, err := provider.NewClient(provider.Config{
		BaseURL:          baseURL,
		MediaBaseURL:     mediaBaseURL,
		RefreshInterval:  5 * time.Minute,
		UserAgent:        providerUserAgent,
		URLStore:         urlStore,
		FingerprintStore: urlStore,
		AlignmentStore:   urlStore,
	})
	if err != nil {
		return err
	}
	// The playback sockets close with a close frame on the way out.
	defer client.Close()
	log.Printf("initializing provider anti-bot session for %s", baseURL)
	if err := client.Start(ctx); err != nil {
		return err
	}
	log.Printf("provider anti-bot session initialized for %s", client.ProviderURL())

	grpcListener, err := net.Listen("tcp", grpcAddress)
	if err != nil {
		return err
	}
	httpListener, err := net.Listen("tcp", httpAddress)
	if err != nil {
		_ = grpcListener.Close()
		return err
	}

	service := grpcserver.New(client)
	grpcServer := grpc.NewServer()
	titlev1.RegisterTitleServiceServer(grpcServer, service)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("title.v1.TitleService", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	reflection.Register(grpcServer)

	gateway := runtime.NewServeMux()
	if err := titlev1.RegisterTitleServiceHandlerServer(ctx, gateway, service); err != nil {
		_ = grpcListener.Close()
		_ = httpListener.Close()
		return err
	}
	for _, route := range []struct {
		method, path string
		handler      runtime.HandlerFunc
	}{
		{http.MethodGet, "/v1/posters", posterHandler(client)},
		{http.MethodGet, "/v1/media/{id}/master.m3u8", masterPlaylistHandler(client)},
		{http.MethodGet, "/v1/media/{id}/headers", mediaHeadersHandler(client)},
		{http.MethodGet, "/v1/media/{id}/quality/{quality}/playlist.m3u8", qualityPlaylistHandler(client)},
		{http.MethodGet, "/v1/media/{id}/cdn", mediaRelayHandler(client)},
		{http.MethodDelete, "/v1/media/{id}/session", mediaSessionHandler(client)},
		// The same for clients that can only GET and POST, like the router's
		// uclient-fetch.
		{http.MethodPost, "/v1/media/{id}/session/end", mediaSessionHandler(client)},
	} {
		if err := gateway.HandlePath(route.method, route.path, route.handler); err != nil {
			_ = grpcListener.Close()
			_ = httpListener.Close()
			return err
		}
	}
	httpServer := &http.Server{
		Handler:           allowAllOrigins(gateway),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf(
		"gRPC server listening on %s; HTTP gateway listening on %s; title_id prefix for %s is %s",
		grpcAddress,
		httpAddress,
		client.ProviderURL(),
		client.DomainPrefix(),
	)

	errorsChannel := make(chan error, 2)
	go func() {
		errorsChannel <- grpcServer.Serve(grpcListener)
	}()
	go func() {
		errorsChannel <- httpServer.Serve(httpListener)
	}()

	var serveError error
	select {
	case <-ctx.Done():
	case serveError = <-errorsChannel:
	}

	healthServer.Shutdown()
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	_ = httpServer.Shutdown(shutdownContext)
	grpcServer.GracefulStop()

	if serveError != nil && !errors.Is(serveError, http.ErrServerClosed) && ctx.Err() == nil {
		return serveError
	}
	return nil
}

type masterPlaylistSource interface {
	MasterPlaylist(ctx context.Context, id string) ([]byte, error)
}

func masterPlaylistHandler(source masterPlaylistSource) runtime.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request, pathParams map[string]string) {
		body, err := source.MasterPlaylist(request.Context(), pathParams["id"])
		if mediaError(response, err) {
			return
		}
		response.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		response.Header().Set("Cache-Control", "private, max-age=300")
		_, _ = response.Write(body)
	}
}

// mediaError answers a failed media request: the CDN's refusal with its
// status and X-VD, as the router passes those on.
func mediaError(response http.ResponseWriter, err error) bool {
	status, vd := provider.MediaStatus(err)
	switch {
	case err == nil:
		return false
	case errors.Is(err, provider.ErrMasterNotFound):
		http.Error(response, err.Error(), http.StatusNotFound)
	case errors.Is(err, provider.ErrMediaURL):
		http.Error(response, err.Error(), http.StatusBadRequest)
	// A 403 reaches the receiver as one, and it does not retry that.
	case errors.Is(err, provider.ErrCDNBlocked):
		http.Error(response, err.Error(), http.StatusForbidden)
	case status >= 400 && status < 600:
		if vd != "" {
			response.Header().Set("X-VD", vd)
		}
		http.Error(response, err.Error(), status)
	default:
		http.Error(response, err.Error(), http.StatusBadGateway)
	}
	return true
}

type qualityPlaylistSource interface {
	QualityPlaylist(ctx context.Context, id, quality string) ([]byte, error)
}

// qualityPlaylistHandler serves GET
// /v1/media/{id}/quality/{quality}/playlist.m3u8: the media playlist of a
// quality of a synthetic master, its URIs absolute.
func qualityPlaylistHandler(source qualityPlaylistSource) runtime.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request, pathParams map[string]string) {
		if !resolutionPattern.MatchString(pathParams["quality"]) {
			http.Error(response, "invalid quality", http.StatusBadRequest)
			return
		}
		body, err := source.QualityPlaylist(request.Context(), pathParams["id"], pathParams["quality"])
		if mediaError(response, err) {
			return
		}
		response.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		response.Header().Set("Cache-Control", "no-store")
		_, _ = response.Write(body)
	}
}

type mediaRelaySource interface {
	FetchMedia(ctx context.Context, id, rawURL string, preview bool) (*http.Response, error)
}

// mediaRelayHandler serves GET /v1/media/{id}/cdn?u=<CDN URL>[&pv=1]: the
// URL fetched for the router as the media's player page fetches it, the
// status, the type and X-VD passed on, the body streamed as it comes. pv=1
// marks a seek preview's fetch, which the player's view doesn't count.
func mediaRelayHandler(source mediaRelaySource) runtime.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request, pathParams map[string]string) {
		upstream, err := source.FetchMedia(request.Context(), pathParams["id"], request.URL.Query().Get("u"),
			request.URL.Query().Get("pv") == "1")
		if err != nil && request.Context().Err() == nil {
			log.Printf("media relay %s: %v", pathParams["id"], err)
		}
		if mediaError(response, err) {
			return
		}
		defer upstream.Body.Close()
		if upstream.StatusCode >= http.StatusBadRequest {
			log.Printf("media relay %s: CDN answered %d (X-VD: %q)", pathParams["id"], upstream.StatusCode, upstream.Header.Get("X-VD"))
		}
		for _, name := range []string{"Content-Type", "X-VD", "Last-Modified", "ETag"} {
			if value := upstream.Header.Get(name); value != "" {
				response.Header().Set(name, value)
			}
		}
		if upstream.ContentLength >= 0 && upstream.Header.Get("Content-Encoding") == "" {
			response.Header().Set("Content-Length", strconv.FormatInt(upstream.ContentLength, 10))
		}
		response.Header().Set("Cache-Control", "no-store")
		response.WriteHeader(upstream.StatusCode)
		flusher, _ := response.(http.Flusher)
		buffer := make([]byte, 64<<10)
		for {
			n, err := upstream.Body.Read(buffer)
			if n > 0 {
				if _, writeErr := response.Write(buffer[:n]); writeErr != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("media relay %s: %v", pathParams["id"], err)
				}
				return
			}
		}
	}
}

type mediaHeadersSource interface {
	MediaHeaders(ctx context.Context, id string, report provider.PlaybackReport) (provider.MediaHeaders, error)
	EndMediaSession(id string)
}

var (
	resolutionPattern = regexp.MustCompile(`^[0-9]{1,5}$`)
	positionPattern   = regexp.MustCompile(`^[0-9]{1,7}(\.[0-9]+)?$`)
	countPattern      = regexp.MustCompile(`^[0-9]{1,15}$`)
	flagPattern       = regexp.MustCompile(`^[01]$`)
)

type mediaHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// mediaSection is a section as the details have it (see the Section
// message).
type mediaSection struct {
	Kind         string  `json:"kind"`
	StartSeconds float64 `json:"startSeconds"`
	EndSeconds   float64 `json:"endSeconds"`
	Skippable    bool    `json:"skippable"`
}

var sectionKinds = map[provider.SectionKind]string{
	provider.SectionRecap:   "KIND_RECAP",
	provider.SectionIntro:   "KIND_INTRO",
	provider.SectionCredits: "KIND_CREDITS",
}

// mediaHeadersHandler serves GET
// /v1/media/{id}/headers?q=<720>&t=<seconds>&p=<1 paused>&w=<1 buffering>&d=<seconds>&b=<bytes>&e=<bits/s>:
// the headers a media proxy sends with the media's CDN requests now, and for
// how many seconds it may keep them, and, for the alternative player, the
// path of title-api's relay of them (proxy), which a router that knows it
// goes through instead, and the sections on its video once that is aligned
// with the primary player's (sections), or whether that is under way
// (aligning). It keeps the playback socket and the view of the
// media going and reports the quality, the position, the pause, whether the
// receiver waits for data, the duration, the bytes of segments loaded and
// their bandwidth, all optional.
func mediaHeadersHandler(source mediaHeadersSource) runtime.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request, pathParams map[string]string) {
		query := request.URL.Query()
		report := provider.PlaybackReport{Resolution: query.Get("q")}
		if report.Resolution != "" && !resolutionPattern.MatchString(report.Resolution) {
			http.Error(response, "invalid q", http.StatusBadRequest)
			return
		}
		for _, field := range []struct {
			name    string
			pattern *regexp.Regexp
			set     func(string)
		}{
			{"t", positionPattern, func(value string) {
				report.Position, _ = strconv.ParseFloat(value, 64)
				report.HasPosition = true
			}},
			{"p", flagPattern, func(value string) {
				report.Paused, report.HasPaused = value == "1", true
			}},
			{"w", flagPattern, func(value string) {
				report.Buffering, report.HasBuffering = value == "1", true
			}},
			{"d", positionPattern, func(value string) { report.Duration, _ = strconv.ParseFloat(value, 64) }},
			{"b", countPattern, func(value string) {
				report.Bytes, _ = strconv.ParseInt(value, 10, 64)
				report.HasBytes = true
			}},
			{"e", countPattern, func(value string) { report.Bandwidth, _ = strconv.ParseInt(value, 10, 64) }},
		} {
			value := query.Get(field.name)
			if value == "" {
				continue
			}
			if !field.pattern.MatchString(value) {
				http.Error(response, "invalid "+field.name, http.StatusBadRequest)
				return
			}
			field.set(value)
		}
		headers, err := source.MediaHeaders(request.Context(), pathParams["id"], report)
		if mediaError(response, err) {
			return
		}
		body := struct {
			Headers  []mediaHeader  `json:"headers"`
			TTL      int64          `json:"ttl"`
			Proxy    string         `json:"proxy,omitempty"`
			Sections []mediaSection `json:"sections,omitempty"`
			Aligning bool           `json:"aligning,omitempty"`
		}{
			Headers: make([]mediaHeader, 0, len(headers.Headers)), TTL: int64(headers.TTL / time.Second),
			Proxy: headers.Proxy, Aligning: headers.Aligning,
		}
		for _, header := range headers.Headers {
			body.Headers = append(body.Headers, mediaHeader{Name: header.Name, Value: header.Value})
		}
		for _, section := range headers.Sections {
			body.Sections = append(body.Sections, mediaSection{
				Kind: sectionKinds[section.Kind], StartSeconds: section.Start, EndSeconds: section.End, Skippable: section.Skippable,
			})
		}
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(response).Encode(body)
	}
}

// mediaSessionHandler serves DELETE /v1/media/{id}/session and POST
// /v1/media/{id}/session/end: the playback ended, so its socket says so and
// closes. Answers 204 either way.
func mediaSessionHandler(source mediaHeadersSource) runtime.HandlerFunc {
	return func(response http.ResponseWriter, _ *http.Request, pathParams map[string]string) {
		source.EndMediaSession(pathParams["id"])
		response.WriteHeader(http.StatusNoContent)
	}
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

type posterSource interface {
	Poster(ctx context.Context, rawURL string) ([]byte, string, error)
}

// posterHandler serves GET /v1/posters?url=<poster URL>. Poster images are
// behind the provider's anti-bot challenge, so they are fetched with the
// service's session; this is plain HTTP because the response is binary.
func posterHandler(source posterSource) runtime.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request, _ map[string]string) {
		body, contentType, err := source.Poster(request.Context(), request.URL.Query().Get("url"))
		switch {
		case errors.Is(err, provider.ErrInvalidPosterURL):
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		case err != nil:
			log.Printf("poster: %v", err)
			http.Error(response, "poster unavailable", http.StatusBadGateway)
			return
		}
		response.Header().Set("Content-Type", contentType)
		response.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = response.Write(body)
	}
}

func allowAllOrigins(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Access-Control-Allow-Origin", "*")
		response.Header().Set("Access-Control-Allow-Methods", "GET, DELETE, OPTIONS")
		response.Header().Set("Access-Control-Allow-Headers", "*")

		if request.Method == http.MethodOptions {
			response.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(response, request)
	})
}
