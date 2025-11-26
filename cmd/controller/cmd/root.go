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
ingress rules accordingly.`,
	RunE:          runController,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.PersistentFlags().String("log-level", "info", "Log level (debug, info, warn, error)")
	rootCmd.PersistentFlags().String("log-format", "json", "Log format (json, text)")

	rootCmd.Flags().String("account-id", "", "Cloudflare account ID")
	rootCmd.Flags().String("tunnel-id", "", "Cloudflare tunnel ID")
	rootCmd.Flags().String("api-token", "", "Cloudflare API token (or use CF_API_TOKEN env var)")
	rootCmd.Flags().String("cluster-domain", "cluster.local", "Kubernetes cluster domain")
	rootCmd.Flags().String("gateway-class-name", "cloudflare-tunnel", "GatewayClass name to watch")
	rootCmd.Flags().String("controller-name", "cf.k8s.lex.la/tunnel-controller", "Controller name for GatewayClass")
	rootCmd.Flags().String("metrics-addr", ":8080", "Address for metrics endpoint")
	rootCmd.Flags().String("health-addr", ":8081", "Address for health probe endpoint")

	// Leader election flags
	rootCmd.Flags().Bool("leader-elect", false, "Enable leader election for high availability")
	rootCmd.Flags().String("leader-election-namespace", "", "Namespace for leader election lease (defaults to controller namespace)")
	rootCmd.Flags().String("leader-election-name", "cloudflare-tunnel-gateway-controller-leader", "Name of the leader election lease")

	// Cloudflared Helm deployment flags
	rootCmd.Flags().Bool("manage-cloudflared", false, "Deploy and manage cloudflared via Helm")
	rootCmd.Flags().String("tunnel-token", "", "Cloudflare tunnel token for remote-managed mode")
	rootCmd.Flags().String("cloudflared-namespace", "cloudflare-tunnel-system", "Namespace for cloudflared deployment")
	rootCmd.Flags().String("cloudflared-protocol", "", "Transport protocol for cloudflared (auto, quic, http2)")
	rootCmd.Flags().String("awg-secret-name", "", "Secret name containing AWG config for sidecar")
	rootCmd.Flags().String("awg-interface-name", "", "AWG interface name (must match config file name without .conf)")

	_ = viper.BindPFlags(rootCmd.Flags())
	_ = viper.BindPFlags(rootCmd.PersistentFlags())
}

func initConfig() {
	viper.SetEnvPrefix("CF")
	viper.AutomaticEnv()

	viper.SetDefault("cluster-domain", "cluster.local")
	viper.SetDefault("gateway-class-name", "cloudflare-tunnel")
	viper.SetDefault("controller-name", "cf.k8s.lex.la/tunnel-controller")
	viper.SetDefault("metrics-addr", ":8080")
	viper.SetDefault("health-addr", ":8081")
	viper.SetDefault("log-level", "info")
	viper.SetDefault("log-format", "json")
	viper.SetDefault("leader-elect", false)
	viper.SetDefault("leader-election-name", "cloudflare-tunnel-gateway-controller-leader")
	viper.SetDefault("manage-cloudflared", false)
	viper.SetDefault("cloudflared-namespace", "cloudflare-tunnel-system")
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
		"version", version,
		"gitsha", gitsha,
	)

	accountID := viper.GetString("account-id")

	tunnelID := viper.GetString("tunnel-id")
	if tunnelID == "" {
		return errors.New("tunnel-id is required")
	}

	apiToken := viper.GetString("api-token")
	if apiToken == "" {
		return errors.New("api-token is required (use --api-token or CF_API_TOKEN env var)")
	}

	manageCloudflared := viper.GetBool("manage-cloudflared")
	tunnelToken := viper.GetString("tunnel-token")

	if manageCloudflared && tunnelToken == "" {
		return errors.New("tunnel-token is required when manage-cloudflared is enabled")
	}

	cfg := controller.Config{
		AccountID:        accountID,
		TunnelID:         tunnelID,
		APIToken:         apiToken,
		ClusterDomain:    viper.GetString("cluster-domain"),
		GatewayClassName: viper.GetString("gateway-class-name"),
		ControllerName:   viper.GetString("controller-name"),
		MetricsAddr:      viper.GetString("metrics-addr"),
		HealthAddr:       viper.GetString("health-addr"),

		LeaderElect:     viper.GetBool("leader-elect"),
		LeaderElectNS:   viper.GetString("leader-election-namespace"),
		LeaderElectName: viper.GetString("leader-election-name"),

		ManageCloudflared: manageCloudflared,
		TunnelToken:       tunnelToken,
		CloudflaredNS:     viper.GetString("cloudflared-namespace"),
		CloudflaredProto:  viper.GetString("cloudflared-protocol"),
		AWGSecretName:     viper.GetString("awg-secret-name"),
		AWGInterfaceName:  viper.GetString("awg-interface-name"),
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := controller.Run(ctx, &cfg); err != nil {
		return errors.Wrap(err, "failed to run controller")
	}

	return nil
}
