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

	configServer := newServer(configAddr, proxy.NewConfigAPI(router))

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

	configServer := newServer(configAddr, proxy.NewConfigAPI(router))
	proxyServer := newServer(proxyAddr, proxy.NewHandler(router))

	errChan := make(chan error, 2)

	go func() {
		logger.Info("starting config API server", "addr", configAddr)

		errChan <- configServer.ListenAndServe()
	}()

	go func() {
		logger.Info("starting proxy server", "addr", proxyAddr)

		errChan <- proxyServer.ListenAndServe()
	}()

	waitForShutdown(logger, errChan)
	gracefulShutdown(logger, configServer, proxyServer)
}

func newServer(addr string, handler http.Handler) *http.Server {
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
	logger.Info("received signal, shutting down", "signal", sig)

	cancel()
}

func waitForShutdown(logger *slog.Logger, errChan <-chan error) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigChan:
		logger.Info("received signal, shutting down", "signal", sig)
	case err := <-errChan:
		logger.Error("server error", "error", err)
	}
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

func envOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return defaultValue
}
