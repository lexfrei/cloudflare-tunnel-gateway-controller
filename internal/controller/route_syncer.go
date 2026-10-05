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
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
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

	// ProxyConfigAPIPort is the config-API port per-Gateway data planes are
	// rendered with, so their push URL names the port they listen on. Zero
	// means the proxy's default.
	ProxyConfigAPIPort int32

	httpBuilder      *ingress.Builder
	grpcBuilder      *ingress.GRPCBuilder
	bindingValidator *routebinding.Validator
	// logRepeats keeps an evaluation failure that persists across syncs from
	// being logged at its full level on every one. A pass is one sync.
	logRepeats *logging.Repeats

	// HostnameOwnership, when non-nil, enables the controller-side layer of
	// the per-namespace hostname-ownership policy (#475): a route whose
	// hostnames fall outside its namespace's allowed suffix is rejected at
	// binding time (Accepted=False/HostnameNotPermitted) and never programmed
	// into the proxy config or the Cloudflare ingress document. Independent of
	// the CEL ValidatingAdmissionPolicy by design — defence in depth: the data
	// plane stays clean even when the admission layer is absent or bypassed.
	HostnameOwnership *hostnameownership.Policy

	// Recorder reports failed tunnel document writes, which route status no
	// longer carries. Nil is a no-op.
	Recorder events.EventRecorder

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

	// claimedTunnels is every tunnel the previous sync grouped for per-Gateway
	// partitions alone, keyed by tunnel ID, with the config that writes it. A
	// tunnel that drops out of the next sync's groups is emptied with that
	// remembered config, because its own Gateway, GatewayConfig and credential
	// may be gone by then. Guarded by syncMu.
	//
	// ponytail: in memory only, so a tunnel abandoned while the controller is
	// down keeps its last document; persisting the set would close that.
	claimedTunnels map[string]claimedTunnel

	// documents is the last ingress document each tunnel was read or written
	// with, keyed by tunnel ID, so a sync whose desired document matches it
	// skips the API. Guarded by syncMu.
	documents map[string]tunnelDocument

	// now overrides the clock for the document cache (tests). Nil is
	// time.Now.
	now func() time.Time
}

// claimedTunnel is one entry of RouteSyncer.claimedTunnels.
type claimedTunnel struct {
	resolved      config.ResolvedConfig
	partitionKeys []string
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
	logRepeats := logging.NewRepeats()

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
		bindingValidator: routebinding.NewReportingValidator(c, logRepeats),
		logRepeats:       logRepeats,
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
	// attribute a data-plane refusal to exactly the parents on that Gateway —
	// RouteParentStatus is per-parent, so a multi-parent route with one
	// refused and one healthy Gateway must not flip the healthy parent.
	parentGateways map[int]string

	// parentPartitions maps ParentRef index to the key of the data-plane
	// partition serving that parent, populated once the partitions are known.
	// The status writer keeps only that partition's diagnostics on the
	// parent's entry. A parent with no entry here is served from no partition.
	parentPartitions map[int]string

	// unevaluated is true when a parentRef could not be evaluated and was
	// recorded as Pending. Nothing in the cluster changes when the read
	// recovers, so the sync that recorded it has to be retried.
	unevaluated bool

	// syncErrByGateway maps a managed Gateway key to the refusal of its
	// dedicated data plane, populated AFTER the tunnel-group sync. A parent
	// whose Gateway is absent here is served. Nil on the early-error path,
	// where the global syncErr applies to every parent instead.
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

	// TransientBrokenKeys are the partition keys of opted-in Gateways whose
	// config resolve failed transiently (retryable). They have no partition
	// this sync (fail closed), but their push cache must be RETAINED across
	// RetainPartitions so a newly-joined pod can still be replayed the last
	// config, and the reconcile requeues to re-resolve.
	TransientBrokenKeys []string

	// ConfigVersion is the proxy-config version reserved when this sync LISTED
	// its routes, carried into every partition push so version order follows
	// snapshot order. Two overlapping reconciles can otherwise build in the
	// opposite order to the one they listed in, and the older snapshot's push
	// would then win the replica's version comparison and serve stale routes.
	// Zero on early-error paths, which push nothing.
	ConfigVersion int64

	// CollisionDiagnostics are route diagnostics synthesized during sync that do
	// not come from the converter or the proxy push — currently the
	// cross-namespace tunnel-sharing collision (#488). The status path
	// concatenates them with the push diagnostics so they reach route status.
	CollisionDiagnostics []proxy.RouteDiagnostic
}

// httpStatusEntries builds routeStatusEntry slice for HTTP routes,
// including rejected routes that need Accepted=False status.
func (sr *SyncResult) httpStatusEntries(
	diagnostics []proxy.RouteDiagnostic,
	updateFn func(ctx context.Context, route *gatewayv1.HTTPRoute, bi routeBindingInfo, fr []ingress.BackendRefError, diags []proxy.RouteDiagnostic, se error) error,
) []routeStatusEntry {
	entries := buildStatusEntries(kindHTTPRouteDiag, sr.HTTPRoutes, sr.HTTPRouteBindings, sr.HTTPFailedRefs, diagnostics, updateFn)
	// Rejected routes have no failed refs — they were rejected at binding level.
	entries = append(entries, buildStatusEntries(kindHTTPRouteDiag, sr.RejectedHTTPRoutes, sr.HTTPRouteBindings, nil, nil, updateFn)...)

	return entries
}

// grpcStatusEntries builds routeStatusEntry slice for GRPC routes,
// including rejected routes that need Accepted=False status.
func (sr *SyncResult) grpcStatusEntries(
	diagnostics []proxy.RouteDiagnostic,
	updateFn func(ctx context.Context, route *gatewayv1.GRPCRoute, bi routeBindingInfo, fr []ingress.BackendRefError, diags []proxy.RouteDiagnostic, se error) error,
) []routeStatusEntry {
	entries := buildStatusEntries(kindGRPCRouteDiag, sr.GRPCRoutes, sr.GRPCRouteBindings, sr.GRPCFailedRefs, diagnostics, updateFn)
	entries = append(entries, buildStatusEntries(kindGRPCRouteDiag, sr.RejectedGRPCRoutes, sr.GRPCRouteBindings, nil, nil, updateFn)...)

	return entries
}

// routeObject is the constraint for Gateway API route types with Name and Namespace.
type routeObject interface {
	gatewayv1.HTTPRoute | gatewayv1.GRPCRoute
}

// buildStatusEntries creates routeStatusEntry slice from any route type. kind
// is the RouteDiagnostic.Kind of T, which selects the route's own diagnostics.
func buildStatusEntries[T routeObject](
	kind string,
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
			diagnostics: filterDiagnostics(diagnostics, kind, namespace, name),
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
	// onSyncError, when set, observes the sync error even on the paths where
	// the returned error is deliberately swallowed to carry a RequeueAfter
	// interval (controller-runtime overrides the interval when an error is
	// returned). The startup-sync retry loop needs the real outcome: treating
	// a swallowed resolve failure as success left the data plane configless
	// with nothing to retry (#581).
	onSyncError func(error)
	// onPushError, when set, observes proxy config push failures, which are
	// otherwise non-blocking by design. The startup-sync retry loop needs
	// them: the initial push is what makes route-less proxy replicas ready,
	// and a failed push leaves no cached config for endpoint-event resyncs to
	// replay, so ending the startup retry on a sync-succeeded/push-failed
	// attempt would re-open the #581 deadlock through the push side.
	onPushError func(error)
}

// syncAndUpdateStatusCommon performs a full route sync, pushes proxy config,
// and updates route status. Used by both HTTPRoute and GRPCRoute reconcilers.
func syncAndUpdateStatusCommon(ctx context.Context, params *syncUpdateParams) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	result, syncResult, syncErr := params.routeSyncer.SyncAllRoutes(ctx)

	// Push config to the L7 proxy replicas (best-effort, non-blocking).
	// Both HTTPRoutes and GRPCRoutes are pushed: the converter maps gRPC
	// service/method matches onto /{service}/{method} path rules and forces
	// h2c upstream (internal/proxy/grpc_converter.go), so gRPC traffic routes
	// through the same in-process proxy as HTTP. The Cloudflare-side tunnel
	// ingress rules built by internal/ingress/grpc_builder are not consulted
	// at runtime — they only populate the Cloudflare dashboard. Both route
	// reconcilers set pushProxy=true; each push rebuilds the full merged
	// config from the SyncResult.
	// Converter diagnostics (unsupported/dropped config surfaced on route status)
	// are produced by buildProxyConfig and returned by syncPartition, so they are
	// only collected on the proxy-push path below. In v3 the proxy is the sole
	// data plane and there are always proxy endpoints, so this is always taken;
	// if a deployment ever ran with zero proxy endpoints the status surfacing
	// would go dark along with the data plane itself.
	var (
		diagnostics []proxy.RouteDiagnostic
		pushed      pushOutcome
	)

	if params.pushProxy && params.proxySyncer != nil && len(params.proxyEndpoints) > 0 && syncResult != nil {
		diagnostics, pushed = pushPartitionConfigs(ctx, logger, params, syncResult)
	}

	// Fold in collision diagnostics (cross-namespace tunnel sharing, #488):
	// they are synthesized during sync, not by the converter or the push, so
	// they must be concatenated here to reach route status.
	if syncResult != nil {
		diagnostics = append(diagnostics, syncResult.CollisionDiagnostics...)
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

	if syncErr != nil && params.onSyncError != nil {
		params.onSyncError(syncErr)
	}

	return syncOutcome(result, pushed, diagnostics, syncErr, statusUpdateErr)
}

// pushOutcome is what a sync's proxy push leaves for the reconcile result.
type pushOutcome struct {
	// lostRace reports a partition push abandoned as a lost stale-version race.
	lostRace bool
	// undecided reports a partition not pushed because a backend reference
	// in it could not be evaluated.
	undecided bool
}

// syncOutcome folds a sync's push outcome into what the reconcile returns: a
// lost push race, a partition left unpushed on an undecided reference and a
// route parent that could not be evaluated each request a requeue, a sync
// error propagates unless a requeue interval is already set, and a status
// update error propagates last.
func syncOutcome(
	result ctrl.Result,
	pushed pushOutcome,
	diagnostics []proxy.RouteDiagnostic,
	syncErr, statusUpdateErr error,
) (ctrl.Result, error) {
	result = withLostRacePushRequeue(result, pushed.lostRace)

	if pushed.undecided && (result.RequeueAfter == 0 || result.RequeueAfter > apiErrorRequeueDelay) {
		result.RequeueAfter = apiErrorRequeueDelay
		result.Priority = new(priorityRoute)
	}

	if syncErr != nil {
		if result.RequeueAfter > 0 {
			// Specific requeue interval requested. Don't propagate the
			// error: controller-runtime would override the interval.
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

	return withParentNotEvaluatedRequeue(result, diagnostics), nil
}

// pushPartitionConfigs delivers each partition's proxy config to its own
// data plane: the shared partition to the chart-deployed proxy endpoints
// (default auth token), each per-Gateway partition to its rendered config
// Service with its OWN token. Push failures are logged non-blocking, and
// stale partition caches are evicted.
//
// A result WITHOUT partitions (the early-error paths via
// buildResultForError) pushes nothing and evicts nothing: that route set was
// never partitioned, so pushing it to the shared endpoints would serve
// tenant routes from the shared data plane — a cross-tenant leak — and
// evicting the partition caches would drop tenant replay state over a
// transient error. Every successful sync always carries at least the shared
// partition (partitionRoutes), so skipping here never starves a healthy
// plane.
// pushFailureSurfaceThreshold is the number of CONSECUTIVE failed pushes a
// partition must accumulate before the failure is surfaced on its routes'
// status (#487). Below it, a one-off blip (a pod rolling, a brief partition)
// stays log-only and does not flip a route condition.
const pushFailureSurfaceThreshold = 3

// maxConcurrentPartitionPushes bounds how many partition pushes run at once
// (#489). Each push already fans out per-endpoint internally, so this caps the
// outer fan-out across partitions rather than opening one connection per tenant
// on a large fleet.
const maxConcurrentPartitionPushes = 16

// partitionRouteDiagnostics stamps one RouteDiagnostic with the given target,
// reason, and message onto every route (HTTP and gRPC) in the partition's OWN
// route set. Used to surface a data-plane problem on exactly the routes a
// Gateway owns.
func partitionRouteDiagnostics(
	partition *routePartition,
	target proxy.DiagnosticTarget,
	reason, message string,
) []proxy.RouteDiagnostic {
	diags := make([]proxy.RouteDiagnostic, 0, len(partition.HTTPRoutes)+len(partition.GRPCRoutes))

	for i := range partition.HTTPRoutes {
		diags = append(diags, proxy.RouteDiagnostic{
			Kind:      kindHTTPRouteDiag,
			Namespace: partition.HTTPRoutes[i].Namespace,
			Name:      partition.HTTPRoutes[i].Name,
			Target:    target,
			Reason:    reason,
			Message:   message,
			Partition: partition.Key,
		})
	}

	for i := range partition.GRPCRoutes {
		diags = append(diags, proxy.RouteDiagnostic{
			Kind:      kindGRPCRouteDiag,
			Namespace: partition.GRPCRoutes[i].Namespace,
			Name:      partition.GRPCRoutes[i].Name,
			Target:    target,
			Reason:    reason,
			Message:   message,
			Partition: partition.Key,
		})
	}

	return diags
}

// proxyPushFailureDiagnostics synthesizes a DiagnosticProxyConfigPush for each
// route in the partition's OWN (pre-union) route set, so a sustained push
// failure surfaces on exactly the routes that Gateway owns — not on a sibling
// tenant whose config happens to ride the same unioned tunnel slice (#487).
func proxyPushFailureDiagnostics(partition *routePartition, pushErr error) []proxy.RouteDiagnostic {
	return partitionRouteDiagnostics(partition, proxy.DiagnosticProxyConfigPush, routeReasonProxyConfigPushFailed,
		proxyPushFailureMessage(partition.Key, pushErr))
}

// proxyPushFailureMessage names the config API handshake failure when that is
// what stopped the push, since no pod health check points at it.
func proxyPushFailureMessage(partitionKey string, pushErr error) string {
	message := fmt.Sprintf(
		"the controller could not push this route's config to its data plane (partition %q) after "+
			"sustained retries; matching requests are served 502 until the push recovers — check the "+
			"proxy pods' health and the config-API NetworkPolicy. This route remains Accepted.",
		partitionKey)

	if reason := describeConfigTLSError(pushErr); reason != "" {
		message += " Config API TLS: " + reason + "."
	}

	return message
}

// tunnelSharedDiagnostics synthesizes a DiagnosticTunnelShared for the routes of
// every infra Gateway that shares one Cloudflare Tunnel with another dedicated
// Gateway (#488). Sharing collapses per-Gateway isolation, so the visibility is
// on the routes, not just a log line. Reaching it across namespaces requires the
// operator's allowSharedTunnels opt-in; the claim is otherwise refused. The
// reason is across namespaces when any Gateway sharing the tunnel lives in a
// namespace other than this one's, within one namespace otherwise.
func tunnelSharedDiagnostics(collisions []tunnelCollision, partitions []routePartition) []proxy.RouteDiagnostic {
	if len(collisions) == 0 {
		return nil
	}

	byKey := make(map[string]*routePartition, len(partitions))
	for i := range partitions {
		byKey[partitions[i].Key] = &partitions[i]
	}

	var diags []proxy.RouteDiagnostic

	for _, collision := range collisions {
		for _, key := range collision.gateways {
			partition, ok := byKey[key]
			if !ok {
				continue
			}

			others := make([]string, 0, len(collision.gateways)-1)
			reason := routeReasonTunnelSharedWithinNamespace
			namespace, _, _ := strings.Cut(key, "/")

			for _, other := range collision.gateways {
				if other == key {
					continue
				}

				others = append(others, other)

				if otherNamespace, _, _ := strings.Cut(other, "/"); otherNamespace != namespace {
					reason = routeReasonTunnelSharedAcrossNamespaces
				}
			}

			message := fmt.Sprintf(
				"this route's Gateway shares Cloudflare Tunnel %q with %s; the edge load-balances the tunnel's "+
					"requests across all of them, so per-Gateway isolation is NOT guaranteed — give each isolated "+
					"Gateway its own tunnel. This route remains Accepted.",
				collision.tunnelID, strings.Join(others, ", "))

			diags = append(diags, partitionRouteDiagnostics(partition, proxy.DiagnosticTunnelShared, reason, message)...)
		}
	}

	return diags
}

func pushPartitionConfigs(
	ctx context.Context,
	logger *slog.Logger,
	params *syncUpdateParams,
	syncResult *SyncResult,
) ([]proxy.RouteDiagnostic, pushOutcome) {
	// The push belongs to the sync that built syncResult, so a failure it logs
	// is lowered while it repeats across syncs, as the binding pass lowers it.
	ctx = logging.WithRepeats(ctx, params.routeSyncer.logRepeats)

	partitions := syncResult.Partitions
	if len(partitions) == 0 {
		logger.Info("skipping proxy push: sync produced no partition split (early error)")

		return nil, pushOutcome{}
	}

	// Same-tunnel partitions must push identical (unioned) configs: the edge
	// load-balances a tunnel's requests across all its connectors, so every
	// data plane on one tunnel has to know every route of that tunnel.
	partitions = unionPartitionRoutes(partitions, syncResult.SharedTunnelID)

	keep := make(map[string]bool, len(partitions)+len(syncResult.TransientBrokenKeys))

	// A Gateway whose resolve failed transiently has no partition this sync, so
	// it would be evicted below — retain its cached config instead, so a pod
	// joining during the blip is still replayed the last good config.
	for _, key := range syncResult.TransientBrokenKeys {
		keep[key] = true
	}

	results := pushPartitionsConcurrently(ctx, params, syncResult, partitions)

	var diagnostics []proxy.RouteDiagnostic

	var pushErrs []error

	var outcome pushOutcome

	// Aggregate single-threaded in ORIGINAL partition order so diagnostics, the
	// keep set, and metrics stay deterministic regardless of push completion order.
	for i := range partitions {
		partition := &partitions[i]
		keep[partition.Key] = true

		// Diagnostics are valid even when the push errors: they describe the
		// route specs, not the push, and must reach the route status.
		diagnostics = append(diagnostics, withPartition(results[i].diags, partition.Key)...)

		if results[i].err != nil {
			pushErrs = append(pushErrs, errors.Wrapf(results[i].err, "pushing partition %s", partition.Key))
			diagnostics = append(diagnostics, partitionPushFailure(ctx, logger, params,
				&syncResult.Partitions[i], partition.Key, results[i].err, &outcome)...)
		}
	}

	if len(pushErrs) > 0 && params.onPushError != nil {
		params.onPushError(errors.Join(pushErrs...))
	}

	params.proxySyncer.RetainPartitions(keep)

	return diagnostics, outcome
}

// partitionPushFailure logs one partition's failed push, records it in
// outcome, and returns the diagnostics it surfaces on the routes of original,
// the partition before the same-tunnel union.
func partitionPushFailure(
	ctx context.Context,
	logger *slog.Logger,
	params *syncUpdateParams,
	original *routePartition,
	key string,
	err error,
	outcome *pushOutcome,
) []proxy.RouteDiagnostic {
	if errors.Is(err, errBackendRefsUndecided) {
		outcome.undecided = true

		level := params.routeSyncer.logRepeats.Level("undecided "+key, err.Error(), slog.LevelError)
		logger.Log(ctx, level, "proxy config not pushed; the data plane keeps its current config and the sync is retried",
			"partition", key, "error", err)

		return nil
	}

	logger.Error("proxy sync failed (non-blocking)", "partition", key, "error", err)
	params.routeSyncer.Metrics.RecordSyncError(ctx, "proxy_push")

	if errors.Is(err, proxy.ErrLostConfigPushRace) {
		outcome.lostRace = true
	}

	// Surface a SUSTAINED push failure on the partition's own routes once it
	// crosses the no-flap threshold (#487).
	if params.proxySyncer.pushFailureStreak(key) >= pushFailureSurfaceThreshold {
		return proxyPushFailureDiagnostics(original, err)
	}

	return nil
}

// lostRacePushRequeueDelay is how soon a sync is re-run after a partition push
// was abandoned as a lost stale-version race. In-process, config versions
// follow route-snapshot order, so an abandoned push normally carries the OLDER
// snapshot and the replica already holds the fresher config. The requeue stays
// as re-delivery insurance for the cases version ordering cannot cover: pushes
// minted without a reserved version, and cross-process races where the
// clock-seeded counters of two controller processes are not comparable — on a
// quiet cluster no further event would trigger the re-delivering sync. Short:
// the skip key is already invalidated and the re-list + rebuild takes the
// highest version, so the retry push cannot lose the same race again.
const lostRacePushRequeueDelay = 2 * time.Second

// withLostRacePushRequeue folds a lost-race push into the reconcile result:
// it requests the short re-delivery requeue without ever lengthening an
// already-shorter one, and leaves the result untouched when no race was lost.
func withLostRacePushRequeue(result ctrl.Result, lostRace bool) ctrl.Result {
	if !lostRace {
		return result
	}

	if result.RequeueAfter == 0 || result.RequeueAfter > lostRacePushRequeueDelay {
		result.RequeueAfter = lostRacePushRequeueDelay
		result.Priority = new(priorityRoute)
	}

	return result
}

// withParentNotEvaluatedRequeue requests a retry when a parent of a route
// could not be evaluated, so the route serves only the hostnames its other
// parents lend, or none. The sync that narrowed it succeeded, so without a
// requeue the missing hostnames would stay unserved until an unrelated event
// re-ran it. A pending requeue due sooner is kept.
func withParentNotEvaluatedRequeue(result ctrl.Result, diagnostics []proxy.RouteDiagnostic) ctrl.Result {
	if result.RequeueAfter > 0 && result.RequeueAfter <= apiErrorRequeueDelay {
		return result
	}

	for i := range diagnostics {
		diag := &diagnostics[i]

		if diag.Target == proxy.DiagnosticProxyConfigPush && diag.Reason == routeReasonParentNotEvaluated {
			result.RequeueAfter = apiErrorRequeueDelay

			return result
		}
	}

	return result
}

// partitionPushResult is one partition's push outcome, written to a per-index
// slot so concurrent pushes never share a slice.
type partitionPushResult struct {
	diags []proxy.RouteDiagnostic
	err   error
}

// pushPartitionsConcurrently pushes every partition's config in parallel and
// returns the per-partition results in input order. A slow connector on one
// partition must not delay the others (#489): each syncPartition
// takes syncMu only to build/record (the network push runs lock-free), so
// distinct partitions push in parallel. A push error is carried in the result,
// never returned to the group, so one failure does not cancel the others.
func pushPartitionsConcurrently(
	ctx context.Context,
	params *syncUpdateParams,
	syncResult *SyncResult,
	partitions []routePartition,
) []partitionPushResult {
	results := make([]partitionPushResult, len(partitions))

	var group errgroup.Group

	group.SetLimit(maxConcurrentPartitionPushes)

	for i := range partitions {
		partition := &partitions[i]

		group.Go(func() error {
			key, authToken, endpoints := sharedPartitionKey, params.proxySyncer.defaultAuthToken, params.proxyEndpoints

			if partition.PerGateway != nil {
				key, authToken = partition.Key, partition.PerGateway.AuthToken
				endpoints = []string{params.proxySyncer.perGatewayConfigEndpoint(partition.Gateway,
					params.routeSyncer.ClusterDomain, params.routeSyncer.ProxyConfigAPIPort)}
			}

			results[i].diags, results[i].err = params.proxySyncer.syncPartition(ctx, syncResult.ConfigVersion,
				key, authToken, endpoints, httpRoutePtrs(partition.HTTPRoutes), grpcRoutePtrs(partition.GRPCRoutes),
				syncResult.HTTPFailedRefs, syncResult.GRPCFailedRefs, partition.CertParents)

			return nil
		})
	}

	_ = group.Wait()

	return results
}

// withPartition names the partition each diagnostic was built for, so the
// status writer puts it only under the parents that partition serves.
func withPartition(diags []proxy.RouteDiagnostic, partitionKey string) []proxy.RouteDiagnostic {
	for i := range diags {
		diags[i].Partition = partitionKey
	}

	return diags
}

// resolveConfigForController resolves configuration from the GatewayClass
// managed by this controller. Returns an error if no matching GatewayClass is found.
//
// When multiple GatewayClasses reference the same controllerName, the class
// with the lexicographically smallest name among those in use is used
// (deterministic ordering). That is only sound while they all reference one
// GatewayClassConfig, which classConfigConflict enforces.
func (s *RouteSyncer) resolveConfigForController(ctx context.Context) (*config.ResolvedConfig, error) {
	classes, err := listGatewayClassesForController(ctx, s.Client, s.ControllerName)
	if err != nil {
		return nil, errors.Wrap(err, "listing GatewayClasses for config resolution")
	}

	if len(classes) == 0 {
		return nil, errors.New("no GatewayClass found for controller " + s.ControllerName)
	}

	classes, err = classesInUse(ctx, s.Client, classes)
	if err != nil {
		return nil, err
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

		if err := classConfigConflict(classes, s.ControllerName); err != nil {
			return nil, err
		}
	}

	resolved, err := s.ConfigResolver.ResolveFromGatewayClass(ctx, &classes[0])
	if err != nil {
		// The resolver names the GatewayClass on every error.
		return nil, errors.Wrap(err, "resolving GatewayClass config")
	}

	return resolved, nil
}

// managedClassConfigConflict reports, as an error marked
// config.ErrInvalidParameters, that the GatewayClasses this controller manages
// and some Gateway uses reference more than one parametersRef.
//
// Route sync writes every class's routes to the first class's tunnel, so it
// programs nothing while the classes disagree. The Gateway and infra
// reconcilers read the policy from each Gateway's own class instead, and ask
// this first so that neither accepts a Gateway, or renders a plane for it, that
// route sync will never program.
func managedClassConfigConflict(ctx context.Context, cli client.Client, controllerName string) error {
	classes, err := listGatewayClassesForController(ctx, cli, controllerName)
	if err != nil {
		return err
	}

	classes, err = classesInUse(ctx, cli, classes)
	if err != nil {
		return err
	}

	return classConfigConflict(classes, controllerName)
}

// classesInUse narrows classes to those at least one Gateway names, being
// deleted or not, and returns them all when none is named.
//
// A class no Gateway names has no routes and no plane, so its parametersRef
// changes nothing that is served, and counting it would let an unused class
// stop every Gateway on the classes that are. A Gateway naming it brings it
// back into the check on the next reconcile.
//
// With no class in use there is nothing to narrow to, and returning them all
// keeps a conflict standing: route sync would otherwise program the first
// class's tunnel, which may be one no Gateway ever used.
func classesInUse(
	ctx context.Context,
	cli client.Client,
	classes []gatewayv1.GatewayClass,
) ([]gatewayv1.GatewayClass, error) {
	if len(classes) < 2 {
		return classes, nil
	}

	var gateways gatewayv1.GatewayList
	if err := cli.List(ctx, &gateways); err != nil {
		return nil, errors.Wrap(err, "listing Gateways to find the GatewayClasses in use")
	}

	named := make(map[string]bool, len(classes))
	for i := range gateways.Items {
		named[string(gateways.Items[i].Spec.GatewayClassName)] = true
	}

	inUse := slices.DeleteFunc(slices.Clone(classes), func(class gatewayv1.GatewayClass) bool {
		return !named[class.Name]
	})
	if len(inUse) == 0 {
		return classes, nil
	}

	return inUse, nil
}

// errClassConfigConflict marks the error classConfigConflict returns, so the
// status writer can keep a shared-plane Gateway's address through it.
var errClassConfigConflict = errors.New("conflicting GatewayClass configuration")

// classConfigConflict is managedClassConfigConflict over an already-listed set.
//
// Different parametersRef means different tunnel credentials: using one class's
// credentials for another class's routes would send traffic to the wrong
// tunnel.
func classConfigConflict(classes []gatewayv1.GatewayClass, controllerName string) error {
	if len(classes) < 2 || !hasConflictingParametersRef(classes) {
		return nil
	}

	names := make([]string, len(classes))
	for i := range classes {
		names[i] = classes[i].Name
	}

	slices.Sort(names)

	// Classified rather than wrapped: wrapping ErrInvalidParameters would
	// append its text, which names a Gateway's infrastructure ref, to a
	// conflict between GatewayClasses.
	//nolint:wrapcheck // MarkInvalidParameters classifies; wrapping would add the text this avoids
	return config.MarkInvalidParameters(errors.Mark(errors.Newf(
		"conflicting parametersRef across GatewayClasses %v for controller %s: "+
			"one controller instance supports only one GatewayClassConfig",
		names, controllerName), errClassConfigConflict))
}

// hasConflictingParametersRef returns true if the given GatewayClasses
// reference different parametersRef (Group, Kind, Name, or Namespace),
// indicating a misconfiguration.
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
// Namespace counts even though GatewayClassConfig is cluster-scoped: the
// resolver refuses a ref that sets one (config.ParametersRefProblem), so
// a namespaced ref and a plain one do not resolve to the same config.
func parametersRefEqual(left, right *gatewayv1.ParametersReference) bool {
	if left == nil && right == nil {
		return true
	}

	if left == nil || right == nil {
		return false
	}

	return left.Group == right.Group && left.Kind == right.Kind && left.Name == right.Name &&
		namespacePtrEqual(left.Namespace, right.Namespace)
}

func namespacePtrEqual(left, right *gatewayv1.Namespace) bool {
	if left == nil || right == nil {
		return left == right
	}

	return *left == *right
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

	s.logRepeats.NextPass()

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
	// The account only serves the document write, so a failure here is left
	// to that write, which retries it and counts the failure.
	accountID, err := s.ConfigResolver.ResolveAccountID(ctx, cfClient, resolvedConfig)
	if err != nil {
		level := s.logRepeats.Level("account id", cfmetrics.ClassifyCloudflareError(err), slog.LevelError)
		logger.Log(ctx, level, "failed to resolve account ID; the tunnel document write will retry it", "error", err)
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

	// Reserve the proxy-config version for THIS route snapshot, while syncMu
	// still serializes us against the other route reconciler. Reserving it here
	// rather than letting the converter mint one per build is what keeps version
	// order equal to snapshot order: the push phase runs lock-free, so a slower
	// reconcile can otherwise build after a fresher one and push a higher
	// version carrying older routes.
	configVersion := proxy.NextConfigVersion()

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

	s.applyPlaneRefusals(ctx, infra, resolvedConfig)

	partitions := partitionRoutes(httpResult, grpcResult, infra)
	groups := buildTunnelGroups(resolvedConfig, partitions)

	// Two DISTINCT opted-in Gateways whose connector tokens parse to the SAME
	// tunnel collapse their isolation: the edge load-balances the tunnel's
	// requests across all connectors, so each tenant's proxy receives the
	// union of both tenants' routes. Shared+infra on one tunnel only happens
	// where the operator set allowSharedTunnels, so it is a collapse they asked
	// for; infra+infra is a silent cross-tenant exposure — warn loudly so the
	// operator sees the misconfiguration.
	collisions := sharedInfraTunnelCollisions(groups)
	for _, collision := range collisions {
		logger.Error("multiple dedicated Gateways share one tunnel; their routes are unioned across tenants — "+
			"give each isolated Gateway its own Cloudflare Tunnel",
			"tunnel", collision.tunnelID, "gateways", strings.Join(collision.gateways, ","))
	}

	collisionDiagnostics := tunnelSharedDiagnostics(collisions, partitions)

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
	s.emptyAbandonedTunnels(ctx, logger, groups, infra, &outcome)

	// Only data-plane refusals reach route status. A failed document write
	// does not: the edge routes a hostname by its DNS record and the proxy
	// serves it, so the document only feeds the dashboard.
	injectPlaneRefusals(httpResult.bindings, infra)
	injectPlaneRefusals(grpcResult.bindings, infra)
	assignParentPartitions(httpResult.bindings, infra)
	assignParentPartitions(grpcResult.bindings, infra)

	syncResult := buildSyncResult(httpResult, grpcResult, outcome.httpFailedRefs, outcome.grpcFailedRefs)
	syncResult.ConfigVersion = configVersion
	syncResult.Partitions = partitions
	syncResult.SharedTunnelID = resolvedConfig.TunnelID
	syncResult.TransientBrokenKeys = infra.transientKeys()
	syncResult.CollisionDiagnostics = collisionDiagnostics

	// A failed document write is retried, and is never a sync error.
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

	// A transient infra-resolve failure left a Gateway's routes unprogrammed
	// (fail closed) but is retryable — requeue so the next sync re-resolves and
	// programs them, rather than waiting for an unrelated event. A failed
	// emptying write needs the same, or a quiet cluster keeps the stale document.
	if leftForRetry(syncResult, &outcome) {
		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay, Priority: new(priorityRoute)}, syncResult, nil
	}

	return ctrl.Result{}, syncResult, nil
}

// leftForRetry reports work a sync left that no watched event brings back: a
// Gateway whose config failed to resolve transiently, an abandoned tunnel
// whose emptying write failed, a route parent that could not be evaluated, or
// a backend reference whose ReferenceGrants could not be read.
func leftForRetry(syncResult *SyncResult, outcome *tunnelGroupsOutcome) bool {
	return len(syncResult.TransientBrokenKeys) > 0 || outcome.emptyingPending ||
		anyUnevaluated(syncResult.HTTPRouteBindings) || anyUnevaluated(syncResult.GRPCRouteBindings) ||
		refsUndecided(syncResult.HTTPFailedRefs, nil) || refsUndecided(syncResult.GRPCFailedRefs, nil)
}

// anyUnevaluated reports whether any route has a parent recorded as Pending
// because it could not be evaluated.
func anyUnevaluated(bindings map[string]routeBindingInfo) bool {
	for key := range bindings {
		if bindings[key].unevaluated {
			return true
		}
	}

	return false
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
		// silently ignored for that merged write — benign, since the same
		// tunnel means the same account. A dedicated plane only reaches this
		// grouping at all under allowSharedTunnels; the class tunnel is
		// otherwise refused to it.
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

// emptyAbandonedTunnels records the tunnels this sync grouped and empties the
// ingress document of every tunnel the previous sync grouped and this one did
// not. Only the tunnels that still have a partition are written above, so
// without this a tunnel losing its last partition (opt-out, refusal, Gateway
// deletion, token moved to another tunnel) keeps its last rules at the edge.
//
// A tunnel that carried the shared partition is never recorded, so never
// emptied: the class tunnel belongs to the operator, and a changed class
// tunnelID must leave the old document serving until the shared proxy moves.
//
// A tunnel whose previous partitions include a Gateway that currently fails to
// resolve, and was not refused, is kept as it is: that Gateway's plane keeps
// running on its last-good config, so its document must too. A failed emptying
// write is kept for the next sync and recorded as pending on outcome so the
// caller requeues, unless Cloudflare refuses it outright (auth or a 4xx such as
// a deleted tunnel), which no retry changes. A successful emptying write counts
// as a write on outcome.
func (s *RouteSyncer) emptyAbandonedTunnels(
	ctx context.Context,
	logger *slog.Logger,
	groups []tunnelGroup,
	infra *infraGateways,
	outcome *tunnelGroupsOutcome,
) {
	previous := s.claimedTunnels
	s.claimedTunnels = make(map[string]claimedTunnel, len(groups))
	grouped := make(map[string]bool, len(groups))

	for i := range groups {
		grouped[groups[i].resolved.TunnelID] = true

		keys := make([]string, 0, len(groups[i].partitions))
		for _, partition := range groups[i].partitions {
			keys = append(keys, partition.Key)
		}

		if slices.Contains(keys, sharedPartitionKey) {
			continue
		}

		s.claimedTunnels[groups[i].resolved.TunnelID] = claimedTunnel{resolved: *groups[i].resolved, partitionKeys: keys}
	}

	for tunnelID, tunnel := range previous {
		// Checked against every group, the shared one included: a class
		// tunnelID moved onto this tunnel had its document written above.
		if grouped[tunnelID] {
			continue
		}

		if slices.ContainsFunc(tunnel.partitionKeys, infra.keepsLastPlane) {
			s.claimedTunnels[tunnelID] = tunnel

			continue
		}

		result := s.syncTunnelGroup(ctx, logger, &tunnelGroup{resolved: &tunnel.resolved})
		if result.err == nil {
			delete(s.documents, tunnelID)

			if result.written {
				outcome.anyWritten = true

				logger.Info("emptied the ingress document of a tunnel no Gateway claims any more", "tunnel", tunnelID)
			}

			continue
		}

		if s.retryEmptying(ctx, logger, tunnelID, result.err) {
			s.claimedTunnels[tunnelID] = tunnel
			outcome.emptyingPending = true
		}
	}
}

// retryEmptying counts and logs a failed emptying write and reports whether
// it is worth retrying: a write Cloudflare refused outright is not.
func (s *RouteSyncer) retryEmptying(ctx context.Context, logger *slog.Logger, tunnelID string, err error) bool {
	errorType := cfmetrics.ClassifyCloudflareError(err)
	s.Metrics.RecordSyncError(ctx, errorType)

	if errorType == cfmetrics.ErrorTypeAuth || errorType == cfmetrics.ErrorTypeClientError {
		logger.Error("could not empty the ingress document of a tunnel no Gateway claims any more; giving up",
			"tunnel", tunnelID, "error", err)

		return false
	}

	logger.Error("could not empty the ingress document of a tunnel no Gateway claims any more; retrying on the next sync",
		"tunnel", tunnelID, "error", err)

	return true
}

// tunnelCollision names the opted-in Gateways that share one tunnel — an
// isolation-defeating misconfiguration.
type tunnelCollision struct {
	tunnelID string
	gateways []string
}

// sharedInfraTunnelCollisions reports tunnel groups holding two or more
// DISTINCT dedicated (infra) Gateways. A shared+infra group is NOT reported: a
// dedicated plane only lands on the class tunnel where the operator set
// allowSharedTunnels, so the collapse is one they asked for. Only infra+infra —
// where two tenants each believe they have an isolated plane — is a cross-tenant
// exposure.
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
			// An empty class account is one that failed to resolve this sync,
			// not a different account.
			accountDiffers := group.resolved.AccountID != "" && resolved.AccountID != group.resolved.AccountID
			if resolved.APIToken != group.resolved.APIToken || accountDiffers {
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
	// emptyingPending reports an abandoned tunnel whose emptying write failed
	// and is kept for a retry.
	emptyingPending bool
}

// syncTunnelGroups runs the ingress-document sync for every tunnel group.
func (s *RouteSyncer) syncTunnelGroups(
	ctx context.Context,
	logger *slog.Logger,
	groups []tunnelGroup,
) tunnelGroupsOutcome {
	var outcome tunnelGroupsOutcome

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

		s.Metrics.RecordSyncError(ctx, cfmetrics.ClassifyCloudflareError(result.err))
		s.reportDocumentWriteFailure(ctx, logger, group, result.err)
		outcome.groupErrs = append(outcome.groupErrs, result.err)
	}

	return outcome
}

// Event for a tunnel document write that failed.
const (
	eventReasonTunnelDocumentWriteFailed = "TunnelDocumentWriteFailed"
	eventActionWriteTunnelDocument       = "WriteTunnelDocument"
)

// eventNoteLimit is the API server's limit on an events.k8s.io note.
const eventNoteLimit = 1024

// reportDocumentWriteFailure logs a failed tunnel document write, once for as
// long as it keeps failing the same way, and emits a Warning Event on every
// Gateway served from that tunnel. A same-tunnel group reports on each.
func (s *RouteSyncer) reportDocumentWriteFailure(ctx context.Context, logger *slog.Logger, group *tunnelGroup, err error) {
	tunnelID := group.resolved.TunnelID
	level := s.logRepeats.Level("tunnel document "+tunnelID, cfmetrics.ClassifyCloudflareError(err), slog.LevelError)
	logger.Log(ctx, level, "writing the tunnel ingress document failed; routes keep serving and the write is retried",
		"tunnel", tunnelID, "error", err)

	if s.Recorder == nil {
		return
	}

	note := "writing the ingress document of tunnel " + tunnelID + " failed: " + err.Error() +
		"; routes keep serving and the write is retried"
	if len(note) > eventNoteLimit {
		note = strings.ToValidUTF8(note[:eventNoteLimit], "")
	}

	for _, gateway := range s.groupGateways(ctx, logger, group) {
		s.Recorder.Eventf(gateway, nil, corev1.EventTypeWarning,
			eventReasonTunnelDocumentWriteFailed, eventActionWriteTunnelDocument, "%s", note)
	}
}

// groupGateways returns the Gateways a tunnel group serves: each dedicated
// partition's own, and every shared-plane Gateway for the shared partition.
func (s *RouteSyncer) groupGateways(ctx context.Context, logger *slog.Logger, group *tunnelGroup) []*gatewayv1.Gateway {
	var gateways []*gatewayv1.Gateway

	for _, partition := range group.partitions {
		if partition.Gateway != nil {
			gateways = append(gateways, partition.Gateway)

			continue
		}

		shared, err := managedGateways(ctx, s.Client, s.ControllerName, false)
		if err != nil {
			logger.Debug("could not list the shared-plane Gateways to report a failed write on", "error", err)

			continue
		}

		gateways = append(gateways, shared...)
	}

	return gateways
}

// errBrokenDataPlane marks a route parent bound to an opted-in Gateway whose
// dedicated data plane did not resolve. Such a route is served nowhere (fail
// closed), so its parent must report Accepted=False rather than silently
// claiming health — the black-hole the per-Gateway Gateway also surfaces as
// InvalidParameters.
// The route condition that carries this carries Reason=Pending (the spec's
// RouteConditionReason enum has no InvalidParameters member — that is a
// Gateway-level reason). So the message attributes InvalidParameters to the
// GATEWAY rather than reading as if it were the route's own reason.
var errBrokenDataPlane = errors.New(
	"the Gateway's dedicated data plane is unavailable; the route is not programmed " +
		"(see the Gateway's Accepted condition, which reports InvalidParameters)")

// injectPlaneRefusals records, on each route binding, the error of each
// Gateway the route is accepted on whose dedicated data plane was refused or
// did not resolve, so its routes are served nowhere. It is attributed per
// Gateway so the status writer flips only the affected parents.
func injectPlaneRefusals(
	bindings map[string]routeBindingInfo,
	infra *infraGateways,
) {
	for key := range bindings {
		binding := bindings[key]

		var errs map[string]error

		for gatewayKey := range binding.acceptedGateways {
			err := gatewayPlaneError(gatewayKey, infra)
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

// applyPlaneRefusals drops, BEFORE partitioning, every Gateway that may not
// have the dedicated data plane it asked for: one claiming a tunnel another
// namespace already serves, and one whose namespace is at the operator's cap.
//
// Both must run here rather than at partition time. A fabricated tunnel claim
// would otherwise collect the incumbent's routes and inject its own into the
// incumbent's document; a Gateway over the cap gets no plane rendered, so
// building and pushing its config would program routes onto a data plane that
// does not exist.
func (s *RouteSyncer) applyPlaneRefusals(
	ctx context.Context,
	infra *infraGateways,
	resolvedConfig *config.ResolvedConfig,
) {
	claims := collectTunnelClaims(ctx, infra.listed, s.ConfigResolver, resolvedConfig.TunnelID)

	applyTunnelOwnership(infra, resolvedConfig.TunnelID, resolvedConfig.AllowSharedTunnels, claims)

	// The cap comes from the FIRST managed class here, while the Gateway and
	// infra reconcilers read it from each Gateway's own class. Not a divergence:
	// every layer refuses through classConfigConflict over the managed classes
	// in use when they carry different parametersRef, so whenever any of them
	// admits a Gateway, every managed class in use resolves the same
	// GatewayClassConfig. Serving several GatewayClassConfigs from one
	// controller means reconciling these two readings first, here and for
	// allowSharedTunnels above.
	applyDataPlaneQuota(infra, resolvedConfig.MaxDataPlanesPerNamespace, collectDataPlaneClaims(infra.listed))
}

// gatewayPlaneError returns the refusal affecting a route accepted on
// gatewayKey: a refused tunnel claim, a namespace over its data-plane quota,
// or the broken-data-plane sentinel for an opted-in Gateway that failed to
// resolve; nil when the Gateway is served.
func gatewayPlaneError(gatewayKey string, infra *infraGateways) error {
	// A refused tunnel claim is checked before the generic broken case: both
	// fail closed, but "your data plane is unavailable" would send the tenant
	// hunting an outage instead of fixing the tunnel their Gateway named.
	if rejection, ok := infra.tunnelRejection(gatewayKey); ok {
		if rejection.Unproven {
			return errors.New(unprovenClaimRouteMessage(rejection))
		}

		return errors.New("the Gateway claims tunnel " + rejection.TunnelID +
			", which it does not own" + rejectionHolderSuffix(rejection) +
			"; the route is not programmed (see the Gateway's Accepted condition)")
	}

	// Checked before the generic broken case for the same reason as a refused
	// claim: "your data plane is unavailable" would send the tenant hunting an
	// outage instead of deleting a Gateway or asking the operator for headroom.
	if capacity, ok := infra.quotaRefusal(gatewayKey); ok {
		return errors.New("the Gateway's namespace " + dataPlaneQuotaLimit(capacity) +
			"; the route is not programmed (see the Gateway's Accepted condition)")
	}

	if infra.isBroken(gatewayKey) {
		return errBrokenDataPlane
	}

	return nil
}

// partitionKeyForGateway names the partition a Gateway that is not broken is
// served from: its own for a resolved dedicated Gateway, the shared one
// otherwise.
func partitionKeyForGateway(gatewayKey string, infra *infraGateways) string {
	if infra.isResolved(gatewayKey) {
		return gatewayKey
	}

	return sharedPartitionKey
}

// assignParentPartitions records, on each route binding, the partition every
// managed parent is served from, so the status writer can keep each partition's
// diagnostics on its own parents. A parent on a broken dedicated Gateway is
// served from no partition and gets no entry.
func assignParentPartitions(bindings map[string]routeBindingInfo, infra *infraGateways) {
	for key := range bindings {
		binding := bindings[key]
		binding.parentPartitions = make(map[int]string, len(binding.parentGateways))

		for refIdx, gatewayKey := range binding.parentGateways {
			if infra.isBroken(gatewayKey) {
				continue
			}

			binding.parentPartitions[refIdx] = partitionKeyForGateway(gatewayKey, infra)
		}

		bindings[key] = binding
	}
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
// and reconciles the group's tunnel ingress document: cached skip → get →
// diff → sort → catch-all → unchanged skip → whole-document update.
//
//nolint:funlen // sequential build → diff → write pipeline, mirrors the historic single-tunnel body
func (s *RouteSyncer) syncTunnelGroup(
	ctx context.Context,
	logger *slog.Logger,
	group *tunnelGroup,
) tunnelGroupResult {
	httpRoutes, grpcRoutes := groupRoutes(group)

	// A grant read failure the builders log is lowered while it repeats.
	ctx = logging.WithRepeats(ctx, s.logRepeats)

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

	cacheKey := documentKey(group.resolved, accountID)
	if desiredDocument := ingress.EnsureCatchAll(desiredRules); s.documentDeployed(cacheKey, desiredDocument) {
		s.Metrics.RecordAPICallSkipped(ctx, "get", "tunnel_config")

		result.ruleCount = len(desiredDocument)

		return result
	}

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
		delete(s.documents, group.resolved.TunnelID)

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

	// Whole-document API: skip the write when the desired document equals the
	// deployed one — steady-state reconciles hit this constantly.
	if ingress.RulesUnchanged(currentConfig.Config.Ingress, finalRules) {
		logger.Debug("tunnel configuration unchanged; skipping update",
			"tunnel", group.resolved.TunnelID, "rules", len(finalRules))
		s.storeDocument(cacheKey, finalRules)

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
		delete(s.documents, group.resolved.TunnelID)

		result.err = err

		return result
	}

	s.Metrics.RecordAPICall(ctx, "update", "tunnel_config", "success", time.Since(updateStart))
	logger.Info("successfully updated tunnel configuration",
		"tunnel", group.resolved.TunnelID, "rules", len(finalRules))

	result.written = true

	s.storeDocument(cacheKey, finalRules)

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
// so the status writer can attribute a data-plane refusal to it.
//
// A ref that could not be evaluated is recorded as not accepted and counted as
// referencing us, so the route's status is written even when that is its only
// parent, and the status writer reports the ref Pending rather than falling
// back to Accepted=True for a parent the route is not bound to. The status
// writer only writes entries for refs that select a managed Gateway, so a ref
// that turns out to be foreign gets no entry, and one whose Gateway it cannot
// read either keeps its existing entry. The two differ on purpose: here the
// route is really not programmed on this parent in this sync, so Pending is
// what happened, while a status pass that only fails its own read learns
// nothing new about the parent.
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
		route := routeNamespace + "/" + routeName
		level := s.logRepeats.Level(fmt.Sprintf("%s %s/%d", kind, route, refIdx), err.Error(), slog.LevelError)

		logger.Log(ctx, level, "failed to resolve route parentRef", "route", route, "refIdx", refIdx, "error", err)

		bindingInfo.bindingResults[refIdx] = unevaluatedParentResult()
		bindingInfo.unevaluated = true

		return true, false
	}

	if !binding.ManagedByThisController {
		return false, false
	}

	if binding.Result.Incomplete && !binding.Result.Accepted {
		// A listener that could not be evaluated may still admit the route.
		binding.Result = unevaluatedParentResult()
	}

	bindingInfo.bindingResults[refIdx] = binding.Result
	bindingInfo.unevaluated = bindingInfo.unevaluated || binding.Result.Incomplete

	if binding.GatewayKey != "" {
		bindingInfo.parentGateways[refIdx] = binding.GatewayKey
	}

	if binding.Result.Accepted && binding.GatewayKey != "" {
		bindingInfo.acceptedGateways[binding.GatewayKey] = true
	}

	return true, binding.Result.Accepted
}

// unevaluatedParentResult is the binding recorded for a parent the controller
// could not evaluate. The error stays in the log: it can quote the parent
// Gateway's spec, which the route's authors may not be allowed to read.
func unevaluatedParentResult() routebinding.BindingResult {
	return routebinding.BindingResult{
		Accepted:   false,
		Incomplete: true,
		Reason:     gatewayv1.RouteReasonPending,
		Message:    "The controller could not evaluate this parent; the controller log names the error",
	}
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
// wildcard (no hostname) last. This ordering is required by Cloudflare API —
// rules without hostname before rules with hostname trigger error 1056.
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

		return cmp.Compare(left.Hostname.Value, right.Hostname.Value)
	})

	return rules
}

// mergeAndSortRules combines the HTTP and GRPC builders' rules into one
// document body without the catch-all, which is added at the end anyway. A
// hostname both builders list stays one rule, naming the smaller backend URL
// as each builder does within its own routes.
func mergeAndSortRules(
	httpRules, grpcRules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	combined := slices.Concat(filterOutCatchAll(httpRules), filterOutCatchAll(grpcRules))
	merged := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(combined))
	byHostname := make(map[string]int, len(combined))

	for i := range combined {
		rule := &combined[i]
		if !rule.Hostname.Present {
			merged = append(merged, *rule)

			continue
		}

		idx, seen := byHostname[rule.Hostname.Value]
		if !seen {
			byHostname[rule.Hostname.Value] = len(merged)
			merged = append(merged, *rule)

			continue
		}

		if rule.Service.Value < merged[idx].Service.Value {
			merged[idx] = *rule
		}
	}

	return sortIngressRules(merged)
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
