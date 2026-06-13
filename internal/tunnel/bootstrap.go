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
		return errors.Wrap(err, "parse tunnel token")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	orchestrator, tunnelCfg, err := buildOrchestrator(ctx, cfg, token, logger)
	if err != nil {
		return err
	}

	connectedSignal := cfdsignal.New(make(chan struct{}))
	reconnectCh := make(chan supervisor.ReconnectSignal, defaultHAConnections)
	graceShutdownC := graceChannel(ctx, cfg.GraceShutdownC)
	tunnelCfg.GracePeriod = resolveGracePeriod(cfg.GracePeriod)

	go waitConnected(ctx, connectedSignal, logger, cfg.OnConnected)

	logger.Info("starting tunnel daemon",
		"tunnelID", token.TunnelID.String(),
		"haConnections", defaultHAConnections,
		"gracePeriod", tunnelCfg.GracePeriod,
	)

	err = supervisor.StartTunnelDaemon(
		ctx,
		tunnelCfg,
		orchestrator,
		connectedSignal,
		reconnectCh,
		graceShutdownC,
	)

	return errors.Wrap(err, "tunnel daemon")
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

	return orchestrator, tunnelCfg, nil
}

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

	observer := connection.NewObserver(zlog, zlog)
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
