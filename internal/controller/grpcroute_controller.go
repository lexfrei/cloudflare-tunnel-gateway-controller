package controller

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// grpcQUICUnsupportedStatusMessage is the actionable GRPCRoute status message
// for an explicit quic tunnel, surfaced on the route's Accepted=False /
// UnsupportedProtocol condition. It names the incompatibility and the exact
// remediation so an operator can act without reading source.
const grpcQUICUnsupportedStatusMessage = "gRPC is not compatible with the \"quic\" tunnel transport " +
	"(cloudflared drops HTTP trailers over QUIC, so grpc-status is lost). Set proxy.tunnel.protocol " +
	"to \"http2\" (or \"auto\"/unset, which the controller will upgrade to http2 for gRPC) to enable this route."

// isExplicitQUIC reports whether the operator deliberately pinned the tunnel
// transport to quic (case-insensitive, trimmed). Only an explicit quic is a
// gRPC misconfiguration the controller flags: auto/unset is upgraded to http2
// by the proxy when a GRPCRoute is present, and http2 carries trailers.
func isExplicitQUIC(protocol string) bool {
	return strings.EqualFold(strings.TrimSpace(protocol), "quic")
}

// grpcProtocolWarning returns an operator-facing message (and true) when
// GRPCRoutes are configured AND the operator has explicitly pinned the tunnel
// transport to quic. cloudflared does not forward HTTP trailers over QUIC, so
// grpc-status never reaches the client and every gRPC call fails. This is a
// cloudflared/Cloudflare limitation, not a controller bug.
//
// auto/unset and http2 do NOT warn: http2 carries trailers, and auto/unset is
// upgraded to http2 by the proxy at startup when a GRPCRoute is present (or the
// proxy logs a restart-needed notice if the route appeared after it dialed) —
// neither is a misconfiguration the operator must act on. Only an explicit quic
// is a deliberate choice of the trailer-dropping transport. Returns ("", false)
// when there is nothing to warn about.
func grpcProtocolWarning(protocol string, grpcRouteCount int) (string, bool) {
	if grpcRouteCount == 0 {
		return "", false
	}

	if !isExplicitQUIC(protocol) {
		return "", false
	}

	return fmt.Sprintf(
		"%d GRPCRoute(s) configured but the tunnel transport protocol is explicitly %q: "+
			"cloudflared does not forward HTTP trailers over QUIC, so grpc-status is dropped at the "+
			"edge and every gRPC call fails with \"server closed the stream without sending trailers\". "+
			"Set proxy.tunnel.protocol=http2 (or auto/unset, which the proxy upgrades to http2 for gRPC). "+
			"This is a cloudflared/Cloudflare limitation, not on our side.",
		grpcRouteCount, protocol,
	), true
}

// GRPCRouteReconciler reconciles GRPCRoute resources, synchronizing them to
// both the Cloudflare Tunnel ingress configuration and the in-process L7
// proxy (which serves gRPC traffic at runtime).
//
// Key behaviors:
//   - Watches all GRPCRoute resources in the cluster
//   - Filters routes by parent Gateway's GatewayClass
//   - Uses shared RouteSyncer for unified sync with HTTPRoutes
//   - Pushes the merged HTTP+gRPC config to the proxy via ProxySyncer
//   - Updates GRPCRoute status with acceptance conditions
type GRPCRouteReconciler struct {
	client.Client

	// Scheme is the runtime scheme for API type registration.
	Scheme *runtime.Scheme

	// ControllerName identifies this controller and is used to filter
	// routes by their parent Gateway's GatewayClass controllerName.
	ControllerName string

	// RouteSyncer provides unified sync for both HTTP and GRPC routes.
	RouteSyncer *RouteSyncer

	// ProxySyncer pushes the merged HTTP+GRPC routing config to the L7
	// proxy replicas. A GRPCRoute change must re-push the proxy config so
	// gRPC traffic routes through the in-process proxy.
	ProxySyncer *ProxySyncer

	// ProxyEndpoints is the list of L7 proxy config-API URLs to push to.
	ProxyEndpoints []string

	// TunnelProtocol is the configured edge transport (auto|http2|quic). Used
	// to warn when GRPCRoutes are present on an explicit quic tunnel, where
	// cloudflared drops the grpc-status trailer. auto/unset is upgraded to http2
	// by the proxy when a GRPCRoute is present, so it is not flagged.
	TunnelProtocol string

	// bindingValidator validates route binding to Gateway listeners.
	bindingValidator *routebinding.Validator

	// startupComplete indicates whether the startup sync has completed.
	startupComplete atomic.Bool
}

func (r *GRPCRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return reconcileRoute(ctx, req, &gatewayv1.GRPCRoute{}, reconcileRouteParams[*gatewayv1.GRPCRoute]{
		startupComplete: &r.startupComplete,
		k8sClient:       r.Client,
		controllerName:  r.ControllerName,
		componentName:   "grpcroute",
		wrapRoute:       func(route *gatewayv1.GRPCRoute) Route { return GRPCRouteWrapper{route} },
		syncAndUpdate:   r.syncAndUpdateStatus,
	})
}

func (r *GRPCRouteReconciler) syncAndUpdateStatus(ctx context.Context) (ctrl.Result, error) {
	return syncAndUpdateStatusCommon(ctx, syncUpdateParams{
		routeSyncer:    r.RouteSyncer,
		proxySyncer:    r.ProxySyncer,
		proxyEndpoints: r.ProxyEndpoints,
		// GRPCRoute changes push the merged HTTP+GRPC config to the proxy so
		// gRPC traffic routes through the in-process proxy. The push rebuilds
		// from the full SyncResult, so a gRPC-only change still re-pushes every
		// route — same model as the HTTPRoute reconciler.
		pushProxy:      true,
		tunnelProtocol: r.TunnelProtocol,
		statusEntries: func(sr *SyncResult) []routeStatusEntry {
			return sr.grpcStatusEntries(r.updateRouteStatus)
		},
	})
}

func (r *GRPCRouteReconciler) isRouteForOurGateway(ctx context.Context, route *gatewayv1.GRPCRoute) bool {
	return IsRouteAcceptedByGateway(ctx, r.Client, r.bindingValidator, r.ControllerName, GRPCRouteWrapper{route})
}

func (r *GRPCRouteReconciler) updateRouteStatus(
	ctx context.Context,
	route *gatewayv1.GRPCRoute,
	bindingInfo routeBindingInfo,
	failedRefs []ingress.BackendRefError,
	syncErr error,
) error {
	params := routeStatusUpdateParams{
		k8sClient:      r.Client,
		controllerName: r.ControllerName,
	}

	// gRPC over an explicit quic tunnel cannot be served (cloudflared drops HTTP
	// trailers over QUIC, so grpc-status is lost), so surface it on the route as
	// Accepted=False / UnsupportedProtocol. auto/unset is upgraded to http2 by
	// the proxy when a GRPCRoute is present, so it is not flagged here.
	if isExplicitQUIC(r.TunnelProtocol) {
		params.acceptedOverride = &acceptedConditionOverride{
			reason:  string(gatewayv1.RouteReasonUnsupportedProtocol),
			message: grpcQUICUnsupportedStatusMessage,
		}
	}

	return updateRouteStatusGeneric(
		ctx,
		params,
		types.NamespacedName{Name: route.Name, Namespace: route.Namespace},
		newGRPCRouteAccessor,
		bindingInfo,
		failedRefs,
		syncErr,
	)
}

func (r *GRPCRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.bindingValidator = routebinding.NewValidator(r.Client)

	return setupRouteController(mgr, &routeControllerSetupParams{
		routeObject:              &gatewayv1.GRPCRoute{},
		reconciler:               r,
		runnable:                 r,
		k8sClient:                r.Client,
		controllerName:           r.ControllerName,
		configResolver:           r.RouteSyncer.ConfigResolver,
		findRoutesForGateway:     r.findRoutesForGateway,
		findRoutesForListenerSet: r.findRoutesForListenerSet,
		findRoutesForRefGrant:    r.findRoutesForReferenceGrant,
		// gRPC is proxy-driving in v3, so watch Service: a route stuck at 500
		// because its backend did not exist yet must recover when the Service
		// appears. BackendTLSPolicy IS watched (watchBackendTLS=true) — gRPC
		// backends honor a matching policy by upgrading to TLS+ALPN HTTP/2;
		// without the watch a policy create/edit would not re-converge the
		// gRPC routes and the proxy would keep dialing cleartext.
		findRoutesForService:       r.findRoutesForService,
		findRoutesForEndpointSlice: r.findRoutesForEndpointSlice,
		getAllRelevantRoutes:       r.getAllRelevantRoutes,
		watchBackendTLS:            true,
	})
}

// findRoutesForListenerSet enqueues every GRPCRoute managed by our
// controller whose parentRef targets the given ListenerSet.
//
//nolint:dupl // mirrored on purpose against HTTPRouteReconciler.findRoutesForListenerSet — different list/wrapper types prevent a clean generic
func (r *GRPCRouteReconciler) findRoutesForListenerSet(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	listenerSet, ok := obj.(*gatewayv1.ListenerSet)
	if !ok {
		return nil
	}

	var routeList gatewayv1.GRPCRouteList
	if err := r.List(ctx, &routeList); err != nil {
		return nil
	}

	routes := make([]Route, len(routeList.Items))
	for i := range routeList.Items {
		routes[i] = GRPCRouteWrapper{&routeList.Items[i]}
	}

	return findRoutesAttachedToListenerSet(ctx, r.Client, listenerSet, r.ControllerName, routes)
}

// Start implements manager.Runnable for startup sync.
func (r *GRPCRouteReconciler) Start(ctx context.Context) error {
	defer r.startupComplete.Store(true)

	logger := logging.Component(ctx, "grpcroute-startup-sync")
	logger.Info("performing startup sync of grpcroute configuration")

	ctx = logging.WithLogger(ctx, logger)

	_, err := r.syncAndUpdateStatus(ctx)
	if err != nil {
		logger.Error("grpcroute startup sync failed", "error", err)
	} else {
		logger.Info("grpcroute startup sync completed successfully")
	}

	return nil
}

func (r *GRPCRouteReconciler) findRoutesForGateway(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var routeList gatewayv1.GRPCRouteList
	if err := r.List(ctx, &routeList); err != nil {
		return nil
	}

	routes := make([]Route, len(routeList.Items))
	for i := range routeList.Items {
		routes[i] = GRPCRouteWrapper{&routeList.Items[i]}
	}

	return FindRoutesForGateway(ctx, r.Client, obj, r.ControllerName, routes)
}

func (r *GRPCRouteReconciler) findRoutesForReferenceGrant(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var routeList gatewayv1.GRPCRouteList

	err := r.List(ctx, &routeList)
	if err != nil {
		return nil
	}

	// Collect routes managed by our Gateway as Route
	routes := make([]Route, 0, len(routeList.Items))

	for i := range routeList.Items {
		route := &routeList.Items[i]
		if r.isRouteForOurGateway(ctx, route) {
			routes = append(routes, GRPCRouteWrapper{route})
		}
	}

	return FindRoutesForReferenceGrant(obj, routes)
}

// findRoutesForEndpointSlice enqueues every managed GRPCRoute that references
// the Service owning the changed EndpointSlice, so the proxy's
// zero-ready-endpoint 503 marking refreshes when pods become Ready/NotReady.
func (r *GRPCRouteReconciler) findRoutesForEndpointSlice(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var routeList gatewayv1.GRPCRouteList
	if err := r.List(ctx, &routeList); err != nil {
		return nil
	}

	routes := make([]Route, 0, len(routeList.Items))

	for i := range routeList.Items {
		route := &routeList.Items[i]
		if r.isRouteForOurGateway(ctx, route) {
			routes = append(routes, GRPCRouteWrapper{route})
		}
	}

	return FindRoutesForEndpointSlice(obj, routes)
}

// findRoutesForService enqueues every GRPCRoute managed by our controller that
// references the changed Service in a backendRef. gRPC is proxy-driving in v3,
// so a Service create must re-reconcile a route that was stuck at 500 because
// its backend did not exist yet — the same self-heal the HTTPRoute reconciler
// has. gRPC ignores Service appProtocol (it forces h2c), so this watch matters
// only for backend existence, not protocol changes.
func (r *GRPCRouteReconciler) findRoutesForService(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var routeList gatewayv1.GRPCRouteList
	if err := r.List(ctx, &routeList); err != nil {
		return nil
	}

	routes := make([]Route, 0, len(routeList.Items))

	for i := range routeList.Items {
		route := &routeList.Items[i]
		if r.isRouteForOurGateway(ctx, route) {
			routes = append(routes, GRPCRouteWrapper{route})
		}
	}

	return FindRoutesForService(obj, routes)
}

func (r *GRPCRouteReconciler) getAllRelevantRoutes(ctx context.Context) []reconcile.Request {
	var routeList gatewayv1.GRPCRouteList

	err := r.List(ctx, &routeList)
	if err != nil {
		return nil
	}

	routes := make([]Route, len(routeList.Items))
	for i := range routeList.Items {
		routes[i] = GRPCRouteWrapper{&routeList.Items[i]}
	}

	return FilterAcceptedRoutes(ctx, r.Client, r.bindingValidator, r.ControllerName, routes)
}
