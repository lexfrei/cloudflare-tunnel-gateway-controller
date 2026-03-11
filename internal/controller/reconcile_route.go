package controller

import (
	"context"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
