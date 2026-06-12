package cmd

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/controller"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/dns"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tracing"
)

// tracingShutdownTimeout bounds the OTLP exporter flush on shutdown.
const tracingShutdownTimeout = 30 * time.Second

//nolint:gochecknoglobals // set by SetVersion from main
var (
	version = "development"
	gitsha  = "development"
)

func SetVersion(ver, sha string) {
	version = ver
	gitsha = sha
}

//nolint:gochecknoglobals // cobra command pattern
var rootCmd = &cobra.Command{
	Use:   "cloudflare-tunnel-gateway-controller",
	Short: "Kubernetes Gateway API controller for Cloudflare Tunnel",
	Long: `A Kubernetes controller that implements the Gateway API for Cloudflare Tunnel.
It watches Gateway and HTTPRoute resources and configures Cloudflare Tunnel
ingress rules accordingly.

Configuration is read from GatewayClassConfig CRD referenced by GatewayClass.
Cloudflare credentials and tunnel settings are stored in Kubernetes Secrets
and referenced from the GatewayClassConfig resource.`,
	RunE:          runController,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().String("log-level", "info", "Log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().String("log-format", "json", "Log format (json, text)")

	rootCmd.Flags().String("cluster-domain", "", "Kubernetes cluster domain (auto-detected if not set)")
	rootCmd.Flags().String("controller-name", "cf.k8s.lex.la/tunnel-controller", "Controller name for GatewayClass")
	rootCmd.Flags().String("metrics-addr", ":8080", "Address for metrics endpoint")
	rootCmd.Flags().String("health-addr", ":8081", "Address for health probe endpoint")

	// Leader election flags
	rootCmd.Flags().Bool("leader-elect", false, "Enable leader election for high availability")
	rootCmd.Flags().String("leader-election-namespace", "", "Namespace for leader election lease (defaults to controller namespace)")
	rootCmd.Flags().String("leader-election-name", "cloudflare-tunnel-gateway-controller-leader", "Name of the leader election lease")

	// L7 proxy data plane configuration (--proxy-endpoints is required in v3)
	rootCmd.Flags().StringSlice("proxy-endpoints", nil, "Proxy config API endpoints (e.g., http://proxy-0:8081,http://proxy-1:8081). Required; the chart wires this to the proxy's headless Service.")
	rootCmd.Flags().String("proxy-auth-token", "", "Bearer token for authenticating proxy config push requests")
	rootCmd.Flags().String("proxy-token-secret", "", "Tunnel-token Secret to watch in `<namespace>/<name>` form; when set, the controller rolls the proxy Deployment whenever the Secret data changes (issue #114). Empty disables the watcher.")
	rootCmd.Flags().String("proxy-deployment-label", "", "Label selector identifying the proxy Deployment(s) to roll on tunnel-token change, in `key=value` form. Defaults to `app.kubernetes.io/component=proxy` (matches the chart).")
	rootCmd.Flags().String("tunnel-protocol", "auto", "The proxy's configured edge transport (auto|http2|quic); used to warn when GRPCRoutes are present on an explicit quic tunnel, which cannot carry gRPC trailers (auto/unset is upgraded to http2 by the proxy).")

	// Hostname-ownership enforcement (issue #475, controller-side layer).
	rootCmd.Flags().Bool("hostname-ownership-enforce", false, "Enforce per-namespace hostname ownership in the controller: routes whose hostnames fall outside their namespace's allowed-suffix label are rejected and never programmed. Complements (and is independent of) the chart's ValidatingAdmissionPolicy.")
	rootCmd.Flags().String("hostname-ownership-label-key", "cf.k8s.lex.la/hostname-suffix", "Namespace label carrying the tenant's allowed hostname suffix.")
	rootCmd.Flags().String("hostname-ownership-namespace-selector", "", "Label selector scoping which namespaces are policed (kubectl syntax). Empty polices every namespace (fail-closed).")

	// Distributed tracing (OpenTelemetry). Off by default. When enabled, the
	// controller instruments its outbound Cloudflare API and proxy config-push
	// clients and exports spans over OTLP/gRPC.
	rootCmd.Flags().Bool("tracing-enabled", false, "Enable OpenTelemetry distributed tracing for the controller.")
	rootCmd.Flags().String("tracing-endpoint", "", "OTLP/gRPC collector endpoint. Bare host:port uses plaintext gRPC; an http:// or https:// prefix selects plaintext vs TLS. Empty defers to the standard OTEL_EXPORTER_OTLP_ENDPOINT variables.")
	rootCmd.Flags().Float64("tracing-sample-rate", 1.0, "Head-sampling probability in [0,1], applied at the trace root via ParentBased(TraceIDRatioBased).")

	// Deprecated: --gateway-class-name is no longer used. The controller discovers
	// GatewayClasses by spec.controllerName, not by name.
	rootCmd.Flags().String("gateway-class-name", "", "DEPRECATED: no longer used, will be removed in a future release")
	_ = rootCmd.Flags().MarkHidden("gateway-class-name")
	_ = rootCmd.Flags().MarkDeprecated("gateway-class-name",
		"the controller now discovers GatewayClasses by spec.controllerName; this flag is ignored")

	_ = viper.BindPFlags(rootCmd.Flags())
	_ = viper.BindPFlags(rootCmd.PersistentFlags())
}

func initConfig() {
	viper.SetEnvPrefix("CF")
	// Map hyphenated flag keys to underscore env names: the key `tracing-enabled`
	// becomes CF_TRACING_ENABLED. Without this, viper looks up the impossible
	// `CF_TRACING-ENABLED` and every hyphenated CF_* env var is silently ignored.
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	viper.SetDefault("controller-name", "cf.k8s.lex.la/tunnel-controller")
	viper.SetDefault("metrics-addr", ":8080")
	viper.SetDefault("health-addr", ":8081")
	viper.SetDefault("log-level", "info")
	viper.SetDefault("log-format", "json")
	viper.SetDefault("tunnel-protocol", "auto")
	viper.SetDefault("leader-elect", false)
	viper.SetDefault("leader-election-name", "cloudflare-tunnel-gateway-controller-leader")
	viper.SetDefault("tracing-enabled", false)
	viper.SetDefault("tracing-sample-rate", 1.0)
}

func Execute() error {
	return errors.Wrap(rootCmd.Execute(), "command execution failed")
}

func setupLogger() *slog.Logger {
	level := slog.LevelInfo

	switch viper.GetString("log-level") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}

	var handler slog.Handler
	if viper.GetString("log-format") == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	// Wrap with TraceHandler for automatic OpenTelemetry trace ID injection
	handler = logging.NewTraceHandler(handler)

	return slog.New(handler)
}

//nolint:noinlineerr // inline error handling is fine here
func runController(_ *cobra.Command, _ []string) error {
	logger := setupLogger()
	slog.SetDefault(logger)
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	logger.Info("starting cloudflare-tunnel-gateway-controller",
		"version", version, "gitsha", gitsha)

	tracingEnabled := viper.GetBool("tracing-enabled")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	shutdownTracing := setupTracing(ctx, logger, tracingEnabled)
	defer shutdownTracing()

	cfg := controller.Config{
		ClusterDomain:  resolveClusterDomain(logger),
		ControllerName: viper.GetString("controller-name"),
		MetricsAddr:    viper.GetString("metrics-addr"),
		HealthAddr:     viper.GetString("health-addr"),

		LeaderElect:     viper.GetBool("leader-elect"),
		LeaderElectNS:   viper.GetString("leader-election-namespace"),
		LeaderElectName: viper.GetString("leader-election-name"),

		ProxyEndpoints:       viper.GetStringSlice("proxy-endpoints"),
		ProxyAuthToken:       viper.GetString("proxy-auth-token"),
		ProxyTokenSecret:     viper.GetString("proxy-token-secret"),
		ProxyDeploymentLabel: viper.GetString("proxy-deployment-label"),
		TunnelProtocol:       viper.GetString("tunnel-protocol"),
		Tracing:              tracingEnabled,

		HostnameOwnershipEnforce:           viper.GetBool("hostname-ownership-enforce"),
		HostnameOwnershipLabelKey:          viper.GetString("hostname-ownership-label-key"),
		HostnameOwnershipNamespaceSelector: viper.GetString("hostname-ownership-namespace-selector"),
	}

	if err := controller.Run(ctx, &cfg); err != nil {
		return errors.Wrap(err, "failed to run controller")
	}

	return nil
}

// setupTracing installs a global OpenTelemetry TracerProvider + propagator via
// internal/tracing and returns a shutdown function. When disabled the function
// is a no-op. A setup failure is logged and the controller continues without
// tracing rather than refusing to start.
func setupTracing(ctx context.Context, logger *slog.Logger, enabled bool) func() {
	cfg := tracing.Config{
		Enabled:     enabled,
		Endpoint:    viper.GetString("tracing-endpoint"),
		SampleRate:  viper.GetFloat64("tracing-sample-rate"),
		ServiceName: "controller",
		Version:     version,
	}

	shutdown, err := tracing.Setup(ctx, cfg)
	if err != nil {
		logger.Error("tracing setup failed -- continuing without tracing", "error", err)

		return func() {}
	}

	if enabled {
		logger.Info("distributed tracing enabled",
			"endpoint", cfg.Endpoint, "sampleRate", cfg.SampleRate)
	}

	// Deliberately a fresh context, not derived from the run ctx: this runs at
	// defer time after SIGTERM has already cancelled the run context, and the
	// OTLP exporter must still get a window to flush.
	//nolint:contextcheck // shutdown must outlive the cancelled run context to flush spans
	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), tracingShutdownTimeout)
		defer cancel()

		serr := shutdown(shutdownCtx)
		if serr != nil {
			logger.Error("tracing shutdown error", "error", serr)
		}
	}
}

// resolveClusterDomain determines the cluster domain to use.
// User-configured value takes precedence, then auto-detection,
// finally falls back to default.
func resolveClusterDomain(logger *slog.Logger) string {
	// User explicit value takes precedence (CLI flag or CF_CLUSTER_DOMAIN env var)
	if configured := viper.GetString("cluster-domain"); configured != "" {
		logger.Info("using configured cluster domain",
			"clusterDomain", configured,
		)

		return configured
	}

	// Try auto-detection from /etc/resolv.conf
	if detected, ok := dns.DetectClusterDomain(); ok {
		logger.Info("auto-detected cluster domain from /etc/resolv.conf",
			"clusterDomain", detected,
		)

		return detected
	}

	// Final fallback to default
	logger.Info("using default cluster domain (auto-detection failed)",
		"clusterDomain", dns.DefaultClusterDomain,
	)

	return dns.DefaultClusterDomain
}
