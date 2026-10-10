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

	moviev1 "movie-api/api/movie/v1"
	"movie-api/internal/grpcserver"
	"movie-api/internal/provider"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

const (
	defaultBaseURL  = "https://go.\x6c\x6f\x72\x64\x66\x69\x6c\x6d5.pro"
	defaultGRPCAddr = ":50051"
	defaultHTTPAddr = ":8080"
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
	grpcAddress := envOrDefault("GRPC_ADDR", defaultGRPCAddr)
	httpAddress := envOrDefault("HTTP_ADDR", defaultHTTPAddr)

	client, err := provider.NewClient(provider.Config{
		BaseURL:         baseURL,
		RefreshInterval: 5 * time.Minute,
		UserAgent:       providerUserAgent,
	})
	if err != nil {
		return err
	}
	log.Printf("initializing provider anti-bot session for %s", baseURL)
	if err := client.Start(ctx); err != nil {
		return err
	}

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
	moviev1.RegisterMovieServiceServer(grpcServer, service)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("movie.v1.MovieService", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	reflection.Register(grpcServer)

	gateway := runtime.NewServeMux()
	if err := moviev1.RegisterMovieServiceHandlerServer(ctx, gateway, service); err != nil {
		_ = grpcListener.Close()
		_ = httpListener.Close()
		return err
	}
	httpServer := &http.Server{
		Handler:           allowAllOrigins(gateway),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf(
		"gRPC server listening on %s; HTTP gateway listening on %s; movie_id prefix for %s is %s",
		grpcAddress,
		httpAddress,
		baseURL,
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

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
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
