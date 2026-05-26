package controller

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlMetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// Config holds all configuration options for the controller manager.
// Values are typically populated from CLI flags or environment variables.
type Config struct {
	// ClusterDomain is the Kubernetes cluster domain for service DNS resolution.
	// Defaults to "cluster.local".
	ClusterDomain string

	// ControllerName identifies this controller per Gateway API spec.
	// The controller watches all GatewayClasses with matching controllerName.
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

	// ProxyEndpoints is the list of L7 proxy config-API URLs.
	// When non-empty, the controller pushes routing config to these endpoints.
	// Example: ["http://proxy-0:8081", "http://proxy-1:8081"]
	ProxyEndpoints []string

	// ProxyAuthToken is the Bearer token for authenticating config push requests.
	// When set, the controller includes "Authorization: Bearer <token>" in push requests.
	ProxyAuthToken string
}

// Run initializes and starts the controller manager with the provided configuration.
// It sets up the config resolver, creates Gateway and HTTPRoute controllers,
// and blocks until the context is cancelled or an error occurs.
//
// The function performs the following steps:
//  1. Fails fast when ProxyEndpoints is empty (v3 requires a configured L7 proxy data plane)
//  2. Initializes controller-runtime manager with metrics and health endpoints
//  3. Registers GatewayClassConfig CRD scheme
//  4. Creates ConfigResolver for reading GatewayClassConfig
//  5. Sets up Gateway/HTTPRoute/GRPCRoute/GatewayClassConfig reconcilers with watches
//  6. Wires the ProxySyncer that pushes HTTPRoute config to the proxy data plane
//  7. Starts the manager and blocks until shutdown
//
//nolint:funlen // controller setup requires multiple sequential steps
func Run(ctx context.Context, cfg *Config) error {
	logger := log.FromContext(ctx).WithName("manager")
	logger.Info("initializing controller manager")

	// In v3 the L7 proxy is the only data plane; without endpoints to push to,
	// the controller would silently no-op all HTTPRoutes. Fail loudly instead.
	// Use a local instead of mutating the caller's Config -- a future
	// retry-wrapper around Run shouldn't accumulate state across calls.
	proxyEndpoints := sanitiseProxyEndpoints(cfg.ProxyEndpoints)
	if len(proxyEndpoints) == 0 {
		return errors.New("--proxy-endpoints is required: v3 controller cannot run without a configured L7 proxy data plane")
	}

	mgrOptions := ctrl.Options{
		Metrics: server.Options{
			BindAddress: cfg.MetricsAddr,
		},
		HealthProbeBindAddress: cfg.HealthAddr,
		// Enable field validation warnings for early detection of unknown fields
		Client: client.Options{
			FieldValidation: "Warn",
		},
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
	metricsCollector := cfmetrics.NewCollector(ctrlMetrics.Registry)

	// Determine default namespace for secret lookups
	defaultNamespace := getControllerNamespace()

	// Create config resolver
	configResolver := config.NewResolver(mgr.GetClient(), defaultNamespace, metricsCollector)

	// Create base logger for component injection
	// Uses slog.Default() which can be configured with TraceHandler at startup
	baseLogger := slog.Default()

	gatewayReconciler := &GatewayReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		ControllerName: cfg.ControllerName,
		ConfigResolver: configResolver,
	}

	if err := gatewayReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup gateway controller")
	}

	// Create shared route syncer for unified HTTP and GRPC route synchronization
	routeSyncer := NewRouteSyncer(
		mgr.GetClient(),
		mgr.GetScheme(),
		cfg.ClusterDomain,
		cfg.ControllerName,
		configResolver,
		metricsCollector,
		baseLogger,
	)

	// Create proxy syncer for L7 proxy config push (mandatory in v3)
	proxySyncer := initProxySyncer(cfg, proxyEndpoints, mgr.GetClient(), baseLogger, logger)

	httpRouteReconciler := &HTTPRouteReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		ControllerName: cfg.ControllerName,
		RouteSyncer:    routeSyncer,
		ProxySyncer:    proxySyncer,
		ProxyEndpoints: proxyEndpoints,
	}

	if err := httpRouteReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup httproute controller")
	}

	grpcRouteReconciler := &GRPCRouteReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		ControllerName: cfg.ControllerName,
		RouteSyncer:    routeSyncer,
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

	if err := setupStatusReconcilers(mgr, cfg.ControllerName); err != nil {
		return err
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

// initProxySyncer creates a ProxySyncer. Starting v3 the L7 proxy is the
// mandatory data plane, so the sanitised proxyEndpoints list MUST be
// non-empty; callers validate that up-front and fail the controller
// bootstrap otherwise. proxyEndpoints is the sanitised slice (not the
// raw cfg.ProxyEndpoints) so the startup log reflects what the syncer
// will actually push to, not what the operator typed in.
func initProxySyncer(
	cfg *Config,
	proxyEndpoints []string,
	k8sClient client.Client,
	baseLogger *slog.Logger,
	logger logr.Logger,
) *ProxySyncer {
	logger.Info("proxy syncer enabled", "endpoints", proxyEndpoints)

	return NewProxySyncer(
		cfg.ClusterDomain,
		cfg.ProxyAuthToken,
		cfg.ControllerName,
		k8sClient,
		baseLogger,
	)
}

// sanitiseProxyEndpoints trims whitespace from every endpoint and drops
// empty entries. Callers pass the result through a `len(...) == 0` check
// to reject malformed `--proxy-endpoints=` / `--proxy-endpoints=,` shapes
// that would otherwise survive a raw len() guard.
func sanitiseProxyEndpoints(endpoints []string) []string {
	out := make([]string, 0, len(endpoints))

	for _, ep := range endpoints {
		if trimmed := strings.TrimSpace(ep); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
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
