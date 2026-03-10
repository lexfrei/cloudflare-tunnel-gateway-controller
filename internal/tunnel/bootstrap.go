package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/cloudflare/cloudflared/client"
	cfdconfig "github.com/cloudflare/cloudflared/config"
	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/edgediscovery"
	"github.com/cloudflare/cloudflared/features"
	"github.com/cloudflare/cloudflared/ingress"
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

	decoded, err := base64.StdEncoding.DecodeString(tokenStr)
	if err != nil {
		return nil, errors.Wrap(errInvalidBase64, err.Error())
	}

	var token Token

	err = json.Unmarshal(decoded, &token)
	if err != nil {
		return nil, errors.Wrap(errInvalidTokenJSON, err.Error())
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
func StartTunnel(ctx context.Context, cfg Config) error {
	token, err := ParseTunnelToken(cfg.Token)
	if err != nil {
		return errors.Wrap(err, "parse tunnel token")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	orchestrator, tunnelCfg, err := buildOrchestrator(ctx, &cfg, token, logger)
	if err != nil {
		return err
	}

	connectedSignal := cfdsignal.New(make(chan struct{}))
	reconnectCh := make(chan supervisor.ReconnectSignal, defaultHAConnections)
	graceShutdownC := make(chan struct{})

	go func() {
		<-ctx.Done()
		close(graceShutdownC)
	}()

	go func() {
		select {
		case <-connectedSignal.Wait():
			logger.Info("tunnel connected to Cloudflare edge")
		case <-ctx.Done():
		}
	}()

	logger.Info("starting tunnel daemon",
		"tunnelID", token.TunnelID.String(),
		"haConnections", defaultHAConnections,
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

	tunnelCfg, orchestratorCfg, err := buildTunnelConfig(ctx, token, proxyURL, &zlog)
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
	zlog *zerolog.Logger,
) (*supervisor.TunnelConfig, *orchestration.Config, error) {
	edgeTLSConfigs, err := buildEdgeTLSConfigs()
	if err != nil {
		return nil, nil, errors.Wrap(err, "build edge TLS configs")
	}

	protocolSelector, tunnelCfg, err := buildProtocolAndClient(ctx, token, zlog)
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

	tunnelCfg.EdgeTLSConfigs = edgeTLSConfigs
	tunnelCfg.ProtocolSelector = protocolSelector
	tunnelCfg.OriginDialerService = originDialerService
	tunnelCfg.Observer = observer
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

func buildProtocolAndClient(
	ctx context.Context,
	token *Token,
	zlog *zerolog.Logger,
) (connection.ProtocolSelector, *supervisor.TunnelConfig, error) {
	protocolSelector, err := connection.NewProtocolSelector(
		connection.AutoSelectFlag,
		token.AccountTag,
		true,  // tunnelTokenProvided
		false, // needPQ
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
