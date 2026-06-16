package controller

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/zero_trust"
	"github.com/cockroachdb/errors"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/hostnameownership"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// RouteSyncer provides unified synchronization of HTTPRoute and GRPCRoute
// resources to Cloudflare Tunnel configuration.
//
// Both HTTPRouteReconciler and GRPCRouteReconciler use this to sync routes,
// ensuring that all route types are collected and synchronized together.
type RouteSyncer struct {
	client.Client

	Scheme         *runtime.Scheme
	ClusterDomain  string
	ControllerName string
	ConfigResolver *config.Resolver
	Metrics        cfmetrics.Collector
	Logger         *slog.Logger

	httpBuilder      *ingress.Builder
	grpcBuilder      *ingress.GRPCBuilder
	bindingValidator *routebinding.Validator

	// HostnameOwnership, when non-nil, enables the controller-side layer of
	// the per-namespace hostname-ownership policy (#475): a route whose
	// hostnames fall outside its namespace's allowed suffix is rejected at
	// binding time (Accepted=False/HostnameNotPermitted) and never programmed
	// into the proxy config or the Cloudflare ingress document. Independent of
	// the CEL ValidatingAdmissionPolicy by design — defence in depth: the data
	// plane stays clean even when the admission layer is absent or bypassed.
	HostnameOwnership *hostnameownership.Policy

	// ViewStore caches the per-Gateway ListenerSet merge view across reconciles
	// (issue #332). Set by the manager after construction and shared with the
	// other reconcilers. nil disables cross-reconcile reuse (per-pass dedup
	// still applies).
	ViewStore *mergeViewStore

	// syncMu protects concurrent calls to SyncAllRoutes.
	// Both HTTPRouteReconciler and GRPCRouteReconciler may call SyncAllRoutes
	// concurrently, and this mutex ensures serialized access to Cloudflare API.
	syncMu sync.Mutex

	// cloudflareClientFactory overrides how the Cloudflare API client is built
	// from the resolved credentials. nil uses the ConfigResolver's default;
	// tests inject a factory pointing at an httptest server.
	cloudflareClientFactory func(resolved *config.ResolvedConfig) *cloudflare.Client
}

// cloudflareClient builds the API client via the injected factory when set,
// the ConfigResolver default otherwise.
func (s *RouteSyncer) cloudflareClient(resolved *config.ResolvedConfig) *cloudflare.Client {
	if s.cloudflareClientFactory != nil {
		return s.cloudflareClientFactory(resolved)
	}

	return s.ConfigResolver.CreateCloudflareClient(resolved)
}

// NewRouteSyncer creates a new RouteSyncer.
func NewRouteSyncer(
	c client.Client,
	scheme *runtime.Scheme,
	clusterDomain string,
	controllerName string,
	configResolver *config.Resolver,
	metricsCollector cfmetrics.Collector,
	logger *slog.Logger,
) *RouteSyncer {
	refGrantValidator := referencegrant.NewValidator(c)

	if logger == nil {
		logger = slog.Default()
	}

	componentLogger := logger.With("component", "route-syncer")

	return &RouteSyncer{
		Client:           c,
		Scheme:           scheme,
		ClusterDomain:    clusterDomain,
		ControllerName:   controllerName,
		ConfigResolver:   configResolver,
		Metrics:          metricsCollector,
		Logger:           componentLogger,
		httpBuilder:      ingress.NewBuilder(clusterDomain, refGrantValidator, c, metricsCollector, componentLogger),
		grpcBuilder:      ingress.NewGRPCBuilder(clusterDomain, refGrantValidator, c, metricsCollector, componentLogger),
		bindingValidator: routebinding.NewValidator(c),
	}
}

// routeBindingInfo contains a route and its binding validation results per parent.
type routeBindingInfo struct {
	// bindingResults maps ParentRef index to binding result for that parent.
	bindingResults map[int]routebinding.BindingResult

	// acceptedGateways is the set of managed Gateway keys ("namespace/name")
	// this route is accepted on. It drives cross-route-type (HTTPRoute vs
	// GRPCRoute) conflict resolution: two routes of different types conflict
	// only when they share a Gateway and their hostnames intersect.
	acceptedGateways map[string]bool

	// parentGateways maps ParentRef index to the managed Gateway key
	// ("namespace/name") that parent binds to. It lets the status writer
	// attribute a tunnel-group sync failure to exactly the parents on that
	// tunnel — RouteParentStatus is per-parent, so a multi-parent route with
	// one failed and one healthy tunnel must not flip the healthy parent.
	parentGateways map[int]string

	// syncErrByGateway maps a managed Gateway key to the sync error of the
	// tunnel serving it, populated AFTER the tunnel-group sync. A parent whose
	// Gateway is absent here synced fine. Nil on the early-error path, where
	// the global syncErr applies to every parent instead.
	syncErrByGateway map[string]error
}

// hasAcceptedParent reports whether the route bound to at least one managed
// Gateway parent (the same condition bindRouteParents folds into its accepted
// return). A route in this state is programmed into the tunnel; a route with no
// accepted parent is in RejectedGRPCRoutes/RejectedHTTPRoutes with Accepted=False.
func (b routeBindingInfo) hasAcceptedParent() bool {
	for _, result := range b.bindingResults {
		if result.Accepted {
			return true
		}
	}

	return false
}

// SyncResult contains the results of a sync operation.
type SyncResult struct {
	HTTPRoutes        []gatewayv1.HTTPRoute
	GRPCRoutes        []gatewayv1.GRPCRoute
	HTTPRouteBindings map[string]routeBindingInfo // key: namespace/name
	GRPCRouteBindings map[string]routeBindingInfo // key: namespace/name
	HTTPFailedRefs    []ingress.BackendRefError   // Failed backend refs from HTTP routes
	GRPCFailedRefs    []ingress.BackendRefError   // Failed backend refs from GRPC routes

	// RejectedHTTPRoutes and RejectedGRPCRoutes are routes that reference
	// our Gateways but were not accepted by binding validation (e.g.,
	// sectionName or port mismatch). Their status must be updated with
	// Accepted=False so conformance tests can observe the rejection.
	RejectedHTTPRoutes []gatewayv1.HTTPRoute
	RejectedGRPCRoutes []gatewayv1.GRPCRoute

	// Partitions is the per-data-plane route split (#479): the shared
	// partition plus one per opted-in Gateway. The proxy push delivers each
	// partition's config to its own endpoints. Empty on early-error paths,
	// where the push falls back to treating everything as shared.
	Partitions []routePartition

	// SharedTunnelID is the class-resolved tunnel of the shared partition;
	// the proxy push uses it to detect per-Gateway partitions sharing the
	// shared tunnel (their configs must be unioned — see
	// unionPartitionRoutes).
	SharedTunnelID string
}

// httpStatusEntries builds routeStatusEntry slice for HTTP routes,
// including rejected routes that need Accepted=False status.
func (sr *SyncResult) httpStatusEntries(
	diagnostics []proxy.RouteDiagnostic,
	updateFn func(ctx context.Context, route *gatewayv1.HTTPRoute, bi routeBindingInfo, fr []ingress.BackendRefError, diags []proxy.RouteDiagnostic, se error) error,
) []routeStatusEntry {
	entries := buildStatusEntries(sr.HTTPRoutes, sr.HTTPRouteBindings, sr.HTTPFailedRefs, diagnostics, updateFn)
	// Rejected routes have no failed refs — they were rejected at binding level.
	entries = append(entries, buildStatusEntries(sr.RejectedHTTPRoutes, sr.HTTPRouteBindings, nil, nil, updateFn)...)

	return entries
}

// grpcStatusEntries builds routeStatusEntry slice for GRPC routes,
// including rejected routes that need Accepted=False status.
func (sr *SyncResult) grpcStatusEntries(
	diagnostics []proxy.RouteDiagnostic,
	updateFn func(ctx context.Context, route *gatewayv1.GRPCRoute, bi routeBindingInfo, fr []ingress.BackendRefError, diags []proxy.RouteDiagnostic, se error) error,
) []routeStatusEntry {
	entries := buildStatusEntries(sr.GRPCRoutes, sr.GRPCRouteBindings, sr.GRPCFailedRefs, diagnostics, updateFn)
	entries = append(entries, buildStatusEntries(sr.RejectedGRPCRoutes, sr.GRPCRouteBindings, nil, nil, updateFn)...)

	return entries
}

// routeObject is the constraint for Gateway API route types with Name and Namespace.
type routeObject interface {
	gatewayv1.HTTPRoute | gatewayv1.GRPCRoute
}

// buildStatusEntries creates routeStatusEntry slice from any route type.
func buildStatusEntries[T routeObject](
	routes []T,
	bindings map[string]routeBindingInfo,
	failedRefs []ingress.BackendRefError,
	diagnostics []proxy.RouteDiagnostic,
	updateFn func(ctx context.Context, route *T, bi routeBindingInfo, fr []ingress.BackendRefError, diags []proxy.RouteDiagnostic, se error) error,
) []routeStatusEntry {
	entries := make([]routeStatusEntry, 0, len(routes))

	for i := range routes {
		route := &routes[i]
		// Both HTTPRoute and GRPCRoute embed ObjectMeta which provides these methods.
		obj, ok := any(route).(interface {
			GetName() string
			GetNamespace() string
		})
		if !ok {
			continue
		}

		name := obj.GetName()
		namespace := obj.GetNamespace()
		routeKey := namespace + "/" + name

		entries = append(entries, routeStatusEntry{
			name:        name,
			namespace:   namespace,
			bindingInfo: bindings[routeKey],
			failedRefs:  filterFailedRefs(failedRefs, namespace, name),
			diagnostics: filterDiagnostics(diagnostics, namespace, name),
			update: func(ctx context.Context, bi routeBindingInfo, fr []ingress.BackendRefError, diags []proxy.RouteDiagnostic, se error) error {
				return updateFn(ctx, route, bi, fr, diags, se)
			},
		})
	}

	return entries
}

// syncUpdateParams holds the common parameters for route sync + status update.
type syncUpdateParams struct {
	routeSyncer    *RouteSyncer
	proxySyncer    *ProxySyncer
	proxyEndpoints []string
	pushProxy      bool
	// tunnelProtocol, when non-empty, enables the gRPC-over-quic warning (only
	// the GRPCRoute reconciler sets it). cloudflared drops trailers over QUIC, so
	// gRPC needs http2; auto/unset is upgraded to http2 by the proxy, so only an
	// explicit quic warns.
	tunnelProtocol string
	statusEntries  func(*SyncResult, []proxy.RouteDiagnostic) []routeStatusEntry
}

// syncAndUpdateStatusCommon performs a full route sync, pushes proxy config,
// and updates route status. Used by both HTTPRoute and GRPCRoute reconcilers.
func syncAndUpdateStatusCommon(ctx context.Context, params syncUpdateParams) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	result, syncResult, syncErr := params.routeSyncer.SyncAllRoutes(ctx)

	// Push config to the L7 proxy replicas (best-effort, non-blocking).
	// Both HTTPRoutes and GRPCRoutes are pushed: the converter maps gRPC
	// service/method matches onto /{service}/{method} path rules and forces
	// h2c upstream (internal/proxy/grpc_converter.go), so gRPC traffic routes
	// through the same in-process proxy as HTTP. The Cloudflare-side tunnel
	// ingress rules built by internal/ingress/grpc_builder are not consulted
	// at runtime in v3 — they only populate the Cloudflare dashboard's
	// edge-routing view. Both route reconcilers set pushProxy=true; each push
	// rebuilds the full merged config from the SyncResult.
	// Converter diagnostics (unsupported/dropped config surfaced on route status)
	// are produced by buildProxyConfig and returned by SyncRoutes, so they are
	// only collected on the proxy-push path below. In v3 the proxy is the sole
	// data plane and there are always proxy endpoints, so this is always taken;
	// if a deployment ever ran with zero proxy endpoints the status surfacing
	// would go dark along with the data plane itself.
	var diagnostics []proxy.RouteDiagnostic

	if params.pushProxy && params.proxySyncer != nil && len(params.proxyEndpoints) > 0 && syncResult != nil {
		diagnostics = pushPartitionConfigs(ctx, logger, params, syncResult)
	}

	// Warn when GRPCRoutes are present on an explicit quic tunnel — cloudflared
	// drops the grpc-status trailer over QUIC, so gRPC calls fail. auto/unset is
	// upgraded to http2 by the proxy, so it does not warn. Only the GRPCRoute
	// reconciler sets tunnelProtocol, so HTTP-only reconciles stay quiet.
	if params.tunnelProtocol != "" && syncResult != nil {
		if msg, warn := grpcProtocolWarning(params.tunnelProtocol, len(syncResult.GRPCRoutes)); warn {
			logger.Error(msg)
		}
	}

	var statusUpdateErr error

	if syncResult != nil {
		statusUpdateErr = updateRoutesStatus(ctx, logger, params.statusEntries(syncResult, diagnostics), syncErr)
	}

	if syncErr != nil {
		if result.RequeueAfter > 0 {
			// Specific requeue interval requested (e.g., ingress rule limit exceeded).
			// Don't propagate error — controller-runtime would override the interval.
			return result, nil
		}

		// Propagate error for controller-runtime backoff-based requeue.
		// This is intentionally different from the pre-refactor behavior which
		// swallowed errors when RequeueAfter was 0, preventing retries.
		return result, syncErr
	}

	if statusUpdateErr != nil {
		return ctrl.Result{}, statusUpdateErr
	}

	return result, nil
}

// pushPartitionConfigs delivers each partition's proxy config to its own
// data plane: the shared partition to the chart-deployed proxy endpoints
// (default auth token), each per-Gateway partition to its rendered config
// Service with its OWN token. Push failures are logged non-blocking (the
// route statuses already reflect the tunnel sync), and stale partition
// caches are evicted.
//
// A result WITHOUT partitions (the early-error paths via
// buildResultForError) pushes nothing and evicts nothing: that route set was
// never partitioned, so pushing it to the shared endpoints would serve
// tenant routes from the shared data plane — a cross-tenant leak — and
// evicting the partition caches would drop tenant replay state over a
// transient error. Every successful sync always carries at least the shared
// partition (partitionRoutes), so skipping here never starves a healthy
// plane.
func pushPartitionConfigs(
	ctx context.Context,
	logger *slog.Logger,
	params syncUpdateParams,
	syncResult *SyncResult,
) []proxy.RouteDiagnostic {
	partitions := syncResult.Partitions
	if len(partitions) == 0 {
		logger.Info("skipping proxy push: sync produced no partition split (early error)")

		return nil
	}

	// Same-tunnel partitions must push identical (unioned) configs: the edge
	// load-balances a tunnel's requests across all its connectors, so every
	// data plane on one tunnel has to know every route of that tunnel.
	partitions = unionPartitionRoutes(partitions, syncResult.SharedTunnelID)

	keep := make(map[string]bool, len(partitions))

	var diagnostics []proxy.RouteDiagnostic

	for i := range partitions {
		partition := &partitions[i]
		keep[partition.Key] = true

		var (
			diags []proxy.RouteDiagnostic
			err   error
		)

		if partition.PerGateway == nil {
			diags, err = params.proxySyncer.SyncRoutes(ctx, params.proxyEndpoints,
				httpRoutePtrs(partition.HTTPRoutes), grpcRoutePtrs(partition.GRPCRoutes),
				syncResult.HTTPFailedRefs, syncResult.GRPCFailedRefs)
		} else {
			endpoints := []string{render.ConfigEndpointURL(partition.Gateway, params.routeSyncer.ClusterDomain)}
			diags, err = params.proxySyncer.SyncPartition(ctx, partition.Key, partition.PerGateway.AuthToken,
				endpoints, httpRoutePtrs(partition.HTTPRoutes), grpcRoutePtrs(partition.GRPCRoutes),
				syncResult.HTTPFailedRefs, syncResult.GRPCFailedRefs)
		}

		// Diagnostics are valid even when the push errors: they describe the
		// route specs, not the push, and must reach the route status.
		diagnostics = append(diagnostics, diags...)

		if err != nil {
			logger.Error("proxy sync failed (non-blocking)", "partition", partition.Key, "error", err)
			params.routeSyncer.Metrics.RecordSyncError(ctx, "proxy_push")
		}
	}

	params.proxySyncer.RetainPartitions(keep)

	return diagnostics
}

// resolveConfigForController resolves configuration from the GatewayClass
// managed by this controller. Returns an error if no matching GatewayClass is found.
//
// When multiple GatewayClasses reference the same controllerName, the class
// with the lexicographically smallest name is used (deterministic ordering).
// A warning is logged because all GatewayClasses under one controller share
// the same tunnel credentials — multiple classes with different parametersRef
// may lead to unexpected behavior.
func (s *RouteSyncer) resolveConfigForController(ctx context.Context) (*config.ResolvedConfig, error) {
	classes, err := listGatewayClassesForController(ctx, s.Client, s.ControllerName)
	if err != nil {
		return nil, errors.Wrap(err, "listing GatewayClasses for config resolution")
	}

	if len(classes) == 0 {
		return nil, errors.New("no GatewayClass found for controller " + s.ControllerName)
	}

	// Sort by name for deterministic selection.
	slices.SortFunc(classes, func(a, b gatewayv1.GatewayClass) int {
		return cmp.Compare(a.Name, b.Name)
	})

	if len(classes) > 1 {
		names := make([]string, len(classes))
		for i := range classes {
			names[i] = classes[i].Name
		}

		s.Logger.Warn("multiple GatewayClasses found for controller, using first alphabetically",
			"controllerName", s.ControllerName,
			"classes", names,
			"selected", classes[0].Name,
		)

		// Different parametersRef means different tunnel credentials — using one
		// class's credentials for another class's routes would silently send
		// traffic to the wrong tunnel. Return an error to prevent data integrity issues.
		if hasConflictingParametersRef(classes) {
			return nil, errors.Wrap(
				errors.New("conflicting parametersRef across GatewayClasses"),
				fmt.Sprintf("classes %v for controller %s — "+
					"one controller instance supports only one tunnel configuration",
					names, s.ControllerName),
			)
		}
	}

	resolved, err := s.ConfigResolver.ResolveFromGatewayClass(ctx, &classes[0])
	if err != nil {
		return nil, errors.Wrap(err, "resolving config for GatewayClass "+classes[0].Name)
	}

	return resolved, nil
}

// hasConflictingParametersRef returns true if the given GatewayClasses
// reference different parametersRef (Group, Kind, or Name), indicating
// a misconfiguration.
func hasConflictingParametersRef(classes []gatewayv1.GatewayClass) bool {
	first := classes[0].Spec.ParametersRef

	for i := 1; i < len(classes); i++ {
		ref := classes[i].Spec.ParametersRef
		if !parametersRefEqual(first, ref) {
			return true
		}
	}

	return false
}

// parametersRefEqual compares two ParametersReference pointers for equality.
func parametersRefEqual(left, right *gatewayv1.ParametersReference) bool {
	if left == nil && right == nil {
		return true
	}

	if left == nil || right == nil {
		return false
	}

	return left.Group == right.Group && left.Kind == right.Kind && left.Name == right.Name
}

// buildResultForError creates a SyncResult containing all relevant routes.
// Used when early errors occur (before routes are collected) to ensure
// route statuses are updated to reflect the error.
func (s *RouteSyncer) buildResultForError(ctx context.Context) *SyncResult {
	views := newListenerViewCache(s.Client, s.ViewStore)
	httpResult, _ := s.getRelevantHTTPRoutes(ctx, views)
	grpcResult, _ := s.getRelevantGRPCRoutes(ctx, views)

	// Cross-route-type conflict resolution is intentionally NOT applied here: on
	// the config-error path a sync error drives every route to
	// Accepted=False/Pending (which dominates the Conflicted reason), so resolving
	// conflicts would only mask which routes reference us without changing any
	// surfaced status.

	result := &SyncResult{}

	if httpResult != nil {
		result.HTTPRoutes = httpResult.accepted
		result.HTTPRouteBindings = httpResult.bindings
		result.RejectedHTTPRoutes = httpResult.rejected
	}

	if grpcResult != nil {
		result.GRPCRoutes = grpcResult.accepted
		result.GRPCRouteBindings = grpcResult.bindings
		result.RejectedGRPCRoutes = grpcResult.rejected
	}

	return result
}

// SyncAllRoutes synchronizes all HTTPRoute and GRPCRoute resources to Cloudflare Tunnel.
//
//nolint:funlen // complex sync logic requires length
func (s *RouteSyncer) SyncAllRoutes(ctx context.Context) (ctrl.Result, *SyncResult, error) {
	// Serialize concurrent sync calls to prevent race conditions when
	// both HTTPRouteReconciler and GRPCRouteReconciler trigger syncs.
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	startTime := time.Now()

	// Prefer context logger (with reconcile ID) over struct logger
	logger := logging.FromContext(ctx)
	if logger == slog.Default() {
		logger = s.Logger
	}

	// Resolve configuration from the first matching GatewayClass.
	// All GatewayClasses managed by this controller share tunnel credentials.
	resolvedConfig, err := s.resolveConfigForController(ctx)
	if err != nil {
		logger.Error("failed to resolve config from GatewayClassConfig", "error", err)

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay, Priority: new(priorityRoute)}, s.buildResultForError(ctx), err
	}

	// Canonicalize the class tunnel ID so same-tunnel grouping keys on the same
	// string as a per-Gateway plane (which carries parsed.TunnelID.String()).
	// The shared ID is the raw GatewayClassConfig.tunnelID; without this, an
	// equivalent-but-differently-cased value would mis-group an infra Gateway on
	// the same physical tunnel as distinct and break the merge.
	resolvedConfig.TunnelID = canonicalTunnelID(resolvedConfig.TunnelID)

	// Create Cloudflare client with resolved credentials
	cfClient := s.cloudflareClient(resolvedConfig)

	// Resolve account ID (auto-detect if not in config) and stash it on the
	// resolved config so the shared tunnel group below skips a second lookup.
	accountID, err := s.ConfigResolver.ResolveAccountID(ctx, cfClient, resolvedConfig)
	if err != nil {
		logger.Error("failed to resolve account ID", "error", err)

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay, Priority: new(priorityRoute)}, s.buildResultForError(ctx), err
	}

	resolvedConfig.AccountID = accountID

	// One merge-view cache for the whole sync: every route's ListenerSet
	// parentRefs that resolve to the same Gateway reuse a single merge instead
	// of rebuilding it per route (issue #332).
	views := newListenerViewCache(s.Client, s.ViewStore)

	// Collect all relevant HTTPRoutes with binding validation
	httpResult, err := s.getRelevantHTTPRoutes(ctx, views)
	if err != nil {
		return ctrl.Result{}, nil, errors.Wrap(err, "failed to list httproutes")
	}

	// Collect all relevant GRPCRoutes with binding validation
	grpcResult, err := s.getRelevantGRPCRoutes(ctx, views)
	if err != nil {
		return ctrl.Result{}, nil, errors.Wrap(err, "failed to list grpcroutes")
	}

	// Resolve cross-route-type (HTTPRoute vs GRPCRoute) attachment conflicts
	// before building any config or status: the loser is moved from accepted to
	// rejected with Accepted=False/Conflicted so it is neither served nor
	// reported as accepted.
	resolveCrossTypeConflicts(httpResult, grpcResult)

	// Partition by data plane (#479): the shared plane plus one partition per
	// Gateway with a dedicated proxy + tunnel. Partition membership IS the
	// isolation guarantee — each tunnel document below sees only its
	// partition's routes.
	infra, err := s.resolveInfraGateways(ctx)
	if err != nil {
		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay, Priority: new(priorityRoute)}, s.buildResultForError(ctx), err
	}

	partitions := partitionRoutes(httpResult, grpcResult, infra)
	groups := buildTunnelGroups(resolvedConfig, partitions)

	// Two DISTINCT opted-in Gateways whose connector tokens parse to the SAME
	// tunnel collapse their isolation: the edge load-balances the tunnel's
	// requests across all connectors, so each tenant's proxy receives the
	// union of both tenants' routes. Shared+infra on one tunnel is the
	// documented migration path, but infra+infra is a silent cross-tenant
	// exposure — warn loudly so the operator sees the misconfiguration.
	for _, collision := range sharedInfraTunnelCollisions(groups) {
		logger.Error("multiple dedicated Gateways share one tunnel; their routes are unioned across tenants — "+
			"give each isolated Gateway its own Cloudflare Tunnel",
			"tunnel", collision.tunnelID, "gateways", strings.Join(collision.gateways, ","))
	}

	// A dedicated Gateway sharing the class tunnel has its credential override
	// silently dropped for the merged write (same tunnel ⇒ one credential).
	// Benign, but surface it so the operator is not surprised the override
	// has no effect.
	for _, drop := range sharedTunnelCredentialDrops(groups) {
		logger.Warn("per-Gateway Cloudflare credential override ignored: this Gateway shares the class tunnel, "+
			"so the class credential writes the merged ingress document — move it to its own tunnel to use a distinct credential",
			"tunnel", drop.tunnelID, "gateway", drop.gateway)
	}

	logger.Info("syncing routes to cloudflare",
		"httpRoutes", len(httpResult.accepted),
		"grpcRoutes", len(grpcResult.accepted),
		"partitions", partitionDisplay(partitions),
		"tunnels", len(groups),
	)

	outcome := s.syncTunnelGroups(ctx, logger, groups)

	// Attribute each failed tunnel group to exactly the parents on it: a
	// route's parent binds to a Gateway, which maps to a partition (its own,
	// for an infra Gateway; the shared partition otherwise). Per-parent
	// status precision — a multi-parent route stays Accepted on the parents
	// whose tunnel synced fine.
	injectPartitionSyncErrors(httpResult.bindings, outcome.failedPartitions, infra)
	injectPartitionSyncErrors(grpcResult.bindings, outcome.failedPartitions, infra)

	syncResult := buildSyncResult(httpResult, grpcResult, outcome.httpFailedRefs, outcome.grpcFailedRefs)
	syncResult.Partitions = partitions
	syncResult.SharedTunnelID = resolvedConfig.TunnelID

	// All groups failed: total sync outage — global error, every route goes
	// Pending (matches the historic single-tunnel failure shape).
	if len(outcome.groupErrs) == len(groups) && len(groups) > 0 {
		s.Metrics.RecordSyncDuration(ctx, "error", time.Since(startTime))

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay, Priority: new(priorityRoute)},
			syncResult, errors.Join(outcome.groupErrs...)
	}

	// Partial failure: only the failed partitions' routes carry a sync error
	// (via RouteSyncErrors) — one tenant's broken tunnel must not flip other
	// tenants' route statuses. Requeue to retry the failed tunnels.
	if len(outcome.groupErrs) > 0 {
		s.recordSyncSuccessMetrics(ctx, "partial", startTime, httpResult, grpcResult,
			len(outcome.httpFailedRefs), len(outcome.grpcFailedRefs), outcome.totalRules)

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay, Priority: new(priorityRoute)}, syncResult, nil
	}

	status := "skipped"
	if outcome.anyWritten {
		status = "success"
	}

	s.recordSyncSuccessMetrics(ctx, status, startTime, httpResult, grpcResult,
		len(outcome.httpFailedRefs), len(outcome.grpcFailedRefs), outcome.totalRules)

	return ctrl.Result{}, syncResult, nil
}

// buildSyncResult assembles the SyncResult shared by every SyncAllRoutes exit
// path that has completed route collection and rule building.
func buildSyncResult(
	httpResult *httpRouteResult,
	grpcResult *grpcRouteResult,
	httpFailedRefs, grpcFailedRefs []ingress.BackendRefError,
) *SyncResult {
	return &SyncResult{
		HTTPRoutes:         httpResult.accepted,
		GRPCRoutes:         grpcResult.accepted,
		HTTPRouteBindings:  httpResult.bindings,
		GRPCRouteBindings:  grpcResult.bindings,
		HTTPFailedRefs:     httpFailedRefs,
		GRPCFailedRefs:     grpcFailedRefs,
		RejectedHTTPRoutes: httpResult.rejected,
		RejectedGRPCRoutes: grpcResult.rejected,
	}
}

// tunnelGroup is the unit of one Cloudflare ingress-document write: every
// partition whose data plane resolves to the same tunnel. Same-tunnel
// partitions MUST merge into one document — independent writes would be
// last-writer-wins on a whole-document API.
type tunnelGroup struct {
	resolved   *config.ResolvedConfig
	partitions []*routePartition
}

// canonicalTunnelID normalizes a tunnel UUID to its canonical lowercase form so
// grouping keys are independent of the source's string form. Per-Gateway planes
// already carry uuid.UUID.String() output; this brings the raw class tunnelID to
// the same form. A value that does not parse as a UUID is returned unchanged
// (the CRD pattern should prevent that, but never silently drop an ID).
func canonicalTunnelID(id string) string {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return id
	}

	return parsed.String()
}

// buildTunnelGroups groups partitions by resolved tunnel ID. The shared
// partition uses the class-resolved config; per-Gateway partitions use the
// identity parsed from their connector tokens. Group order is deterministic:
// the shared tunnel first, the rest sorted by tunnel ID.
func buildTunnelGroups(shared *config.ResolvedConfig, partitions []routePartition) []tunnelGroup {
	byTunnel := make(map[string]*tunnelGroup)
	order := make([]string, 0, len(partitions))

	for i := range partitions {
		partition := &partitions[i]

		resolved := shared
		if partition.PerGateway != nil {
			resolved = &partition.PerGateway.ResolvedConfig
		}

		// Same tunnel ⇒ one document ⇒ one credential: the FIRST partition's
		// resolved config wins for the whole group. When an infra Gateway
		// shares the class tunnel, a GatewayConfig credential override is
		// silently ignored for that merged write — benign (same tunnel ⇒ same
		// account), and shared-tunnel use is the migration path, not isolation.
		group, ok := byTunnel[resolved.TunnelID]
		if !ok {
			group = &tunnelGroup{resolved: resolved}
			byTunnel[resolved.TunnelID] = group
			order = append(order, resolved.TunnelID)
		}

		group.partitions = append(group.partitions, partition)
	}

	// The shared partition is always first in partitions, so `order` already
	// starts with the shared tunnel; sort the remainder for determinism.
	if len(order) > 1 {
		rest := order[1:]
		slices.Sort(rest)
	}

	groups := make([]tunnelGroup, 0, len(order))
	for _, tunnelID := range order {
		groups = append(groups, *byTunnel[tunnelID])
	}

	return groups
}

// tunnelCollision names the opted-in Gateways that share one tunnel — an
// isolation-defeating misconfiguration.
type tunnelCollision struct {
	tunnelID string
	gateways []string
}

// sharedInfraTunnelCollisions reports tunnel groups holding two or more
// DISTINCT dedicated (infra) Gateways. A shared+infra group is the documented
// migration path and is NOT reported; only infra+infra — where two tenants
// each believe they have an isolated plane — is a cross-tenant exposure.
func sharedInfraTunnelCollisions(groups []tunnelGroup) []tunnelCollision {
	var collisions []tunnelCollision

	for i := range groups {
		group := &groups[i]

		var infraKeys []string

		for _, partition := range group.partitions {
			if partition.PerGateway != nil {
				infraKeys = append(infraKeys, partition.Key)
			}
		}

		if len(infraKeys) >= 2 {
			slices.Sort(infraKeys)
			collisions = append(collisions, tunnelCollision{
				tunnelID: group.resolved.TunnelID,
				gateways: infraKeys,
			})
		}
	}

	return collisions
}

// credentialOverrideDrop names a per-Gateway partition whose resolved
// Cloudflare credential differs from the credential that actually writes its
// tunnel's ingress document (the group owner's). Same tunnel ⇒ one document ⇒
// one credential, so the override is silently ignored.
type credentialOverrideDrop struct {
	tunnelID string
	gateway  string
}

// sharedTunnelCredentialDrops reports infra Gateways that share the CLASS
// tunnel (the shared partition's group) but resolve a different credential
// than the class — their GatewayConfig credential override is dropped for the
// merged write. This is benign (the same tunnel belongs to one account, so the
// account tag matches), but a silent operator surprise worth surfacing.
// infra+infra groups are NOT reported here — sharedInfraTunnelCollisions
// already flags those loudly as a cross-tenant exposure.
func sharedTunnelCredentialDrops(groups []tunnelGroup) []credentialOverrideDrop {
	var drops []credentialOverrideDrop

	for i := range groups {
		group := &groups[i]

		hasShared := false

		for _, partition := range group.partitions {
			if partition.PerGateway == nil {
				hasShared = true

				break
			}
		}

		if !hasShared {
			continue
		}

		// The shared partition is always first, so group.resolved is the class
		// credential that writes the merged document.
		for _, partition := range group.partitions {
			if partition.PerGateway == nil {
				continue
			}

			resolved := &partition.PerGateway.ResolvedConfig
			if resolved.APIToken != group.resolved.APIToken || resolved.AccountID != group.resolved.AccountID {
				drops = append(drops, credentialOverrideDrop{
					tunnelID: group.resolved.TunnelID,
					gateway:  partition.Key,
				})
			}
		}
	}

	return drops
}

// tunnelGroupsOutcome aggregates the per-group sync results.
type tunnelGroupsOutcome struct {
	httpFailedRefs []ingress.BackendRefError
	grpcFailedRefs []ingress.BackendRefError
	totalRules     int
	anyWritten     bool
	groupErrs      []error
	// failedPartitions maps a partition key (sharedPartitionKey or an infra
	// Gateway's "namespace/name") to the sync error of the tunnel serving it.
	// The per-route, per-Gateway attribution is derived from this against the
	// route bindings, so a multi-parent route reports the failure only on the
	// parents whose tunnel actually failed.
	failedPartitions map[string]error
}

// syncTunnelGroups runs the ingress-document sync for every tunnel group,
// mapping each group's failure onto exactly its partitions' routes.
func (s *RouteSyncer) syncTunnelGroups(
	ctx context.Context,
	logger *slog.Logger,
	groups []tunnelGroup,
) tunnelGroupsOutcome {
	outcome := tunnelGroupsOutcome{failedPartitions: make(map[string]error)}

	for i := range groups {
		group := &groups[i]
		result := s.syncTunnelGroup(ctx, logger, group)

		outcome.httpFailedRefs = append(outcome.httpFailedRefs, result.httpFailedRefs...)
		outcome.grpcFailedRefs = append(outcome.grpcFailedRefs, result.grpcFailedRefs...)
		outcome.totalRules += result.ruleCount

		if result.written {
			outcome.anyWritten = true
		}

		if result.err == nil {
			continue
		}

		outcome.groupErrs = append(outcome.groupErrs, result.err)

		for _, partition := range group.partitions {
			outcome.failedPartitions[partition.Key] = result.err
		}
	}

	return outcome
}

// errBrokenDataPlane marks a route parent bound to an opted-in Gateway whose
// dedicated data plane did not resolve. Such a route is served nowhere (fail
// closed), so its parent must report Accepted=False rather than silently
// claiming health — the black-hole the per-Gateway Gateway also surfaces as
// InvalidParameters.
var errBrokenDataPlane = errors.New(
	"the Gateway's dedicated data plane is unavailable (InvalidParameters); the route is not programmed")

// injectPartitionSyncErrors records, on each route binding, the sync error
// affecting each Gateway the route is accepted on, attributed per Gateway so
// the status writer flips only the affected parents. A parent is affected
// when (a) its Gateway opted into a dedicated data plane that did NOT resolve
// (broken → served nowhere), or (b) the tunnel serving its partition failed
// to sync. A parent on a healthy tunnel carries no error.
func injectPartitionSyncErrors(
	bindings map[string]routeBindingInfo,
	failedPartitions map[string]error,
	infra *infraGateways,
) {
	for key := range bindings {
		binding := bindings[key]

		var errs map[string]error

		for gatewayKey := range binding.acceptedGateways {
			err := gatewaySyncError(gatewayKey, failedPartitions, infra)
			if err == nil {
				continue
			}

			if errs == nil {
				errs = make(map[string]error)
			}

			errs[gatewayKey] = err
		}

		if errs != nil {
			binding.syncErrByGateway = errs
			bindings[key] = binding
		}
	}
}

// gatewaySyncError returns the error affecting a route accepted on gatewayKey:
// the broken-data-plane sentinel for an opted-in Gateway that failed to
// resolve, otherwise the sync error of the partition (own, or shared) serving
// it, or nil when healthy.
func gatewaySyncError(gatewayKey string, failedPartitions map[string]error, infra *infraGateways) error {
	if infra.isBroken(gatewayKey) {
		return errBrokenDataPlane
	}

	partitionKey := sharedPartitionKey
	if infra.isResolved(gatewayKey) {
		partitionKey = gatewayKey
	}

	return failedPartitions[partitionKey]
}

// tunnelGroupResult is one group's sync outcome.
type tunnelGroupResult struct {
	httpFailedRefs []ingress.BackendRefError
	grpcFailedRefs []ingress.BackendRefError
	ruleCount      int
	written        bool
	err            error
}

// groupRoutes unions the group's partition routes, deduplicated by
// namespace/name — a multi-parent route can sit in two partitions of the
// same tunnel and must contribute its rules once.
func groupRoutes(group *tunnelGroup) ([]gatewayv1.HTTPRoute, []gatewayv1.GRPCRoute) {
	seenHTTP := make(map[string]bool)
	seenGRPC := make(map[string]bool)

	var (
		httpRoutes []gatewayv1.HTTPRoute
		grpcRoutes []gatewayv1.GRPCRoute
	)

	for _, partition := range group.partitions {
		for i := range partition.HTTPRoutes {
			key := partition.HTTPRoutes[i].Namespace + "/" + partition.HTTPRoutes[i].Name
			if seenHTTP[key] {
				continue
			}

			seenHTTP[key] = true

			httpRoutes = append(httpRoutes, partition.HTTPRoutes[i])
		}

		for i := range partition.GRPCRoutes {
			key := partition.GRPCRoutes[i].Namespace + "/" + partition.GRPCRoutes[i].Name
			if seenGRPC[key] {
				continue
			}

			seenGRPC[key] = true

			grpcRoutes = append(grpcRoutes, partition.GRPCRoutes[i])
		}
	}

	return httpRoutes, grpcRoutes
}

// syncTunnelGroup builds the desired rules from EXACTLY the group's routes
// and reconciles the group's tunnel ingress document: get → diff → sort →
// catch-all → limit check → unchanged skip → whole-document update.
//
//nolint:funlen // sequential build → diff → write pipeline, mirrors the historic single-tunnel body
func (s *RouteSyncer) syncTunnelGroup(
	ctx context.Context,
	logger *slog.Logger,
	group *tunnelGroup,
) tunnelGroupResult {
	httpRoutes, grpcRoutes := groupRoutes(group)

	httpBuild := s.httpBuilder.Build(ctx, httpRoutes)
	grpcBuild := s.grpcBuilder.Build(ctx, grpcRoutes)

	result := tunnelGroupResult{
		httpFailedRefs: httpBuild.FailedRefs,
		grpcFailedRefs: grpcBuild.FailedRefs,
	}

	desiredRules := mergeAndSortRules(httpBuild.Rules, grpcBuild.Rules)

	cfClient := s.cloudflareClient(group.resolved)

	accountID, err := s.ConfigResolver.ResolveAccountID(ctx, cfClient, group.resolved)
	if err != nil {
		result.err = errors.Wrapf(err, "resolving account for tunnel %s", group.resolved.TunnelID)

		return result
	}

	// One GET per tunnel per sync (the PUT is skipped when unchanged, the GET
	// is not). Cloudflare API rate limits scale with tunnel count; fine for
	// tens of tenants, revisit with a per-tunnel config cache if a deployment
	// ever runs hundreds of dedicated planes.
	getStart := time.Now()

	currentConfig, err := cfClient.ZeroTrust.Tunnels.Cloudflared.Configurations.Get(
		ctx,
		group.resolved.TunnelID,
		zero_trust.TunnelCloudflaredConfigurationGetParams{
			AccountID: cloudflare.String(accountID),
		},
	)
	if err != nil {
		s.Metrics.RecordAPICall(ctx, "get", "tunnel_config", "error", time.Since(getStart))
		s.Metrics.RecordAPIError(ctx, "get", cfmetrics.ClassifyCloudflareError(err))
		logger.Error("failed to get current tunnel configuration",
			"tunnel", group.resolved.TunnelID, "error", err)

		result.err = err

		return result
	}

	s.Metrics.RecordAPICall(ctx, "get", "tunnel_config", "success", time.Since(getStart))

	toAdd, toRemove := ingress.DiffRules(currentConfig.Config.Ingress, desiredRules)
	logger.Info("computed diff",
		"tunnel", group.resolved.TunnelID, "toAdd", len(toAdd), "toRemove", len(toRemove))

	// ApplyDiff returns rules in arbitrary order (kept-from-current first,
	// then toAdd); wildcard rules must sort after specific hostnames to avoid
	// Cloudflare error 1056, and the catch-all must close the document.
	finalRules := ingress.EnsureCatchAll(sortIngressRules(
		ingress.ApplyDiff(currentConfig.Config.Ingress, toAdd, toRemove)))

	result.ruleCount = len(finalRules)

	if len(finalRules) > maxIngressRules {
		logger.Error("ingress rules limit exceeded",
			"tunnel", group.resolved.TunnelID, "count", len(finalRules), "max", maxIngressRules)
		s.Metrics.RecordSyncError(ctx, "limit_exceeded")

		result.err = errors.Newf("ingress rules limit exceeded for tunnel %s: %d rules (max %d)",
			group.resolved.TunnelID, len(finalRules), maxIngressRules)

		return result
	}

	// Whole-document API: skip the write when the desired document equals the
	// deployed one — steady-state reconciles hit this constantly.
	if ingress.RulesUnchanged(currentConfig.Config.Ingress, finalRules) {
		logger.Debug("tunnel configuration unchanged; skipping update",
			"tunnel", group.resolved.TunnelID, "rules", len(finalRules))

		return result
	}

	updateStart := time.Now()

	_, err = cfClient.ZeroTrust.Tunnels.Cloudflared.Configurations.Update(ctx, group.resolved.TunnelID,
		zero_trust.TunnelCloudflaredConfigurationUpdateParams{
			AccountID: cloudflare.String(accountID),
			Config: cloudflare.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
				Ingress: cloudflare.F(finalRules),
			}),
		})
	if err != nil {
		s.Metrics.RecordAPICall(ctx, "update", "tunnel_config", "error", time.Since(updateStart))
		s.Metrics.RecordAPIError(ctx, "update", cfmetrics.ClassifyCloudflareError(err))
		s.Metrics.RecordSyncError(ctx, cfmetrics.ClassifyCloudflareError(err))
		logger.Error("failed to update tunnel configuration",
			"tunnel", group.resolved.TunnelID, "error", err)

		result.err = err

		return result
	}

	s.Metrics.RecordAPICall(ctx, "update", "tunnel_config", "success", time.Since(updateStart))
	logger.Info("successfully updated tunnel configuration",
		"tunnel", group.resolved.TunnelID, "rules", len(finalRules))

	result.written = true

	return result
}

// recordSyncSuccessMetrics records the per-sync metric set shared by the
// skip path and the write path. status distinguishes a write ("success")
// from a steady-state no-op ("skipped") so the skip rate is observable.
func (s *RouteSyncer) recordSyncSuccessMetrics(
	ctx context.Context,
	status string,
	startTime time.Time,
	httpResult *httpRouteResult,
	grpcResult *grpcRouteResult,
	httpFailedRefs, grpcFailedRefs int,
	ruleCount int,
) {
	s.Metrics.RecordSyncDuration(ctx, status, time.Since(startTime))
	s.Metrics.RecordSyncedRoutes(ctx, "http", len(httpResult.accepted))
	s.Metrics.RecordSyncedRoutes(ctx, "grpc", len(grpcResult.accepted))
	s.Metrics.RecordIngressRules(ctx, ruleCount)
	s.Metrics.RecordFailedBackendRefs(ctx, "http", httpFailedRefs)
	s.Metrics.RecordFailedBackendRefs(ctx, "grpc", grpcFailedRefs)
}

// httpRouteResult holds accepted and rejected HTTPRoutes from binding validation.
type httpRouteResult struct {
	accepted []gatewayv1.HTTPRoute
	rejected []gatewayv1.HTTPRoute
	bindings map[string]routeBindingInfo
}

// grpcRouteResult holds accepted and rejected GRPCRoutes from binding validation.
type grpcRouteResult struct {
	accepted []gatewayv1.GRPCRoute
	rejected []gatewayv1.GRPCRoute
	bindings map[string]routeBindingInfo
}

//nolint:dupl // mirrored on purpose against getRelevantGRPCRoutes — different list/result types prevent a clean generic
func (s *RouteSyncer) getRelevantHTTPRoutes(
	ctx context.Context,
	views *listenerViewCache,
) (*httpRouteResult, error) {
	logger := logging.FromContext(ctx)
	if logger == slog.Default() {
		logger = s.Logger
	}

	var routeList gatewayv1.HTTPRouteList
	if err := s.List(ctx, &routeList); err != nil {
		return nil, errors.Wrap(err, "failed to list httproutes")
	}

	result := &httpRouteResult{
		bindings: make(map[string]routeBindingInfo),
	}

	for i := range routeList.Items {
		route := &routeList.Items[i]
		bindingInfo, accepted, referencesUs := s.bindRouteParents(
			ctx, logger, route.Namespace, route.Name, route.Spec.Hostnames,
			routebinding.KindHTTPRoute, route.Spec.ParentRefs, views,
		)
		result.bindings[route.Namespace+"/"+route.Name] = bindingInfo

		switch {
		case accepted:
			result.accepted = append(result.accepted, routeList.Items[i])
		case referencesUs:
			result.rejected = append(result.rejected, routeList.Items[i])
		}
	}

	return result, nil
}

//nolint:dupl // mirrored on purpose against getRelevantHTTPRoutes — different list/result types prevent a clean generic
func (s *RouteSyncer) getRelevantGRPCRoutes(
	ctx context.Context,
	views *listenerViewCache,
) (*grpcRouteResult, error) {
	logger := logging.FromContext(ctx)
	if logger == slog.Default() {
		logger = s.Logger
	}

	var routeList gatewayv1.GRPCRouteList
	if err := s.List(ctx, &routeList); err != nil {
		return nil, errors.Wrap(err, "failed to list grpcroutes")
	}

	result := &grpcRouteResult{
		bindings: make(map[string]routeBindingInfo),
	}

	for i := range routeList.Items {
		route := &routeList.Items[i]
		bindingInfo, accepted, referencesUs := s.bindRouteParents(
			ctx, logger, route.Namespace, route.Name, route.Spec.Hostnames,
			routebinding.KindGRPCRoute, route.Spec.ParentRefs, views,
		)
		result.bindings[route.Namespace+"/"+route.Name] = bindingInfo

		switch {
		case accepted:
			result.accepted = append(result.accepted, routeList.Items[i])
		case referencesUs:
			result.rejected = append(result.rejected, routeList.Items[i])
		}
	}

	return result, nil
}

// bindRouteParents walks a route's parentRefs through the shared
// resolveRouteParentBinding helper and folds the per-ref outcomes into the
// single routeBindingInfo + (accepted, referencesUs) summary that both
// HTTP/GRPC syncer paths need.
func (s *RouteSyncer) bindRouteParents(
	ctx context.Context,
	logger *slog.Logger,
	routeNamespace, routeName string,
	hostnames []gatewayv1.Hostname,
	kind gatewayv1.Kind,
	parentRefs []gatewayv1.ParentReference,
	views *listenerViewCache,
) (routeBindingInfo, bool, bool) {
	bindingInfo := routeBindingInfo{
		bindingResults:   make(map[int]routebinding.BindingResult),
		acceptedGateways: make(map[string]bool),
		parentGateways:   make(map[int]string),
	}

	hasAccepted := false
	referencesUs := false

	for refIdx, ref := range parentRefs {
		referenced, accepted := s.bindOneParent(ctx, logger, routeNamespace, routeName, hostnames, kind, ref, refIdx, views, &bindingInfo)
		referencesUs = referencesUs || referenced
		hasAccepted = hasAccepted || accepted
	}

	// Hostname-ownership enforcement (#475, controller layer) runs AFTER the
	// regular binding so its rejection replaces only otherwise-accepted
	// bindings — a route that failed binding keeps its more specific reason.
	if hasAccepted && s.HostnameOwnership != nil {
		if denied := s.rejectIfHostnameNotOwned(ctx, logger, routeNamespace, routeName, hostnames, &bindingInfo); denied {
			hasAccepted = false
		}
	}

	return bindingInfo, hasAccepted, referencesUs
}

// bindOneParent resolves and records one parentRef into bindingInfo, returning
// whether the ref references a Gateway we manage and whether it was accepted.
// parentGateways[refIdx] is recorded for every managed ref (accepted or not)
// so the status writer can attribute a per-tunnel sync failure to it.
func (s *RouteSyncer) bindOneParent(
	ctx context.Context,
	logger *slog.Logger,
	routeNamespace, routeName string,
	hostnames []gatewayv1.Hostname,
	kind gatewayv1.Kind,
	ref gatewayv1.ParentReference,
	refIdx int,
	views *listenerViewCache,
	bindingInfo *routeBindingInfo,
) (bool, bool) {
	routeInfo := &routebinding.RouteInfo{
		Name:        routeName,
		Namespace:   routeNamespace,
		Hostnames:   hostnames,
		Kind:        kind,
		SectionName: ref.SectionName,
		Port:        ref.Port,
	}

	binding, err := resolveRouteParentBinding(ctx, s.Client, s.bindingValidator, s.ControllerName, ref, routeNamespace, routeInfo, views)
	if err != nil {
		logger.Error("failed to resolve route parentRef",
			"route", routeNamespace+"/"+routeName, "refIdx", refIdx, "error", err)

		return false, false
	}

	if !binding.ManagedByThisController {
		return false, false
	}

	bindingInfo.bindingResults[refIdx] = binding.Result

	if binding.GatewayKey != "" {
		bindingInfo.parentGateways[refIdx] = binding.GatewayKey
	}

	if binding.Result.Accepted && binding.GatewayKey != "" {
		bindingInfo.acceptedGateways[binding.GatewayKey] = true
	}

	return true, binding.Result.Accepted
}

// rejectIfHostnameNotOwned evaluates the hostname-ownership policy for the
// route and, on denial, downgrades every accepted parent binding to
// Accepted=False/HostnameNotPermitted and clears the accepted-Gateway set so
// the route is excluded from the data plane. Returns true when denied.
//
// A namespace read failure also denies (fail closed): an unreadable namespace
// must not become an enforcement bypass.
func (s *RouteSyncer) rejectIfHostnameNotOwned(
	ctx context.Context,
	logger *slog.Logger,
	routeNamespace, routeName string,
	hostnames []gatewayv1.Hostname,
	bindingInfo *routeBindingInfo,
) bool {
	verdict := s.evaluateHostnameOwnership(ctx, routeNamespace, hostnames)
	if verdict.Allowed {
		return false
	}

	logger.Warn("route rejected by hostname-ownership policy",
		"route", routeNamespace+"/"+routeName, "reason", verdict.Message)

	for refIdx, result := range bindingInfo.bindingResults {
		if !result.Accepted {
			continue
		}

		bindingInfo.bindingResults[refIdx] = routebinding.BindingResult{
			Accepted: false,
			Reason:   hostnameownership.RouteReasonHostnameNotPermitted,
			Message:  verdict.Message,
		}
	}

	bindingInfo.acceptedGateways = make(map[string]bool)

	return true
}

// evaluateHostnameOwnership reads the route's namespace labels and applies
// the compiled policy. Fail closed on a namespace read error.
func (s *RouteSyncer) evaluateHostnameOwnership(
	ctx context.Context,
	routeNamespace string,
	hostnames []gatewayv1.Hostname,
) hostnameownership.Verdict {
	var namespace corev1.Namespace
	if err := s.Get(ctx, types.NamespacedName{Name: routeNamespace}, &namespace); err != nil {
		return hostnameownership.Verdict{
			Allowed: false,
			Message: fmt.Sprintf(
				"hostname-ownership policy could not read namespace %q (%v); failing closed", routeNamespace, err),
		}
	}

	return s.HostnameOwnership.Evaluate(namespace.Labels, hostnames)
}

// sortIngressRules sorts ingress rules: specific hostnames alphabetically first,
// wildcard (no hostname) last, same hostname by path length (longer first).
// This ordering is required by Cloudflare API — rules without hostname before
// rules with hostname trigger error 1056.
func sortIngressRules(
	rules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	slices.SortStableFunc(rules, func(left, right zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress) int {
		leftPresent := left.Hostname.Present
		rightPresent := right.Hostname.Present

		// Rules without hostname (wildcard) sort after rules with hostname.
		if leftPresent != rightPresent {
			if leftPresent {
				return -1
			}

			return 1
		}

		// Both have hostname or both don't — sort alphabetically, then by path length (longer first).
		if c := cmp.Compare(left.Hostname.Value, right.Hostname.Value); c != 0 {
			return c
		}

		return cmp.Compare(len(right.Path.Value), len(left.Path.Value))
	})

	return rules
}

// mergeAndSortRules combines HTTP and GRPC rules and adds catch-all.
// Rules are already sorted within each builder, but we need to merge them.
func mergeAndSortRules(
	httpRules, grpcRules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	// Remove catch-all from httpRules if present (it's added at the end anyway)
	httpFiltered := filterOutCatchAll(httpRules)
	grpcFiltered := filterOutCatchAll(grpcRules)

	// Combine all rules
	combined := make(
		[]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
		0,
		len(httpFiltered)+len(grpcFiltered)+1,
	)
	combined = append(combined, httpFiltered...)
	combined = append(combined, grpcFiltered...)

	return sortIngressRules(combined)
}

func filterOutCatchAll(
	rules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	filtered := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(rules))

	for i := range rules {
		if !ingress.IsCatchAll(ingress.RuleFromUpdate(&rules[i])) {
			filtered = append(filtered, rules[i])
		}
	}

	return filtered
}
