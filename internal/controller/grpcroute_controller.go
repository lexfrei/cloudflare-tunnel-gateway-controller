package controller

import (
	"context"
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

// GRPCRouteReconciler reconciles GRPCRoute resources and synchronizes them
// to Cloudflare Tunnel ingress configuration.
//
// Key behaviors:
//   - Watches all GRPCRoute resources in the cluster
//   - Filters routes by parent Gateway's GatewayClass
//   - Uses shared RouteSyncer for unified sync with HTTPRoutes
//   - Updates GRPCRoute status with acceptance conditions
type GRPCRouteReconciler struct {
	client.Client

	// Scheme is the runtime scheme for API type registration.
	Scheme *runtime.Scheme

	// GatewayClassName filters which routes to process.
	GatewayClassName string

	// ControllerName is reported in GRPCRoute status.
	ControllerName string

	// RouteSyncer provides unified sync for both HTTP and GRPC routes.
	RouteSyncer *RouteSyncer

	// bindingValidator validates route binding to Gateway listeners.
	bindingValidator *routebinding.Validator

	// startupComplete indicates whether the startup sync has completed.
	startupComplete atomic.Bool
}

func (r *GRPCRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return reconcileRoute(ctx, req, &gatewayv1.GRPCRoute{}, reconcileRouteParams[*gatewayv1.GRPCRoute]{
		startupComplete:  &r.startupComplete,
		k8sClient:        r.Client,
		bindingValidator: r.bindingValidator,
		gatewayClassName: r.GatewayClassName,
		componentName:    "grpcroute",
		wrapRoute:        func(route *gatewayv1.GRPCRoute) Route { return GRPCRouteWrapper{route} },
		syncAndUpdate:    r.syncAndUpdateStatus,
	})
}

func (r *GRPCRouteReconciler) syncAndUpdateStatus(ctx context.Context) (ctrl.Result, error) {
	logger := logging.FromContext(ctx)

	result, syncResult, syncErr := r.RouteSyncer.SyncAllRoutes(ctx)

	// Note: ProxySyncer is not used here because it only collects HTTPRoutes.
	// GRPCRoute proxy support will be added when the proxy converter supports gRPC.

	// Update status for all GRPC routes with per-parent binding results
	var statusUpdateErr error

	if syncResult != nil {
		statusUpdateErr = updateRoutesStatus(ctx, logger, syncResult.grpcStatusEntries(r.updateRouteStatus), syncErr)
	}

	if syncErr != nil && result.RequeueAfter == 0 {
		// Don't propagate error for limit exceeded (no requeue needed)
		return result, nil
	}

	// Return error if status updates failed - controller-runtime will requeue with backoff
	if statusUpdateErr != nil {
		return ctrl.Result{}, statusUpdateErr
	}

	return result, nil
}

func (r *GRPCRouteReconciler) isRouteForOurGateway(ctx context.Context, route *gatewayv1.GRPCRoute) bool {
	return IsRouteAcceptedByGateway(ctx, r.Client, r.bindingValidator, r.GatewayClassName, GRPCRouteWrapper{route})
}

func (r *GRPCRouteReconciler) updateRouteStatus(
	ctx context.Context,
	route *gatewayv1.GRPCRoute,
	bindingInfo routeBindingInfo,
	failedRefs []ingress.BackendRefError,
	syncErr error,
) error {
	return updateRouteStatusGeneric(
		ctx,
		routeStatusUpdateParams{
			k8sClient:        r.Client,
			gatewayClassName: r.GatewayClassName,
			controllerName:   r.ControllerName,
		},
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
		routeObject:           &gatewayv1.GRPCRoute{},
		reconciler:            r,
		runnable:              r,
		k8sClient:             r.Client,
		gatewayClassName:      r.GatewayClassName,
		configResolver:        r.RouteSyncer.ConfigResolver,
		bindingValidator:      r.bindingValidator,
		findRoutesForGateway:  r.findRoutesForGateway,
		findRoutesForRefGrant: r.findRoutesForReferenceGrant,
		getAllRelevantRoutes:  r.getAllRelevantRoutes,
	})
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

	return FindRoutesForGateway(obj, r.GatewayClassName, routes)
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

	return FilterAcceptedRoutes(ctx, r.Client, r.bindingValidator, r.GatewayClassName, routes)
}
