package controller

import (
	"context"

	"github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/accounts"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cockroachdb/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/helm"
)

// Config holds all configuration options for the controller manager.
// Values are typically populated from CLI flags or environment variables.
type Config struct {
	// AccountID is the Cloudflare account ID. If empty, it will be auto-detected
	// from the API token (requires token to have access to exactly one account).
	AccountID string

	// TunnelID is the Cloudflare Tunnel ID (required).
	TunnelID string

	// APIToken is the Cloudflare API token with Tunnel permissions (required).
	APIToken string

	// ClusterDomain is the Kubernetes cluster domain for service DNS resolution.
	// Defaults to "cluster.local".
	ClusterDomain string

	// GatewayClassName is the name of the GatewayClass to watch.
	// Only Gateways referencing this class will be reconciled.
	GatewayClassName string

	// ControllerName is the controller name reported in GatewayClass status.
	ControllerName string

	// MetricsAddr is the address for the Prometheus metrics endpoint.
	MetricsAddr string

	// HealthAddr is the address for health and readiness probe endpoints.
	HealthAddr string

	// LeaderElect enables leader election for high availability.
	// Required when running multiple replicas.
	LeaderElect bool

	// LeaderElectNS is the namespace for the leader election lease.
	LeaderElectNS string

	// LeaderElectName is the name of the leader election lease.
	LeaderElectName string

	// ManageCloudflared enables automatic cloudflared deployment via Helm.
	ManageCloudflared bool

	// TunnelToken is the Cloudflare Tunnel token for remote-managed mode.
	// Required when ManageCloudflared is true.
	TunnelToken string

	// CloudflaredNS is the namespace for cloudflared deployment.
	CloudflaredNS string

	// CloudflaredProto is the transport protocol (auto, quic, http2).
	CloudflaredProto string

	// AWGSecretName is the secret containing AWG VPN configuration.
	// If set, enables AWG sidecar for cloudflared.
	AWGSecretName string

	// AWGInterfaceName is the AWG network interface name.
	AWGInterfaceName string
}

// Run initializes and starts the controller manager with the provided configuration.
// It sets up the Cloudflare API client, creates Gateway and HTTPRoute controllers,
// and blocks until the context is cancelled or an error occurs.
//
// The function performs the following steps:
//  1. Creates Cloudflare API client with the provided token
//  2. Auto-detects account ID if not provided
//  3. Initializes controller-runtime manager with metrics and health endpoints
//  4. Sets up GatewayReconciler and HTTPRouteReconciler
//  5. Optionally initializes Helm manager for cloudflared deployment
//  6. Starts the manager and blocks until shutdown
//
//nolint:funlen,noinlineerr // controller setup requires multiple steps
func Run(ctx context.Context, cfg *Config) error {
	logger := log.FromContext(ctx).WithName("manager")
	logger.Info("initializing controller manager")

	logger.Info("creating cloudflare client")

	cfClient := cloudflare.NewClient(
		option.WithAPIToken(cfg.APIToken),
	)

	logger.Info("cloudflare client created")

	accountID := cfg.AccountID
	if accountID == "" {
		logger.Info("resolving account ID")

		var resolveErr error

		accountID, resolveErr = resolveAccountID(ctx, cfClient)
		if resolveErr != nil {
			logger.Error(resolveErr, "failed to resolve account ID")

			return resolveErr
		}

		logger.Info("auto-detected account ID", "accountID", accountID)
	}

	logger.Info("creating ctrl.Manager")

	mgrOptions := ctrl.Options{
		Metrics: server.Options{
			BindAddress: cfg.MetricsAddr,
		},
		HealthProbeBindAddress: cfg.HealthAddr,
	}

	if cfg.LeaderElect {
		mgrOptions.LeaderElection = true
		mgrOptions.LeaderElectionID = cfg.LeaderElectName
		mgrOptions.LeaderElectionNamespace = cfg.LeaderElectNS

		logger.Info("leader election enabled",
			"id", cfg.LeaderElectName,
			"namespace", cfg.LeaderElectNS,
		)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOptions)
	if err != nil {
		return errors.Wrap(err, "failed to create manager")
	}

	if err := gatewayv1.Install(mgr.GetScheme()); err != nil {
		return errors.Wrap(err, "failed to add gateway-api scheme")
	}

	var helmManager *helm.Manager

	if cfg.ManageCloudflared {
		var helmErr error

		helmManager, helmErr = helm.NewManager()
		if helmErr != nil {
			return errors.Wrap(helmErr, "failed to create helm manager")
		}

		logger.Info("helm manager initialized for cloudflared deployment")
	}

	gatewayReconciler := &GatewayReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		GatewayClassName: cfg.GatewayClassName,
		ControllerName:   cfg.ControllerName,
		TunnelID:         cfg.TunnelID,
		HelmManager:      helmManager,
		TunnelToken:      cfg.TunnelToken,
		CloudflaredNS:    cfg.CloudflaredNS,
		Protocol:         cfg.CloudflaredProto,
		AWGSecretName:    cfg.AWGSecretName,
		AWGInterfaceName: cfg.AWGInterfaceName,
	}

	if err := gatewayReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup gateway controller")
	}

	httpRouteReconciler := &HTTPRouteReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		CFClient:         cfClient,
		AccountID:        accountID,
		TunnelID:         cfg.TunnelID,
		ClusterDomain:    cfg.ClusterDomain,
		GatewayClassName: cfg.GatewayClassName,
		ControllerName:   cfg.ControllerName,
	}

	if err := httpRouteReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup httproute controller")
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return errors.Wrap(err, "failed to set up health check")
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return errors.Wrap(err, "failed to set up ready check")
	}

	logger.Info("starting manager")

	if err := mgr.Start(ctx); err != nil {
		return errors.Wrap(err, "failed to start manager")
	}

	return nil
}

func resolveAccountID(ctx context.Context, client *cloudflare.Client) (string, error) {
	result, err := client.Accounts.List(ctx, accounts.AccountListParams{})
	if err != nil {
		return "", errors.Wrap(err, "failed to list accounts")
	}

	accountList := result.Result
	if len(accountList) == 0 {
		return "", errors.New("no accounts found for this API token")
	}

	if len(accountList) > 1 {
		//nolint:wrapcheck // this is a new error, not wrapping external
		return "", errors.Newf("multiple accounts found (%d), please specify --account-id explicitly", len(accountList))
	}

	return accountList[0].ID, nil
}
