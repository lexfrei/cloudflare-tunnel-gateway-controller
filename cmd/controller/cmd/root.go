package cmd

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cockroachdb/errors"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/controller"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/dns"
)

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
	rootCmd.Flags().String("gateway-class-name", "cloudflare-tunnel", "GatewayClass name to watch")
	rootCmd.Flags().String("controller-name", "cf.k8s.lex.la/tunnel-controller", "Controller name for GatewayClass")
	rootCmd.Flags().String("metrics-addr", ":8080", "Address for metrics endpoint")
	rootCmd.Flags().String("health-addr", ":8081", "Address for health probe endpoint")

	// Leader election flags
	rootCmd.Flags().Bool("leader-elect", false, "Enable leader election for high availability")
	rootCmd.Flags().String("leader-election-namespace", "", "Namespace for leader election lease (defaults to controller namespace)")
	rootCmd.Flags().String("leader-election-name", "cloudflare-tunnel-gateway-controller-leader", "Name of the leader election lease")

	_ = viper.BindPFlags(rootCmd.Flags())
	_ = viper.BindPFlags(rootCmd.PersistentFlags())
}

func initConfig() {
	viper.SetEnvPrefix("CF")
	viper.AutomaticEnv()

	viper.SetDefault("gateway-class-name", "cloudflare-tunnel")
	viper.SetDefault("controller-name", "cf.k8s.lex.la/tunnel-controller")
	viper.SetDefault("metrics-addr", ":8080")
	viper.SetDefault("health-addr", ":8081")
	viper.SetDefault("log-level", "info")
	viper.SetDefault("log-format", "json")
	viper.SetDefault("leader-elect", false)
	viper.SetDefault("leader-election-name", "cloudflare-tunnel-gateway-controller-leader")
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

	return slog.New(handler)
}

//nolint:noinlineerr // inline error handling is fine here
func runController(_ *cobra.Command, _ []string) error {
	logger := setupLogger()
	slog.SetDefault(logger)
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	logger.Info("starting cloudflare-tunnel-gateway-controller",
		"version", version, "gitsha", gitsha)

	cfg := controller.Config{
		ClusterDomain:    resolveClusterDomain(logger),
		GatewayClassName: viper.GetString("gateway-class-name"),
		ControllerName:   viper.GetString("controller-name"),
		MetricsAddr:      viper.GetString("metrics-addr"),
		HealthAddr:       viper.GetString("health-addr"),

		LeaderElect:     viper.GetBool("leader-elect"),
		LeaderElectNS:   viper.GetString("leader-election-namespace"),
		LeaderElectName: viper.GetString("leader-election-name"),
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := controller.Run(ctx, &cfg); err != nil {
		return errors.Wrap(err, "failed to run controller")
	}

	return nil
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
