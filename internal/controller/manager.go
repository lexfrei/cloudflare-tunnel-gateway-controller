package controller

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrlMetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	mcsv1alpha1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/hostnameownership"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// installSchemes registers the Gateway API v1, v1beta1 (for ReferenceGrant)
// and GatewayClassConfig CRD types into the manager's runtime scheme.
// Extracted from Run so the per-reconciler setup chain in Run stays under
// the cyclomatic-complexity gate.
func installSchemes(mgr ctrl.Manager) error {
	if err := gatewayv1.Install(mgr.GetScheme()); err != nil {
		return errors.Wrap(err, "failed to add gateway-api scheme")
	}

	if err := gatewayv1beta1.Install(mgr.GetScheme()); err != nil {
		return errors.Wrap(err, "failed to add gateway-api v1beta1 scheme")
	}

	if err := v1alpha1.AddToScheme(mgr.GetScheme()); err != nil {
		return errors.Wrap(err, "failed to add GatewayClassConfig scheme")
	}

	if err := mcsv1alpha1.Install(mgr.GetScheme()); err != nil {
		return errors.Wrap(err, "failed to add mcs-api (ServiceImport) scheme")
	}

	// apiextensions types let the GatewayClass reconciler read the installed
	// Gateway API CRD bundle-version annotation for the SupportedVersion check.
	if err := apiextensionsv1.AddToScheme(mgr.GetScheme()); err != nil {
		return errors.Wrap(err, "failed to add apiextensions scheme")
	}

	return nil
}

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

	// TunnelProtocol is the proxy's configured edge transport (auto|http2|quic).
	// Used only to warn when GRPCRoutes are present on an explicit quic tunnel,
	// where cloudflared drops the grpc-status trailer. auto/unset is upgraded to
	// http2 by the proxy when a GRPCRoute is present, so it is not flagged.
	TunnelProtocol string

	// HostnameOwnershipEnforce enables the controller-side layer of the
	// per-namespace hostname-ownership policy (#475): routes whose hostnames
	// fall outside their namespace's allowed suffix are rejected at binding
	// time and never programmed. Independent of (and in addition to) the CEL
	// ValidatingAdmissionPolicy the chart can install — defence in depth.
	HostnameOwnershipEnforce bool

	// HostnameOwnershipLabelKey is the namespace label carrying the allowed
	// hostname suffix. Must match the admission policy's labelKey.
	HostnameOwnershipLabelKey string

	// HostnameOwnershipNamespaceSelector scopes which namespaces are policed
	// (kubectl label-selector syntax; empty = all namespaces, fail-closed
	// everywhere). Must match the admission policy binding's namespaceSelector.
	HostnameOwnershipNamespaceSelector string

	// MonitoringNamespaceSelector (kubectl label-selector syntax; empty =
	// controller namespace only) additionally admits the per-Gateway proxy's
	// config-API port — which also serves /metrics — in the rendered
	// NetworkPolicy, so Prometheus in the matching namespaces can scrape.
	MonitoringNamespaceSelector string

	// RenderNetworkPolicy gates whether the controller renders the per-Gateway
	// config-API NetworkPolicy. Wired from the chart's proxy.networkPolicy.enabled
	// (default true). Set false on strict CNIs where the node-sourced kubelet
	// probes cannot match the policy's namespaceSelector ingress rule — the
	// documented escape hatch then applies to per-Gateway planes, not only the
	// shared proxy.
	RenderNetworkPolicy bool

	// ProxyImage is the container image for per-Gateway rendered proxy
	// Deployments (GatewayConfig data planes). The chart wires it to the
	// release's proxy image; GatewayConfig.spec.image overrides per Gateway.
	ProxyImage string

	// ProxyTokenSecret identifies the Secret holding the tunnel token used by
	// the proxy. Format: "<namespace>/<name>". When set, the controller
	// watches the named Secret and patches the proxy Deployment's pod
	// template with a revision annotation on every change, causing
	// Kubernetes to roll the pods so cloudflared picks up the rotated
	// credential. Issue #114.
	//
	// Empty value disables the watcher entirely -- safe default for setups
	// that don't rotate the token, and for `helm template` rendering
	// without a real Secret.
	ProxyTokenSecret string

	// ProxyDeploymentLabel selects the proxy Deployment(s) to roll on
	// tunnel-token Secret change. Format: "key=value". When unset, the
	// SecretReconciler falls back to the chart's standard proxy label set
	// (`app.kubernetes.io/component=proxy`) within ProxyTokenSecret's
	// namespace.
	ProxyDeploymentLabel string

	// Tracing enables OpenTelemetry instrumentation of the controller's
	// outbound HTTP clients (Cloudflare API + proxy config push). The global
	// TracerProvider is installed by the binary entrypoint; this flag only
	// gates whether those clients wrap their transports with otelhttp.
	Tracing bool
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
//nolint:funlen,gocyclo,cyclop // controller setup requires multiple sequential reconciler wires
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

	if err := installSchemes(mgr); err != nil {
		return err
	}

	// Create metrics collector and register with controller-runtime
	metricsCollector := cfmetrics.NewCollector(ctrlMetrics.Registry)

	// Determine default namespace for secret lookups
	defaultNamespace := getControllerNamespace()

	// Create config resolver. When tracing is on, instrument the Cloudflare
	// API client so outbound API calls emit client spans.
	var resolverOpts []config.ResolverOption
	if cfg.Tracing {
		resolverOpts = append(resolverOpts, config.WithCloudflareTracing())
	}

	configResolver := config.NewResolver(mgr.GetClient(), defaultNamespace, metricsCollector, resolverOpts...)

	// Create base logger for component injection
	// Uses slog.Default() which can be configured with TraceHandler at startup
	baseLogger := slog.Default()

	// Shared per-Gateway merge-view cache (issue #332). One instance threaded
	// through every reconciler/syncer that needs the ListenerSet merge view so
	// a burst of reconciles from one event reuses a single computation.
	viewStore := newMergeViewStore()

	gatewayReconciler := &GatewayReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		ControllerName: cfg.ControllerName,
		ConfigResolver: configResolver,
		ViewStore:      viewStore,
	}

	if err := gatewayReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup gateway controller")
	}

	// Per-Gateway data planes (#479): renders a dedicated proxy Deployment +
	// config Service (+ optional HPA) for Gateways carrying
	// infrastructure.parametersRef. Status stays with gatewayReconciler.
	monitoringSelector, err := parseNamespaceSelector(cfg.MonitoringNamespaceSelector)
	if err != nil {
		return errors.Wrap(err, "invalid --monitoring-namespace-selector")
	}

	gatewayInfraReconciler := &GatewayInfraReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		ControllerName: cfg.ControllerName,
		ConfigResolver: configResolver,
		Recorder:       mgr.GetEventRecorder("gateway-infra-controller"),
		RenderDefaults: render.Defaults{
			ProxyImage:     cfg.ProxyImage,
			TunnelProtocol: cfg.TunnelProtocol,
		},
		ControllerNamespace:         defaultNamespace,
		MonitoringNamespaceSelector: monitoringSelector,
		RenderNetworkPolicy:         cfg.RenderNetworkPolicy,
	}

	if err := gatewayInfraReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup gateway infra controller")
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
	routeSyncer.ViewStore = viewStore

	if cfg.HostnameOwnershipEnforce {
		ownershipPolicy, ownershipErr := hostnameownership.New(
			cfg.HostnameOwnershipLabelKey, cfg.HostnameOwnershipNamespaceSelector)
		if ownershipErr != nil {
			// Fail loud at startup: a malformed policy must not silently run
			// with enforcement off.
			return errors.Wrap(ownershipErr, "failed to compile hostname-ownership policy")
		}

		routeSyncer.HostnameOwnership = ownershipPolicy

		logger.Info("hostname-ownership enforcement enabled",
			"labelKey", cfg.HostnameOwnershipLabelKey,
			"namespaceSelector", cfg.HostnameOwnershipNamespaceSelector)
	}

	// Create proxy syncer for L7 proxy config push (mandatory in v3)
	proxySyncer := initProxySyncer(cfg, proxyEndpoints, mgr.GetClient(), baseLogger, logger)
	proxySyncer.ViewStore = viewStore

	httpRouteReconciler := &HTTPRouteReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		ControllerName: cfg.ControllerName,
		RouteSyncer:    routeSyncer,
		ProxySyncer:    proxySyncer,
		ProxyEndpoints: proxyEndpoints,
		Recorder:       mgr.GetEventRecorder("httproute-controller"),
		ViewStore:      viewStore,
	}

	// A newly-rendered per-Gateway data plane needs an initial config push to
	// pass /readyz (config version > 0), just as the shared plane gets one
	// from the startup sync. Route reconciles are route-event-driven, so a
	// data plane with no routes would never be synced — wire the infra
	// reconciler to run a full route sync (cache + push to every partition)
	// when it creates a data plane.
	gatewayInfraReconciler.TriggerRouteSync = func(ctx context.Context) error {
		_, err := httpRouteReconciler.syncAndUpdateStatus(ctx)

		return err
	}

	if err := httpRouteReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup httproute controller")
	}

	grpcRouteReconciler := &GRPCRouteReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		ControllerName: cfg.ControllerName,
		RouteSyncer:    routeSyncer,
		ProxySyncer:    proxySyncer,
		ProxyEndpoints: proxyEndpoints,
		TunnelProtocol: cfg.TunnelProtocol,
		Recorder:       mgr.GetEventRecorder("grpcroute-controller"),
		ViewStore:      viewStore,
	}

	if err := grpcRouteReconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup grpcroute controller")
	}

	if err := setupListenerSetReconciler(mgr, cfg.ControllerName, viewStore); err != nil {
		return err
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

	if err := setupProxyEndpointReconciler(mgr, proxySyncer, proxyEndpoints); err != nil {
		return err
	}

	if err := setupProxySecretReconciler(mgr, cfg.ProxyTokenSecret, cfg.ProxyDeploymentLabel); err != nil {
		return err
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

// setupListenerSetReconciler wires the ListenerSet controller. Extracted
// from Run so the per-reconciler setup chain in Run stays under the
// cyclomatic-complexity gate.
func setupListenerSetReconciler(mgr ctrl.Manager, controllerName string, viewStore *mergeViewStore) error {
	reconciler := &ListenerSetReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		ControllerName: controllerName,
		ViewStore:      viewStore,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup listenerset controller")
	}

	return nil
}

// setupProxySecretReconciler wires the tunnel-token Secret watcher that
// patches the proxy Deployment's pod template on Secret change so
// Kubernetes natively rolls the pods (issue #114). When
// proxyTokenSecret is empty the watcher is skipped entirely -- the
// controller starts and runs without it, matching the behaviour of
// clusters that don't rotate the token at runtime.
//
// Extracted from Run to keep the per-reconciler setup chain in Run
// under the cyclomatic-complexity gate.
func setupProxySecretReconciler(mgr ctrl.Manager, proxyTokenSecret, proxyDeploymentLabel string) error {
	if strings.TrimSpace(proxyTokenSecret) == "" {
		return nil
	}

	reconciler, err := NewProxySecretReconciler(mgr.GetClient(), proxyTokenSecret, proxyDeploymentLabel)
	if err != nil {
		return errors.Wrap(err, "failed to construct proxy secret reconciler")
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup proxy secret reconciler")
	}

	return nil
}

// setupProxyEndpointReconciler wires the EndpointSlice watcher that
// re-pushes the cached proxy config whenever a new proxy pod joins (or
// an old one leaves) the headless Service's endpoints. Without this, a
// pod that becomes Ready BETWEEN HTTPRoute reconciles stays at
// /readyz == 503 until the next HTTPRoute change. Issue #293.
//
// Extracted from Run so the per-reconciler setup chain in Run stays
// under the cyclomatic-complexity gate.
func setupProxyEndpointReconciler(mgr ctrl.Manager, proxySyncer *ProxySyncer, proxyEndpoints []string) error {
	reconciler := &ProxyEndpointReconciler{
		Client:         mgr.GetClient(),
		ProxySyncer:    proxySyncer,
		ProxyEndpoints: proxyEndpoints,
	}

	if err := reconciler.SetupWithManager(mgr); err != nil {
		return errors.Wrap(err, "failed to setup proxy endpoint reconciler")
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

	var syncerOpts []ProxySyncerOption
	if cfg.Tracing {
		syncerOpts = append(syncerOpts, WithSyncerTracing())
	}

	return NewProxySyncer(
		cfg.ClusterDomain,
		cfg.ProxyAuthToken,
		cfg.ControllerName,
		k8sClient,
		baseLogger,
		syncerOpts...,
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

// parseNamespaceSelector parses a kubectl label-selector string into a
// structured *metav1.LabelSelector. An empty string yields nil — "no selector"
// is a meaningful value (e.g. the monitoring allowance defaults to none).
func parseNamespaceSelector(selectorStr string) (*metav1.LabelSelector, error) {
	if strings.TrimSpace(selectorStr) == "" {
		//nolint:nilnil // nil selector is the meaningful "none configured" result
		return nil, nil
	}

	selector, err := metav1.ParseToLabelSelector(selectorStr)
	if err != nil {
		return nil, errors.Wrap(err, "parsing namespace selector")
	}

	return selector, nil
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
