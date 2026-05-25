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
	defaultConfigAddr  = ":8081"
	defaultProxyAddr   = ":8080"
	readHeaderTimeout  = 10 * time.Second
	readTimeout        = 5 * time.Minute
	configReadTimeout  = 60 * time.Second
	configWriteTimeout = 60 * time.Second
	shutdownTimeout    = 30 * time.Second
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
	proxyHandler := proxy.NewHandler(router, wsHandlerOptions(logger)...)
	router.SetHandler(proxyHandler)

	authToken := os.Getenv("PROXY_AUTH_TOKEN")
	warnIfNoAuth(logger, authToken)

	configServer := newServer(configAddr, proxy.NewConfigAPI(router, authToken))

	// Create in-process origin proxy — traffic flows directly from cloudflared
	// to our handler without HTTP serialization or localhost TCP hop.
	originProxy := tunnel.NewGatewayOriginProxy(proxyHandler, logger)

	// Register signal handler before starting tunnel to prevent signal loss
	// during startup. The goroutine waits for either a signal or context cancellation.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go handleSignals(ctx, logger, cancel, sigChan)

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

	gracefulShutdown(logger, configServer)

	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("tunnel error", "error", err)
		cancel()
		os.Exit(1) //nolint:gocritic // cancel() called explicitly above
	}
}

// runStandaloneMode starts the proxy as a standalone HTTP server.
// Used for local development and testing without a tunnel.
func runStandaloneMode(logger *slog.Logger) {
	configAddr := envOrDefault("PROXY_CONFIG_ADDR", defaultConfigAddr)
	proxyAddr := envOrDefault("PROXY_ADDR", defaultProxyAddr)

	router := proxy.NewRouter()
	proxyHandler := proxy.NewHandler(router, wsHandlerOptions(logger)...)
	router.SetHandler(proxyHandler)

	authToken := os.Getenv("PROXY_AUTH_TOKEN")
	warnIfNoAuth(logger, authToken)

	configServer := newServer(configAddr, proxy.NewConfigAPI(router, authToken))
	proxyServer := newProxyServer(proxyAddr, proxyHandler)

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
		ReadTimeout:       configReadTimeout,
		WriteTimeout:      configWriteTimeout,
	}
}

// newProxyServer creates an HTTP server without WriteTimeout.
// Per-route timeouts are enforced via context deadlines, so a global
// WriteTimeout would prematurely kill long-running proxied responses.
// ReadTimeout is set to a generous 5 minutes to protect against slow-loris
// attacks while still allowing large request bodies.
func newProxyServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
	}
}

func handleSignals(ctx context.Context, logger *slog.Logger, cancel context.CancelFunc, sigChan <-chan os.Signal) {
	select {
	case sig := <-sigChan:
		logger.Info("received signal, shutting down", "signal", sig)

		cancel()
	case <-ctx.Done():
		// Context was cancelled by another path (e.g., tunnel exit).
		// Nothing to do — just let the goroutine exit cleanly.
	}
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

func warnIfNoAuth(logger *slog.Logger, authToken string) {
	if authToken == "" {
		logger.Warn("config API running WITHOUT authentication -- set PROXY_AUTH_TOKEN for production use")
	}
}

func envOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return defaultValue
}

// parseWSEnvDurations reads PROXY_WS_DIAL_TIMEOUT and
// PROXY_WS_HANDSHAKE_TIMEOUT from the environment and parses them as
// time.Duration. Returns zero for unset / empty / unparseable values
// so a downstream proxy.With* helper (which gates on > 0) treats them
// as "no override". Logs a WARN on parse failure so a typo'd env var
// doesn't silently fall back to the default with no diagnostic.
//
// Split from wsHandlerOptions so the env-var-to-duration translation
// can be unit-tested directly without the HandlerOption indirection;
// callers that need the proxy plumbing use wsHandlerOptions, which
// composes this helper with the proxy.With* constructors.
func parseWSEnvDurations(logger *slog.Logger) (time.Duration, time.Duration) {
	dialTimeout := parseEnvDuration(logger, "PROXY_WS_DIAL_TIMEOUT")
	handshakeTimeout := parseEnvDuration(logger, "PROXY_WS_HANDSHAKE_TIMEOUT")

	return dialTimeout, handshakeTimeout
}

// parseEnvDuration is the per-env-var primitive: zero on unset / empty
// / parse failure, parsed duration otherwise. Note that negative
// durations parse successfully (time.ParseDuration accepts "-5s") and
// pass through as-is; the downstream > 0 gate in wsHandlerOptions
// drops them so the proxy defaults still apply. WARN on parse failure
// names both the env var and the offending value.
//
// "WARN + zero" rather than fail-loud is deliberate for these tunables:
// the proxy MUST start with the package defaults if the operator's
// override is malformed (a misconfigured timeout shouldn't kill the
// whole tunnel). The WARN ensures the typo doesn't hide; the
// downstream zero-fallback in wsHandlerOptions ensures the
// previously-working behaviour stays the same. For non-tunable env
// vars (credentials, tunnel ID), a parse failure should fail-loud --
// don't copy this swallow pattern there.
func parseEnvDuration(logger *slog.Logger, name string) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return 0
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		logger.Warn(name+" failed to parse -- keeping default",
			"value", raw, "error", err)

		return 0
	}

	return parsed
}

// wsHandlerOptions composes parseWSEnvDurations with the proxy.With*
// option constructors. Zero durations (unset / unparseable env vars)
// flow through as no-op options because the With* helpers drop them.
func wsHandlerOptions(logger *slog.Logger) []proxy.HandlerOption {
	dialTimeout, handshakeTimeout := parseWSEnvDurations(logger)

	var opts []proxy.HandlerOption

	if dialTimeout > 0 {
		opts = append(opts, proxy.WithWSDialTimeout(dialTimeout))
	}

	if handshakeTimeout > 0 {
		opts = append(opts, proxy.WithWSHandshakeReadTimeout(handshakeTimeout))
	}

	return opts
}
