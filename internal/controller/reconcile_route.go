package controller

import (
	"context"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// reconcileRouteParams holds common parameters for the generic route reconcile loop.
type reconcileRouteParams[T client.Object] struct {
	startupComplete  *atomic.Bool
	k8sClient        client.Client
	bindingValidator *routebinding.Validator
	gatewayClassName string
	componentName    string
	wrapRoute        func(T) Route
	syncAndUpdate    func(ctx context.Context) (ctrl.Result, error)
}

// reconcileRoute is the generic Reconcile implementation shared by
// HTTPRouteReconciler and GRPCRouteReconciler. It eliminates duplication
// between the two controllers that follow an identical reconcile pattern:
// wait for startup → get route → check ownership → sync.
func reconcileRoute[T client.Object](
	ctx context.Context,
	req ctrl.Request,
	route T,
	params reconcileRouteParams[T],
) (ctrl.Result, error) {
	if !params.startupComplete.Load() {
		return ctrl.Result{RequeueAfter: startupPendingRequeueDelay, Priority: new(priorityRoute)}, nil
	}

	ctx = logging.WithReconcileID(ctx)

	logger := logging.Component(ctx, params.componentName+"-reconciler").With(params.componentName, req.String())
	ctx = logging.WithLogger(ctx, logger)

	if err := params.k8sClient.Get(ctx, req.NamespacedName, route); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info(params.componentName + " deleted, triggering full sync")

			return params.syncAndUpdate(ctx)
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to get "+params.componentName)
	}

	wrapped := params.wrapRoute(route)
	if !IsRouteAcceptedByGateway(ctx, params.k8sClient, params.bindingValidator, params.gatewayClassName, wrapped) {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling " + params.componentName)

	return params.syncAndUpdate(ctx)
}

// routeControllerSetupParams holds parameters for setting up a route controller
// with standard watches (Gateway, GatewayClassConfig, Secret, ReferenceGrant).
type routeControllerSetupParams struct {
	routeObject           client.Object
	reconciler            reconcile.Reconciler
	runnable              manager.Runnable
	k8sClient             client.Client
	gatewayClassName      string
	configResolver        *config.Resolver
	bindingValidator      *routebinding.Validator
	findRoutesForGateway  handler.MapFunc
	findRoutesForRefGrant handler.MapFunc
	getAllRelevantRoutes  RequestsFunc
}

// setupRouteController sets up the controller-runtime builder with standard
// watches shared between HTTPRoute and GRPCRoute controllers.
func setupRouteController(mgr ctrl.Manager, params *routeControllerSetupParams) error {
	mapper := &ConfigMapper{
		Client:           params.k8sClient,
		GatewayClassName: params.gatewayClassName,
		ConfigResolver:   params.configResolver,
	}

	err := ctrl.NewControllerManagedBy(mgr).
		For(params.routeObject).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(params.findRoutesForGateway),
		).
		Watches(
			&v1alpha1.GatewayClassConfig{},
			handler.EnqueueRequestsFromMapFunc(mapper.MapConfigToRequests(params.getAllRelevantRoutes)),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(mapper.MapSecretToRequests(params.getAllRelevantRoutes)),
		).
		Watches(
			&gatewayv1beta1.ReferenceGrant{},
			handler.EnqueueRequestsFromMapFunc(params.findRoutesForRefGrant),
		).
		Complete(params.reconciler)
	if err != nil {
		return errors.Wrap(err, "failed to setup route controller")
	}

	if err := mgr.Add(params.runnable); err != nil {
		return errors.Wrap(err, "failed to add startup sync runnable")
	}

	return nil
}
