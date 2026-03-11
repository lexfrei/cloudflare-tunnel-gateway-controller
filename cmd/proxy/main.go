package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
)

const (
	defaultConfigAddr = ":8081"
	defaultProxyAddr  = ":8080"
	readHeaderTimeout = 10 * time.Second
	writeTimeout      = 60 * time.Second
	shutdownTimeout   = 30 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	slog.SetDefault(logger)

	tunnelToken := os.Getenv("TUNNEL_TOKEN")

	if tunnelToken != "" {
		runTunnelMode(logger, tunnelToken)
	} else {
		runStandaloneMode(logger)
	}
}

// runTunnelMode starts the proxy with cloudflared tunnel integration.
// Traffic flows in-process: cloudflared → GatewayOriginProxy → proxy.Handler.
// No localhost HTTP server is needed for proxying.
func runTunnelMode(logger *slog.Logger, token string) {
	configAddr := envOrDefault("PROXY_CONFIG_ADDR", defaultConfigAddr)

	router := proxy.NewRouter()
	proxyHandler := proxy.NewHandler(router)

	authToken := os.Getenv("PROXY_AUTH_TOKEN")
	configServer := newServer(configAddr, proxy.NewConfigAPI(router, authToken))

	// Create in-process origin proxy — traffic flows directly from cloudflared
	// to our handler without HTTP serialization or localhost TCP hop.
	originProxy := tunnel.NewGatewayOriginProxy(proxyHandler, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go handleSignals(logger, cancel)

	go func() {
		logger.Info("starting config API server", "addr", configAddr)

		err := configServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("config API server error", "error", err)
			cancel()
		}
	}()

	logger.Info("starting cloudflared tunnel with in-process proxy")

	err := tunnel.StartTunnel(ctx, tunnel.Config{
		Token:       token,
		Logger:      logger,
		OriginProxy: originProxy,
	})
	if err != nil {
		logger.Error("tunnel error", "error", err)
	}

	gracefulShutdown(logger, configServer)
}

// runStandaloneMode starts the proxy as a standalone HTTP server.
// Used for local development and testing without a tunnel.
func runStandaloneMode(logger *slog.Logger) {
	configAddr := envOrDefault("PROXY_CONFIG_ADDR", defaultConfigAddr)
	proxyAddr := envOrDefault("PROXY_ADDR", defaultProxyAddr)

	router := proxy.NewRouter()

	authToken := os.Getenv("PROXY_AUTH_TOKEN")
	configServer := newServer(configAddr, proxy.NewConfigAPI(router, authToken))
	proxyServer := newProxyServer(proxyAddr, proxy.NewHandler(router))

	errChan := make(chan error, 2)

	go func() {
		logger.Info("starting config API server", "addr", configAddr)

		errChan <- configServer.ListenAndServe()
	}()

	go func() {
		logger.Info("starting proxy server", "addr", proxyAddr)

		errChan <- proxyServer.ListenAndServe()
	}()

	startupFailure := waitForShutdown(logger, errChan)
	gracefulShutdown(logger, configServer, proxyServer)
	drainErrors(logger, errChan)

	if startupFailure {
		os.Exit(1)
	}
}

func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
	}
}

// newProxyServer creates an HTTP server without WriteTimeout.
// Per-route timeouts are enforced via context deadlines, so a global
// WriteTimeout would prematurely kill long-running proxied responses.
func newProxyServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}
}

func handleSignals(logger *slog.Logger, cancel context.CancelFunc) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	sig := <-sigChan

	signal.Stop(sigChan)

	logger.Info("received signal, shutting down", "signal", sig)

	cancel()
}

// waitForShutdown blocks until a termination signal is received or a server
// error is read from errChan. It returns true when the trigger was a server
// error (i.e. a startup failure), so the caller can exit with a non-zero code.
// All remaining errors in errChan are drained and logged before returning.
func waitForShutdown(logger *slog.Logger, errChan <-chan error) bool {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	var startupFailure bool

	select {
	case sig := <-sigChan:
		logger.Info("received signal, shutting down", "signal", sig)
	case err := <-errChan:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)

			startupFailure = true
		}
	}

	signal.Stop(sigChan)

	return startupFailure
}

func gracefulShutdown(logger *slog.Logger, servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	for _, server := range servers {
		err := server.Shutdown(ctx)
		if err != nil {
			logger.Error("server shutdown error", "addr", server.Addr, "error", err)
		}
	}

	logger.Info("shutdown complete")
}

// drainErrors reads remaining errors from errChan after servers have been shut down.
// Must be called AFTER gracefulShutdown to avoid blocking on still-running servers.
func drainErrors(logger *slog.Logger, errChan <-chan error) {
	for {
		select {
		case err := <-errChan:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("server error", "error", err)
			}
		default:
			return
		}
	}
}

func envOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return defaultValue
}
