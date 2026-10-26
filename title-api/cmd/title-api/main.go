package main

import (
	"context"
	"encoding/json"
	"errors"
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

// mediaError answers a failed master or headers request.
func mediaError(response http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, provider.ErrMasterNotFound):
		http.Error(response, err.Error(), http.StatusNotFound)
	// A 403 reaches the receiver as one, and it does not retry that.
	case errors.Is(err, provider.ErrCDNBlocked):
		http.Error(response, err.Error(), http.StatusForbidden)
	default:
		http.Error(response, err.Error(), http.StatusBadGateway)
	}
	return true
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

// mediaHeadersHandler serves GET
// /v1/media/{id}/headers?q=<720>&t=<seconds>&p=<1 paused>&d=<seconds>&b=<bytes>&e=<bits/s>:
// the headers a media proxy sends with the media's CDN requests now, and for
// how many seconds it may keep them. It keeps the playback socket and the
// view of the media going and reports the quality, the position, the pause,
// the duration, the bytes of segments loaded and their bandwidth, all
// optional.
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
			Headers []mediaHeader `json:"headers"`
			TTL     int64         `json:"ttl"`
		}{Headers: make([]mediaHeader, 0, len(headers.Headers)), TTL: int64(headers.TTL / time.Second)}
		for _, header := range headers.Headers {
			body.Headers = append(body.Headers, mediaHeader{Name: header.Name, Value: header.Value})
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
