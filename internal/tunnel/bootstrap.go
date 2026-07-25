package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"github.com/cloudflare/cloudflared/client"
	cfdconfig "github.com/cloudflare/cloudflared/config"
	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/edgediscovery"
	"github.com/cloudflare/cloudflared/features"
	"github.com/cloudflare/cloudflared/ingress"
	"github.com/cloudflare/cloudflared/ingress/origins"
	"github.com/cloudflare/cloudflared/orchestration"
	cfdsignal "github.com/cloudflare/cloudflared/signal"
	"github.com/cloudflare/cloudflared/supervisor"
	"github.com/cloudflare/cloudflared/tlsconfig"
)

const (
	defaultHAConnections       = 4
	defaultGracePeriod         = 30 * time.Second
	defaultRPCTimeout          = 5 * time.Second
	defaultWriteStreamTimeout  = 10 * time.Second
	defaultQUICFlowControlConn = 30 * 1024 * 1024 // 30 MB
	defaultQUICFlowControlStr  = 6 * 1024 * 1024  // 6 MB
	defaultRetries             = 5
	defaultMaxEdgeAddrRetries  = 8

	proxyVersion = "cloudflare-tunnel-gateway-proxy"
)

var (
	errEmptyToken       = errors.New("tunnel token is empty")
	errInvalidBase64    = errors.New("tunnel token is not valid base64")
	errInvalidTokenJSON = errors.New("tunnel token is not valid JSON")
	errMissingAccountID = errors.New("tunnel token missing account tag")
	errMissingTunnelID  = errors.New("tunnel token missing tunnel ID")
	errMissingSecret    = errors.New("tunnel token missing tunnel secret")
	errUnknownTLS       = errors.New("unknown TLS settings for protocol")
	errUnknownProtocol  = errors.New("unknown tunnel protocol (want auto, http2, or quic)")
)

// Config holds the configuration for starting a cloudflared tunnel.
type Config struct {
	// Token is the raw tunnel token (base64 JSON: {"a":"accountTag","s":"secret","t":"tunnelID"}).
	Token string
	// Logger for tunnel operations.
	Logger *slog.Logger
	// ProxyURL is the catch-all backend URL (e.g., "http://localhost:8080").
	// Ignored when OriginProxy is set (in-process mode).
	ProxyURL string
	// OriginProxy enables in-process delegation mode.
	// When set, traffic is routed directly to this proxy without HTTP serialization.
	// The ProxyURL field is ignored and a placeholder is used for orchestrator initialization.
	OriginProxy connection.OriginProxy
	// Protocol selects the edge transport: "" / "auto" (QUIC with HTTP/2
	// fallback), "http2", or "quic". gRPC requires "http2" because cloudflared
	// does not forward HTTP trailers over QUIC (grpc-status is dropped).
	Protocol string
	// OnConnected, when set, is invoked once when cloudflared registers its
	// first connection with the Cloudflare edge. The proxy entrypoint uses it to
	// flip readiness, so the pod reports Ready only after the tunnel can actually
	// receive traffic (before that the edge returns 530). Runs on the signal
	// goroutine; keep it non-blocking.
	OnConnected func()
	// GraceShutdownC, when non-nil, starts a graceful connector drain when
	// closed: cloudflared unregisters from the edge (which is the ONLY layer
	// that stops new requests — the edge routes to tunnel connections, not to
	// the Kubernetes Service) and then waits GracePeriod for in-flight
	// requests before the daemon exits. The ctx passed to StartTunnel MUST
	// stay alive for the whole drain: the unregister RPC and the grace wait
	// both run on it, and a cancelled ctx skips them entirely (see
	// waitForUnregister in the vendored cloudflared). When nil, the drain
	// trigger is derived from ctx.Done(), which preserves the legacy
	// hard-stop behaviour — no graceful drain is possible in that mode.
	GraceShutdownC <-chan struct{}
	// GracePeriod bounds the in-flight drain window after the connector
	// unregisters. Zero or negative selects the 30s default; values above
	// cloudflared's MaxGracePeriod (3m) are clamped to it.
	GracePeriod time.Duration
}

// Token mirrors the cloudflared token JSON structure.
type Token struct {
	AccountTag   string    `json:"a"`
	TunnelSecret []byte    `json:"s"`
	TunnelID     uuid.UUID `json:"t"`
	Endpoint     string    `json:"e,omitempty"`
}

// ParseTunnelToken decodes a base64-encoded tunnel token.
func ParseTunnelToken(tokenStr string) (*Token, error) {
	if tokenStr == "" {
		return nil, errEmptyToken
	}

	// SECURITY: never wrap with the stdlib base64/json error string — it embeds
	// the offending decoded bytes (the connector token's own content), and this
	// error is surfaced on the tenant-readable Gateway status. The static
	// sentinels already say what failed.
	decoded, err := base64.StdEncoding.DecodeString(tokenStr)
	if err != nil {
		return nil, errInvalidBase64
	}

	var token Token

	err = json.Unmarshal(decoded, &token)
	if err != nil {
		return nil, errInvalidTokenJSON
	}

	if token.AccountTag == "" {
		return nil, errMissingAccountID
	}

	if token.TunnelID == uuid.Nil {
		return nil, errMissingTunnelID
	}

	if len(token.TunnelSecret) == 0 {
		return nil, errMissingSecret
	}

	return &token, nil
}

// StartTunnel starts a cloudflared tunnel daemon with the given configuration.
// It blocks until the context is cancelled or the tunnel fails.
func StartTunnel(ctx context.Context, cfg *Config) error {
	token, err := ParseTunnelToken(cfg.Token)
	if err != nil {
		// Marked non-retryable: a malformed token fails identically on every
		// retry, so StartTunnelWithRetry must not back off and wait on it.
		return markNonRetryable(errors.Wrap(err, "parse tunnel token"))
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// attemptCtx scopes every background goroutine this call spawns --
	// directly (startWaitConnected) and indirectly, inside buildOrchestrator
	// and supervisor.StartTunnelDaemon (the feature-selector refresh loop,
	// the orchestrator's proxy-close watcher, the DNS resolver refresh loop)
	// -- to THIS SPECIFIC call, cancelled the moment StartTunnel returns.
	// Passing ctx itself to those constructors, as a single call to
	// StartTunnel always did before the retry loop existed, ties their
	// goroutines to ctx's own lifetime instead: harmless for exactly one
	// call per process, but a growing set of orphaned goroutines --
	// pinning whatever they closed over -- for every attempt that fails
	// before connecting, once StartTunnelWithRetry can call StartTunnel
	// many times across a long outage. cancelAttempt only ever fires after
	// this function's own remaining work is done (it is the last defer to
	// run), so nothing downstream observes attemptCtx as "more cancelled"
	// than ctx itself would have been at any point that mattered to it.
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()

	orchestrator, tunnelCfg, err := buildOrchestrator(attemptCtx, cfg, token, logger)
	if err != nil {
		// Marked non-retryable: every failure reachable here (protocol/TLS
		// selection, ingress parsing, orchestrator construction) is a
		// deterministic config-build error, not a network condition -- the
		// network-dependent lookups nested in this path (feature fetch,
		// protocol-percentage fetch) degrade gracefully on their own DNS
		// failure and never propagate an error through this return.
		return markNonRetryable(err)
	}

	connectedSignal := cfdsignal.New(make(chan struct{}))
	reconnectCh := make(chan supervisor.ReconnectSignal, defaultHAConnections)
	// graceChannel's fallback (deriving the drain trigger from ctx.Done()
	// when cfg.GraceShutdownC is nil) intentionally keeps using the outer
	// ctx, not attemptCtx: production always sets GraceShutdownC, so this
	// branch never runs today, and the drain trigger is conceptually an
	// operator-level signal (SIGTERM), not a per-attempt one.
	graceShutdownC := graceChannel(ctx, cfg.GraceShutdownC)

	// See startWaitConnected's own doc comment for why this must be scoped
	// per-attempt rather than tied to ctx directly.
	stopWaiting := startWaitConnected(attemptCtx, connectedSignal, logger, cfg.OnConnected)
	defer stopWaiting()

	logger.Info("starting tunnel daemon",
		"tunnelID", token.TunnelID.String(),
		"haConnections", defaultHAConnections,
		"gracePeriod", tunnelCfg.GracePeriod,
	)

	err = supervisor.StartTunnelDaemon(
		attemptCtx,
		tunnelCfg,
		orchestrator,
		connectedSignal,
		reconnectCh,
		graceShutdownC,
	)

	return errors.Wrap(err, "tunnel daemon")
}

// startWaitConnected spawns waitConnected on a context scoped to the
// returned stop function rather than directly on ctx, and returns that stop
// function for the caller to invoke when done waiting. stop() blocks until
// the spawned goroutine has actually returned.
//
// This matters because StartTunnelWithRetry calls StartTunnel repeatedly
// with the SAME long-lived ctx across every bootstrap attempt -- it is only
// cancelled at process shutdown, not between attempts. Spawning
// "go waitConnected(ctx, ...)" directly, as a single call to StartTunnel
// always did before the retry loop existed, would park the goroutine on
// ctx.Done() until the process exits whenever that attempt fails before
// connecting -- harmless for exactly one call per process lifetime, but a
// goroutine leaked per failed retry once StartTunnel can be called many
// times in the same process, unbounded for an outage long enough to matter.
// stop() -- called via defer in StartTunnel -- cancels the child context
// when that specific attempt returns, regardless of ctx's own lifetime.
//
// Blocking until the goroutine exits (rather than just cancelling and
// returning immediately) also closes a narrower race: runBootstrapAttempt
// (retry.go) reads its connected flag right after dial() returns, but
// onConnected runs on this goroutine, asynchronously with respect to
// StartTunnel's own return. Waiting for the goroutine here, in StartTunnel's
// deferred call, means any onConnected call already in flight when
// StartTunnel returns has definitely completed -- and therefore so has the
// flag write -- before runBootstrapAttempt ever reads it. What this does
// NOT close: the select inside waitConnected itself has no defined
// preference between connected.Wait() and ctx.Done() when both are ready in
// the same instant, so a connect that lands at the exact moment this
// specific attempt is giving up remains a real, if vanishingly narrow, race
// -- unreachable in practice because cloudflared's own shutdown sequence
// (supervisor.Run() unwinding every HA connection's retry budget) takes
// seconds to minutes, far longer than a goroutine needs to be scheduled.
func startWaitConnected(
	ctx context.Context,
	connected *cfdsignal.Signal,
	logger *slog.Logger,
	onConnected func(),
) func() {
	waitCtx, cancel := context.WithCancel(ctx)

	done := make(chan struct{})

	go func() {
		defer close(done)

		waitConnected(waitCtx, connected, logger, onConnected)
	}()

	return func() {
		cancel()
		<-done
	}
}

// waitConnected blocks until the tunnel registers its first connection with the
// Cloudflare edge or the context is cancelled. On connection it logs and invokes
// onConnected (when non-nil) exactly once — the proxy entrypoint passes a
// callback that flips readiness, so the pod reports Ready only after the edge is
// reachable. On context cancellation it returns without invoking onConnected, so
// a tunnel that never connects never reports ready.
func waitConnected(ctx context.Context, connected *cfdsignal.Signal, logger *slog.Logger, onConnected func()) {
	select {
	case <-connected.Wait():
		logger.Info("tunnel connected to Cloudflare edge")

		if onConnected != nil {
			onConnected()
		}
	case <-ctx.Done():
	}
}

// graceChannel returns the channel whose close triggers cloudflared's
// graceful connector drain. An explicit channel is used as-is so the caller
// can drain while keeping ctx alive; nil derives the trigger from ctx
// cancellation, preserving the legacy behaviour for callers that never drain
// (that form cannot drain gracefully — the cancelled ctx aborts the
// unregister RPC inside the vendored supervisor).
func graceChannel(ctx context.Context, explicit <-chan struct{}) <-chan struct{} {
	if explicit != nil {
		return explicit
	}

	derived := make(chan struct{})

	go func() {
		<-ctx.Done()
		close(derived)
	}()

	return derived
}

// resolveGracePeriod clamps the configured drain window: zero or negative
// selects the default, values above cloudflared's MaxGracePeriod are clamped
// to it (the edge enforces that bound on the unregister RPC anyway).
func resolveGracePeriod(period time.Duration) time.Duration {
	if period <= 0 {
		return defaultGracePeriod
	}

	if period > connection.MaxGracePeriod {
		return connection.MaxGracePeriod
	}

	return period
}

// buildOrchestrator creates the orchestration.Orchestrator and tunnel config.
// When cfg.OriginProxy is set, it enables in-process delegation mode.
func buildOrchestrator(
	ctx context.Context,
	cfg *Config,
	token *Token,
	logger *slog.Logger,
) (*orchestration.Orchestrator, *supervisor.TunnelConfig, error) {
	zlog := newZerologLogger()

	// In in-process mode, use a placeholder URL for orchestrator initialization.
	// The actual proxying is handled by OverrideProxy, bypassing ingress rules entirely.
	proxyURL := cfg.ProxyURL
	if cfg.OriginProxy != nil {
		proxyURL = "http://localhost:0"
	}

	tunnelCfg, orchestratorCfg, err := buildTunnelConfig(ctx, token, proxyURL, cfg.Protocol, &zlog)
	if err != nil {
		return nil, nil, errors.Wrap(err, "build tunnel config")
	}

	orchestrator, err := orchestration.NewOrchestrator(
		ctx,
		orchestratorCfg,
		nil, // tags
		nil, // internal rules
		&zlog,
	)
	if err != nil {
		return nil, nil, errors.Wrap(err, "create orchestrator")
	}

	if cfg.OriginProxy != nil {
		orchestrator.OverrideProxy = cfg.OriginProxy

		logger.Info("in-process origin proxy enabled")
	}

	// Apply the operator-configured drain window over buildProtocolAndClient's
	// default here, where it is observable in the returned tunnelCfg — keeping
	// it out of StartTunnel (which returns only an error) so a regression
	// dropping it fails a test instead of silently ignoring PROXY_GRACE_PERIOD.
	tunnelCfg.GracePeriod = resolveGracePeriod(cfg.GracePeriod)

	return orchestrator, tunnelCfg, nil
}

// sharedObserver returns a process-lifetime connection.Observer, constructed
// once on first use.
//
// connection.NewObserver spawns an unconditional background goroutine
// (dispatchEvents) with no way to stop it -- no ctx parameter, no Close()
// method exposed by the vendored library -- unlike buildOrchestrator's other
// two per-attempt goroutine hazards (the feature-selector refresh loop, the
// orchestrator's proxy-close watcher), which are closed by threading
// StartTunnel's per-attempt context through instead (see attemptCtx in
// StartTunnel). A fresh Observer per bootstrap attempt, as buildTunnelConfig
// always constructed before the retry loop existed, would leak that
// goroutine -- and everything it closes over via registered EventSinks --
// once per failed attempt, unboundedly for an outage long enough to matter.
//
// Reusing one Observer for the process's lifetime bounds this to exactly
// one goroutine, trading it for a smaller ongoing cost: Observer exposes no
// way to unregister a sink, so supervisor.NewSupervisor's per-attempt
// *tunnelstate.ConnTracker (registered as a sink on every call, via
// NewConnAwareLogger) accumulates in the shared Observer's internal sinks
// slice across every attempt instead of being freed, and dispatchEvents
// walks the whole slice on every forwarded event -- so both memory and
// per-event dispatch cost grow with attempt count. Per attempt that cost is
// small and fixed (a stale ConnTracker only receives and discards forwarded
// events -- it holds no dial or connection state of its own), but the TOTAL
// is NOT bounded by this fix alone: for a bootstrap loop that retries
// indefinitely (see errNonRetryableStart's doc comment on a well-formed but
// edge-rejected token), sinks grows for as long as that condition persists,
// unboundedly in principle. This is a real, disclosed trade-off, not a
// closed one -- tracked in #606.
//
// The proper fix is a vendor patch giving Observer a Close()/UnregisterSink;
// deliberately out of scope here, since it touches a separate repository
// (the cloudflared fork). Sharing the OTHER per-attempt state Observer's
// sinks carry -- reusing tunnelstate.ConnTracker itself instead of
// registering a fresh one per attempt -- is not a safe alternative either:
// ConnTracker.HasConnectedWith feeds the vendored supervisor's own
// protocol-fallback decision (supervisor/tunnel.go's serveTunnel), so
// sharing it would leak one attempt's connection history into another
// attempt's control flow, trading a bounded memory cost for an unbounded
// correctness one.
//
//nolint:gochecknoglobals // see doc comment above: one Observer must outlive any single StartTunnel call
var sharedObserver = sync.OnceValue(func() *connection.Observer {
	zlog := newZerologLogger()

	return connection.NewObserver(&zlog, &zlog)
})

func buildTunnelConfig(
	ctx context.Context,
	token *Token,
	proxyURL string,
	protocol string,
	zlog *zerolog.Logger,
) (*supervisor.TunnelConfig, *orchestration.Config, error) {
	edgeTLSConfigs, err := buildEdgeTLSConfigs()
	if err != nil {
		return nil, nil, errors.Wrap(err, "build edge TLS configs")
	}

	protocolSelector, tunnelCfg, err := buildProtocolAndClient(ctx, token, protocol, zlog)
	if err != nil {
		return nil, nil, err
	}

	ingressRules, err := buildCatchAllIngress(proxyURL)
	if err != nil {
		return nil, nil, errors.Wrap(err, "build ingress rules")
	}

	warpRouting := ingress.NewWarpRoutingConfig(&cfdconfig.WarpRoutingConfig{})

	originDialerService := ingress.NewOriginDialer(ingress.OriginConfig{
		DefaultDialer: ingress.NewDialer(warpRouting),
	}, zlog)

	observer := sharedObserver()
	dnsService := origins.NewDNSResolverService(originDialerService, zlog, origins.NewMetrics(prometheus.DefaultRegisterer))

	tunnelCfg.EdgeTLSConfigs = edgeTLSConfigs
	tunnelCfg.ProtocolSelector = protocolSelector
	tunnelCfg.OriginDialerService = originDialerService
	tunnelCfg.Observer = observer
	tunnelCfg.OriginDNSService = dnsService
	tunnelCfg.NamedTunnel = &connection.TunnelProperties{
		Credentials: connection.Credentials{
			AccountTag:   token.AccountTag,
			TunnelSecret: token.TunnelSecret,
			TunnelID:     token.TunnelID,
			Endpoint:     token.Endpoint,
		},
	}

	orchestratorCfg := &orchestration.Config{
		Ingress:             &ingressRules,
		WarpRouting:         warpRouting,
		OriginDialerService: originDialerService,
	}

	return tunnelCfg, orchestratorCfg, nil
}

// resolveProtocolFlag maps the operator-facing protocol value onto the
// cloudflared protocol-selector flag. "" and "auto" keep cloudflared's default
// (QUIC with HTTP/2 fallback); "http2" and "quic" pin a single transport. gRPC
// requires "http2" — cloudflared does not forward HTTP trailers over QUIC.
func resolveProtocolFlag(protocol string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "", connection.AutoSelectFlag:
		return connection.AutoSelectFlag, nil
	case connection.HTTP2.String():
		return connection.HTTP2.String(), nil
	case connection.QUIC.String():
		return connection.QUIC.String(), nil
	default:
		return "", errors.Wrapf(errUnknownProtocol, "%q", protocol)
	}
}

func buildProtocolAndClient(
	ctx context.Context,
	token *Token,
	protocol string,
	zlog *zerolog.Logger,
) (connection.ProtocolSelector, *supervisor.TunnelConfig, error) {
	protocolFlag, err := resolveProtocolFlag(protocol)
	if err != nil {
		return nil, nil, err
	}

	protocolSelector, err := connection.NewProtocolSelector(
		protocolFlag,
		token.AccountTag,
		true, // tunnelTokenProvided
		edgediscovery.ProtocolPercentage,
		connection.ResolveTTL,
		zlog,
	)
	if err != nil {
		return nil, nil, errors.Wrap(err, "create protocol selector")
	}

	featureSelector, err := features.NewFeatureSelector(ctx, token.AccountTag, nil, false, zlog)
	if err != nil {
		return nil, nil, errors.Wrap(err, "create feature selector")
	}

	clientCfg, err := client.NewConfig(proxyVersion, runtime.GOARCH, featureSelector)
	if err != nil {
		return nil, nil, errors.Wrap(err, "create client config")
	}

	tunnelCfg := &supervisor.TunnelConfig{
		ClientConfig:                        clientCfg,
		GracePeriod:                         defaultGracePeriod,
		CloseConnOnce:                       &sync.Once{},
		Region:                              token.Endpoint,
		HAConnections:                       defaultHAConnections,
		Log:                                 zlog,
		LogTransport:                        zlog,
		ReportedVersion:                     proxyVersion,
		Retries:                             defaultRetries,
		MaxEdgeAddrRetries:                  defaultMaxEdgeAddrRetries,
		RPCTimeout:                          defaultRPCTimeout,
		WriteStreamTimeout:                  defaultWriteStreamTimeout,
		QUICConnectionLevelFlowControlLimit: defaultQUICFlowControlConn,
		QUICStreamLevelFlowControlLimit:     defaultQUICFlowControlStr,
	}

	return protocolSelector, tunnelCfg, nil
}

func buildEdgeTLSConfigs() (map[connection.Protocol]*tls.Config, error) {
	configs := make(map[connection.Protocol]*tls.Config, len(connection.ProtocolList))

	rootCAs, err := buildRootCAPool()
	if err != nil {
		return nil, err
	}

	for _, proto := range connection.ProtocolList {
		tlsSettings := proto.TLSSettings()
		if tlsSettings == nil {
			return nil, errors.Wrapf(errUnknownTLS, "%s", proto)
		}

		cfg := &tls.Config{
			RootCAs:    rootCAs,
			ServerName: tlsSettings.ServerName,
			MinVersion: tls.VersionTLS12,
		}

		if len(tlsSettings.NextProtos) > 0 {
			cfg.NextProtos = tlsSettings.NextProtos
		}

		configs[proto] = cfg
	}

	return configs, nil
}

func buildRootCAPool() (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}

	cfCerts, err := tlsconfig.GetCloudflareRootCA()
	if err != nil {
		return nil, errors.Wrap(err, "load Cloudflare root CAs")
	}

	for _, cert := range cfCerts {
		pool.AddCert(cert)
	}

	return pool, nil
}

func buildCatchAllIngress(proxyURL string) (ingress.Ingress, error) {
	cfg := &cfdconfig.Configuration{
		Ingress: []cfdconfig.UnvalidatedIngressRule{
			{
				Service: proxyURL,
			},
		},
	}

	result, err := ingress.ParseIngress(cfg)
	if err != nil {
		return ingress.Ingress{}, errors.Wrap(err, "parse catch-all ingress")
	}

	return result, nil
}

// newZerologLogger creates a zerolog.Logger for cloudflared components.
func newZerologLogger() zerolog.Logger {
	return zerolog.New(zerolog.NewConsoleWriter()).
		Level(zerolog.InfoLevel).
		With().
		Str("component", "cloudflared").
		Timestamp().
		Logger()
}
