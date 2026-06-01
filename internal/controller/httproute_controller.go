package controller

import (
	"context"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

const (
	// apiErrorRequeueDelay is the delay before retrying when Cloudflare API calls fail.
	apiErrorRequeueDelay = 15 * time.Second

	// startupPendingRequeueDelay is the delay before retrying when startup sync is not yet complete.
	startupPendingRequeueDelay = 1 * time.Second

	// maxIngressRules is the maximum number of ingress rules allowed per Cloudflare Tunnel.
	// Cloudflare's limit is approximately 1000 rules per tunnel.
	maxIngressRules = 1000

	// Route status messages.
	routeAcceptedMessage = "Route accepted and programmed in Cloudflare Tunnel"
	resolvedRefsMessage  = "All references resolved"

	// Priority levels for reconciliation queue.
	// Higher values = higher priority = processed first.
	// GatewayClassConfig changes are most critical and processed first.
	priorityGatewayClassConfig = 100
	priorityGateway            = 80
	priorityRoute              = 50
)

// HTTPRouteReconciler reconciles HTTPRoute resources and synchronizes them
// to Cloudflare Tunnel ingress configuration.
//
// Key behaviors:
//   - Watches all HTTPRoute resources in the cluster
//   - Filters routes by parent Gateway's GatewayClass
//   - Uses shared RouteSyncer for unified sync with GRPCRoutes
//   - Updates Cloudflare Tunnel config via API (cloudflared hot-reloads)
//   - Updates HTTPRoute status with acceptance conditions
//
// On startup, the reconciler performs a full sync to ensure tunnel configuration
// matches the current state of route resources. This means any ingress rules
// created outside of this controller will be replaced.
type HTTPRouteReconciler struct {
	client.Client

	// Scheme is the runtime scheme for API type registration.
	Scheme *runtime.Scheme

	// ControllerName identifies this controller and is used to filter
	// routes by their parent Gateway's GatewayClass controllerName.
	ControllerName string

	// RouteSyncer provides unified sync for both HTTP and GRPC routes.
	RouteSyncer *RouteSyncer

	// ProxySyncer pushes routing config to the L7 proxy replicas.
	ProxySyncer *ProxySyncer

	// ProxyEndpoints is the list of L7 proxy config-API URLs.
	ProxyEndpoints []string

	// bindingValidator validates route binding to Gateway listeners.
	bindingValidator *routebinding.Validator

	// startupComplete indicates whether the startup sync has completed.
	// This prevents race conditions between startup sync and reconcile loop.
	startupComplete atomic.Bool
}

func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return reconcileRoute(ctx, req, &gatewayv1.HTTPRoute{}, reconcileRouteParams[*gatewayv1.HTTPRoute]{
		startupComplete: &r.startupComplete,
		k8sClient:       r.Client,
		controllerName:  r.ControllerName,
		componentName:   "httproute",
		wrapRoute:       func(route *gatewayv1.HTTPRoute) Route { return HTTPRouteWrapper{route} },
		syncAndUpdate:   r.syncAndUpdateStatus,
	})
}

func (r *HTTPRouteReconciler) syncAndUpdateStatus(ctx context.Context) (ctrl.Result, error) {
	return syncAndUpdateStatusCommon(ctx, syncUpdateParams{
		routeSyncer:    r.RouteSyncer,
		proxySyncer:    r.ProxySyncer,
		proxyEndpoints: r.ProxyEndpoints,
		pushProxy:      true,
		statusEntries: func(sr *SyncResult, diags []proxy.RouteDiagnostic) []routeStatusEntry {
			return sr.httpStatusEntries(diags, r.updateRouteStatus)
		},
	})
}

// grpcRoutePtrs converts a slice of GRPCRoute values to a slice of pointers.
func grpcRoutePtrs(routes []gatewayv1.GRPCRoute) []*gatewayv1.GRPCRoute {
	out := make([]*gatewayv1.GRPCRoute, len(routes))
	for i := range routes {
		out[i] = &routes[i]
	}

	return out
}

// httpRoutePtrs converts a slice of HTTPRoute values to a slice of pointers.
func httpRoutePtrs(routes []gatewayv1.HTTPRoute) []*gatewayv1.HTTPRoute {
	ptrs := make([]*gatewayv1.HTTPRoute, len(routes))
	for idx := range routes {
		ptrs[idx] = &routes[idx]
	}

	return ptrs
}

func (r *HTTPRouteReconciler) isRouteForOurGateway(ctx context.Context, route *gatewayv1.HTTPRoute) bool {
	return IsRouteAcceptedByGateway(ctx, r.Client, r.bindingValidator, r.ControllerName, HTTPRouteWrapper{route})
}

func (r *HTTPRouteReconciler) updateRouteStatus(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	bindingInfo routeBindingInfo,
	failedRefs []ingress.BackendRefError,
	diagnostics []proxy.RouteDiagnostic,
	syncErr error,
) error {
	return updateRouteStatusGeneric(
		ctx,
		routeStatusUpdateParams{
			k8sClient:      r.Client,
			controllerName: r.ControllerName,
			diagnostics:    diagnostics,
		},
		types.NamespacedName{Name: route.Name, Namespace: route.Namespace},
		newHTTPRouteAccessor,
		bindingInfo,
		failedRefs,
		syncErr,
	)
}

func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.bindingValidator = routebinding.NewValidator(r.Client)

	return setupRouteController(mgr, &routeControllerSetupParams{
		routeObject:                &gatewayv1.HTTPRoute{},
		reconciler:                 r,
		runnable:                   r,
		k8sClient:                  r.Client,
		controllerName:             r.ControllerName,
		configResolver:             r.RouteSyncer.ConfigResolver,
		findRoutesForGateway:       r.findRoutesForGateway,
		findRoutesForListenerSet:   r.findRoutesForListenerSet,
		findRoutesForRefGrant:      r.findRoutesForReferenceGrant,
		findRoutesForService:       r.findRoutesForService,
		findRoutesForEndpointSlice: r.findRoutesForEndpointSlice,
		watchBackendTLS:            true,
		getAllRelevantRoutes:       r.getAllRelevantRoutes,
	})
}

// findRoutesForListenerSet enqueues every HTTPRoute managed by our controller
// whose parentRef targets the given ListenerSet, so the route re-binds (and
// recomputes its inherited hostnames) when the ListenerSet's listeners
// change. Routes attached directly to the parent Gateway are intentionally
// NOT enqueued here: a Gateway-bound route inherits hostnames only from the
// Gateway's own listeners, so it has no dependency on ListenerSet changes.
//
//nolint:dupl // mirrored on purpose against GRPCRouteReconciler.findRoutesForListenerSet — different list/wrapper types prevent a clean generic
func (r *HTTPRouteReconciler) findRoutesForListenerSet(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	listenerSet, ok := obj.(*gatewayv1.ListenerSet)
	if !ok {
		return nil
	}

	var routeList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routeList); err != nil {
		return nil
	}

	routes := make([]Route, len(routeList.Items))
	for i := range routeList.Items {
		routes[i] = HTTPRouteWrapper{&routeList.Items[i]}
	}

	return findRoutesAttachedToListenerSet(ctx, r.Client, listenerSet, r.ControllerName, routes)
}

// Start implements manager.Runnable for startup sync.
func (r *HTTPRouteReconciler) Start(ctx context.Context) error {
	// Mark startup as complete when this function returns,
	// regardless of success or failure
	defer r.startupComplete.Store(true)

	logger := logging.Component(ctx, "httproute-startup-sync")
	logger.Info("performing startup sync of tunnel configuration")

	ctx = logging.WithLogger(ctx, logger)

	_, err := r.syncAndUpdateStatus(ctx)
	if err != nil {
		logger.Error("startup sync failed", "error", err)
		// Don't return error - allow controller to start even if initial sync fails
	} else {
		logger.Info("startup sync completed successfully")
	}

	return nil
}

func (r *HTTPRouteReconciler) findRoutesForGateway(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var routeList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routeList); err != nil {
		return nil
	}

	routes := make([]Route, len(routeList.Items))
	for i := range routeList.Items {
		routes[i] = HTTPRouteWrapper{&routeList.Items[i]}
	}

	return FindRoutesForGateway(ctx, r.Client, obj, r.ControllerName, routes)
}

// findRoutesForService enqueues every HTTPRoute managed by our controller that
// references the given Service in any of its backendRefs. A Service-side change
// (e.g. appProtocol added to a port) must trigger a reconcile so the proxy
// config stays current. Routes belonging to other GatewayClasses are filtered
// out — mirroring findRoutesForReferenceGrant.
func (r *HTTPRouteReconciler) findRoutesForService(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var routeList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routeList); err != nil {
		return nil
	}

	routes := make([]Route, 0, len(routeList.Items))

	for i := range routeList.Items {
		route := &routeList.Items[i]
		if r.isRouteForOurGateway(ctx, route) {
			routes = append(routes, HTTPRouteWrapper{route})
		}
	}

	return FindRoutesForService(obj, routes)
}

// findRoutesForEndpointSlice enqueues every managed HTTPRoute that references
// the Service owning the changed EndpointSlice, so the proxy's
// zero-ready-endpoint 503 marking refreshes when pods become Ready/NotReady.
func (r *HTTPRouteReconciler) findRoutesForEndpointSlice(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var routeList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routeList); err != nil {
		return nil
	}

	routes := make([]Route, 0, len(routeList.Items))

	for i := range routeList.Items {
		route := &routeList.Items[i]
		if r.isRouteForOurGateway(ctx, route) {
			routes = append(routes, HTTPRouteWrapper{route})
		}
	}

	return FindRoutesForEndpointSlice(obj, routes)
}

func (r *HTTPRouteReconciler) findRoutesForReferenceGrant(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var routeList gatewayv1.HTTPRouteList

	err := r.List(ctx, &routeList)
	if err != nil {
		return nil
	}

	// Collect routes managed by our Gateway as Route
	routes := make([]Route, 0, len(routeList.Items))

	for i := range routeList.Items {
		route := &routeList.Items[i]
		if r.isRouteForOurGateway(ctx, route) {
			routes = append(routes, HTTPRouteWrapper{route})
		}
	}

	return FindRoutesForReferenceGrant(obj, routes)
}

func (r *HTTPRouteReconciler) getAllRelevantRoutes(ctx context.Context) []reconcile.Request {
	var routeList gatewayv1.HTTPRouteList

	err := r.List(ctx, &routeList)
	if err != nil {
		return nil
	}

	routes := make([]Route, len(routeList.Items))
	for i := range routeList.Items {
		routes[i] = HTTPRouteWrapper{&routeList.Items[i]}
	}

	return FilterAcceptedRoutes(ctx, r.Client, r.bindingValidator, r.ControllerName, routes)
}
