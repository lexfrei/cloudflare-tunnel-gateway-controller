package controller

import (
	"context"
	"os"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlMetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/helm"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/metrics"
)

// Config holds all configuration options for the controller manager.
// Values are typically populated from CLI flags or environment variables.
type Config struct {
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
}

// Run initializes and starts the controller manager with the provided configuration.
// It sets up the config resolver, creates Gateway and HTTPRoute controllers,
// and blocks until the context is cancelled or an error occurs.
//
// The function performs the following steps:
//  1. Initializes controller-runtime manager with metrics and health endpoints
//  2. Registers GatewayClassConfig CRD scheme
//  3. Creates ConfigResolver for reading GatewayClassConfig
//  4. Sets up GatewayReconciler and HTTPRouteReconciler with watches
//  5. Optionally initializes Helm manager for cloudflared deployment
//  6. Starts the manager and blocks until shutdown
//
//nolint:funlen,noinlineerr // controller setup requires multiple steps
func Run(ctx context.Context, cfg *Config) error {
	logger := log.FromContext(ctx).WithName("manager")
	logger.Info("initializing controller manager")

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

	// Register Gateway API types
	if err := gatewayv1.Install(mgr.GetScheme()); err != nil {
		return errors.Wrap(err, "failed to add gateway-api scheme")
	}

	// Register Gateway API v1beta1 types (required for ReferenceGrant)
	if err := gatewayv1beta1.Install(mgr.GetScheme()); err != nil {
		return errors.Wrap(err, "failed to add gateway-api v1beta1 scheme")
	}

	// Register GatewayClassConfig CRD types
	if err := v1alpha1.AddToScheme(mgr.GetScheme()); err != nil {
		return errors.Wrap(err, "failed to add GatewayClassConfig scheme")
	}

	// Create metrics collector and register with controller-runtime
	metricsCollector := metrics.NewCollector(ctrlMetrics.Registry)

	// Determine default namespace for secret lookups
	defaultNamespace := getControllerNamespace()

	// Create config resolver
	configResolver := config.NewResolver(mgr.GetClient(), defaultNamespace, metricsCollector)

	// Initialize Helm manager for cloudflared deployment
	helmManager, helmErr := helm.NewManager(metricsCollector)
	if helmErr != nil {
		return errors.Wrap(helmErr, "failed to create helm manager")
	}

	logger.Info("helm manager initialized for cloudflared deployment")

	gatewayReconciler := &GatewayReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		GatewayClassName: cfg.GatewayClassName,
		ControllerName:   cfg.ControllerName,
		ConfigResolver:   configResolver,
		HelmManager:      helmManager,
	}

	if err := gatewayReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup gateway controller")
	}

	// Create shared route syncer for unified HTTP and GRPC route synchronization
	routeSyncer := NewRouteSyncer(
		mgr.GetClient(),
		mgr.GetScheme(),
		cfg.ClusterDomain,
		cfg.GatewayClassName,
		configResolver,
		metricsCollector,
	)

	httpRouteReconciler := &HTTPRouteReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		GatewayClassName: cfg.GatewayClassName,
		ControllerName:   cfg.ControllerName,
		RouteSyncer:      routeSyncer,
	}

	if err := httpRouteReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup httproute controller")
	}

	grpcRouteReconciler := &GRPCRouteReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		GatewayClassName: cfg.GatewayClassName,
		ControllerName:   cfg.ControllerName,
		RouteSyncer:      routeSyncer,
	}

	if err := grpcRouteReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup grpcroute controller")
	}

	// Setup GatewayClassConfig controller for status updates
	configReconciler := &GatewayClassConfigReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		DefaultNamespace: defaultNamespace,
	}

	if err := configReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup gatewayclassconfig controller")
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

// getControllerNamespace returns the namespace where the controller is running.
// It first checks CONTROLLER_NAMESPACE environment variable, then reads from
// the standard Kubernetes downward API file, falling back to "default".
func getControllerNamespace() string {
	// Allow override via environment variable (useful for testing)
	if ns := os.Getenv("CONTROLLER_NAMESPACE"); ns != "" {
		return ns
	}

	// Try reading from downward API
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err == nil {
		return string(data)
	}

	// Fallback to default
	return "default"
}

// init registers core types needed for watching Secrets.
func init() {
	// corev1 is already registered by controller-runtime, but we ensure it's available
	_ = corev1.AddToScheme
}
