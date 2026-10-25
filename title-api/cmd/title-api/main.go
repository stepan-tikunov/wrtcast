package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
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
	if err := gateway.HandlePath(http.MethodGet, "/v1/posters", posterHandler(client)); err != nil {
		_ = grpcListener.Close()
		_ = httpListener.Close()
		return err
	}
	if err := gateway.HandlePath(http.MethodGet, "/v1/media/{id}/master.m3u8", masterPlaylistHandler(client)); err != nil {
		_ = grpcListener.Close()
		_ = httpListener.Close()
		return err
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
		if errors.Is(err, provider.ErrMasterNotFound) {
			http.Error(response, err.Error(), http.StatusNotFound)
			return
		}
		// A 403 reaches the receiver as one, and it does not retry that.
		if errors.Is(err, provider.ErrCDNBlocked) {
			http.Error(response, err.Error(), http.StatusForbidden)
			return
		}
		if err != nil {
			http.Error(response, err.Error(), http.StatusBadGateway)
			return
		}
		response.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		response.Header().Set("Cache-Control", "private, max-age=300")
		_, _ = response.Write(body)
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
		response.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		response.Header().Set("Access-Control-Allow-Headers", "*")

		if request.Method == http.MethodOptions {
			response.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(response, request)
	})
}
