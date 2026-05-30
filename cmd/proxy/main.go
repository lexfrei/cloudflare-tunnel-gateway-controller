package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
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
	// defaultStartupProtocolWait bounds how long the proxy waits for the
	// controller's first config push before dialing the edge, when the
	// configured transport is auto/unset and the proxy must learn whether a
	// GRPCRoute is present (gRPC needs http2). Overridable via
	// PROXY_TUNNEL_PROTOCOL_WAIT. The wait only delays auto deployments, and
	// only until the first push (or this cap) — explicit http2/quic dial
	// immediately.
	defaultStartupProtocolWait = 30 * time.Second
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
	proxyHandler := proxy.NewHandler(router, handlerOptions(logger)...)
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

	// Resolve the edge transport before dialing. PROXY_TUNNEL_PROTOCOL selects
	// it (auto|http2|quic, default auto). For auto/unset this waits briefly for
	// the controller's first config push so the proxy can upgrade to http2 when
	// a GRPCRoute is present — cloudflared drops HTTP trailers over QUIC, so gRPC
	// needs http2. Explicit http2/quic dial immediately without waiting.
	effectiveProtocol := proxy.ResolveStartupProtocol(
		ctx,
		os.Getenv("PROXY_TUNNEL_PROTOCOL"),
		router.FirstConfigLoaded(),
		startupProtocolWait(logger),
		logger,
	)

	logger.Info("starting cloudflared tunnel with in-process proxy", "protocol", effectiveProtocol)

	err := tunnel.StartTunnel(ctx, tunnel.Config{
		Token:       token,
		Logger:      logger,
		OriginProxy: originProxy,
		Protocol:    effectiveProtocol,
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
	proxyHandler := proxy.NewHandler(router, handlerOptions(logger)...)
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

// startupProtocolWait reads PROXY_TUNNEL_PROTOCOL_WAIT and returns how long the
// proxy should wait for the first config push before dialing the edge on an
// auto/unset transport. Unset, empty, malformed, zero, or negative values fall
// back to defaultStartupProtocolWait: a non-positive wait would defeat the
// gRPC-aware upgrade (the proxy would time out immediately and dial auto even
// when a GRPCRoute is about to arrive), so only a positive override is honoured.
func startupProtocolWait(logger *slog.Logger) time.Duration {
	if d := parseEnvDuration(logger, "PROXY_TUNNEL_PROTOCOL_WAIT"); d > 0 {
		return d
	}

	return defaultStartupProtocolWait
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

// accessLogHandlerOption translates PROXY_ACCESS_LOG_ENABLED and
// PROXY_ACCESS_LOG_SAMPLING_RATE into a proxy.HandlerOption.
//
// PROXY_ACCESS_LOG_ENABLED gates the whole feature. Unset / "" / "0" /
// "false" → disabled (no option emitted, zero-cost on hot path).
// "1" / "true" → enabled.
//
// PROXY_ACCESS_LOG_SAMPLING_RATE is parsed as a float64 in [0,1].
// Unset → default 1.0 (log everything). Unparseable → WARN and fall
// back to 1.0. Out-of-range values pass through; proxy.WithAccessLog
// + shouldSampleAccessLog clamp them so operator typos degrade to
// "always log" rather than "silently never log".
//
// Same "WARN + safe-default" pattern as parseEnvDuration -- proxy
// MUST start even with a malformed tunable; the WARN ensures the
// typo doesn't hide.
func accessLogHandlerOption(logger *slog.Logger) proxy.HandlerOption {
	if !accessLogEnabled() {
		return nil
	}

	return proxy.WithAccessLog(logger, parseAccessLogSamplingRate(logger))
}

// accessLogStripQueryOption translates PROXY_ACCESS_LOG_STRIP_QUERY
// into proxy.WithAccessLogStripQuery. Truthy forms: 1/true (case-
// insensitive, trimmed). Returns nil when access logging itself is
// disabled (strip is meaningless without emission) or when the
// strip toggle is off (default-false; emitting WithAccessLogStripQuery(false)
// would still be a correct no-op but adds an unused option entry).
func accessLogStripQueryOption() proxy.HandlerOption {
	if !accessLogEnabled() {
		return nil
	}

	if !isTruthyEnv("PROXY_ACCESS_LOG_STRIP_QUERY") {
		return nil
	}

	return proxy.WithAccessLogStripQuery(true)
}

// truthyEnvOne and truthyEnvTrue are the two accepted shell-flag /
// YAML-bool forms for env vars that toggle proxy features. Hoisted
// to constants so goconst doesn't trip across the per-feature
// accessLogEnabled / accessLogStripQueryOption / etc. callers, and
// so a future "accept TRUE only" rename happens in one place.
const (
	truthyEnvOne  = "1"
	truthyEnvTrue = "true"
)

// isTruthyEnv reports whether an env-var raw value reads as boolean
// true for the proxy's toggle convention. Trimmed + case-insensitive
// match against the truthyEnv* constants.
func isTruthyEnv(name string) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))

	return raw == truthyEnvOne || raw == truthyEnvTrue
}

// accessLogEnabled reports whether PROXY_ACCESS_LOG_ENABLED requests
// the feature. The "1" / "true" forms are accepted (case-insensitive)
// so both shell-flag (`PROXY_ACCESS_LOG_ENABLED=1`) and YAML-bool
// (`enabled: true`) styles work without surprise.
func accessLogEnabled() bool {
	return isTruthyEnv("PROXY_ACCESS_LOG_ENABLED")
}

// parseAccessLogSamplingRate reads PROXY_ACCESS_LOG_SAMPLING_RATE,
// defaulting to 1.0 on unset / unparseable input. WARN logs surface
// the typo so the operator notices.
func parseAccessLogSamplingRate(logger *slog.Logger) float64 {
	raw := strings.TrimSpace(os.Getenv("PROXY_ACCESS_LOG_SAMPLING_RATE"))
	if raw == "" {
		return 1.0
	}

	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		logger.Warn("PROXY_ACCESS_LOG_SAMPLING_RATE failed to parse -- defaulting to 1.0",
			"value", raw, "error", err)

		return 1.0
	}

	return parsed
}

// handlerOptions composes every env-driven proxy.HandlerOption -- WS
// tunables + access log -- into the slice passed to proxy.NewHandler.
// Nil entries (e.g. accessLogHandlerOption when disabled) are
// filtered so proxy.NewHandler doesn't see a nil HandlerOption (it
// would panic invoking it).
func handlerOptions(logger *slog.Logger) []proxy.HandlerOption {
	opts := wsHandlerOptions(logger)

	if accessOpt := accessLogHandlerOption(logger); accessOpt != nil {
		opts = append(opts, accessOpt)
	}

	if stripOpt := accessLogStripQueryOption(); stripOpt != nil {
		opts = append(opts, stripOpt)
	}

	return opts
}
