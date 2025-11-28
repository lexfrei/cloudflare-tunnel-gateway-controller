package controller

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
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

	// startupComplete indicates whether the startup sync has completed.
	startupComplete atomic.Bool
}

//nolint:noinlineerr // inline error handling is fine for controller pattern
func (r *GRPCRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Wait for startup sync to complete before processing reconcile events
	if !r.startupComplete.Load() {
		return ctrl.Result{RequeueAfter: startupPendingRequeueDelay}, nil
	}

	logger := slog.Default().With("grpcroute", req.NamespacedName)

	var route gatewayv1.GRPCRoute
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("grpcroute deleted, triggering full sync")

			return r.syncAndUpdateStatus(ctx)
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to get grpcroute")
	}

	if !r.isRouteForOurGateway(ctx, &route) {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling grpcroute")

	return r.syncAndUpdateStatus(ctx)
}

//nolint:noinlineerr,funcorder // inline error handling for controller pattern; placed near Reconcile for readability
func (r *GRPCRouteReconciler) syncAndUpdateStatus(ctx context.Context) (ctrl.Result, error) {
	logger := slog.Default().With("component", "grpcroute-sync")

	result, syncResult, syncErr := r.RouteSyncer.SyncAllRoutes(ctx)

	// Update status for all GRPC routes
	if syncResult != nil {
		accepted := syncErr == nil
		message := ""

		if syncErr != nil {
			message = syncErr.Error()
		}

		for i := range syncResult.GRPCRoutes {
			if err := r.updateRouteStatus(ctx, &syncResult.GRPCRoutes[i], accepted, message); err != nil {
				logger.Error("failed to update grpcroute status", "error", err)
			}
		}
	}

	if syncErr != nil && result.RequeueAfter == 0 {
		// Don't propagate error for limit exceeded (no requeue needed)
		return result, nil
	}

	return result, nil
}

//nolint:funcorder,noinlineerr // private helper method
func (r *GRPCRouteReconciler) isRouteForOurGateway(ctx context.Context, route *gatewayv1.GRPCRoute) bool {
	for _, ref := range route.Spec.ParentRefs {
		if ref.Kind != nil && *ref.Kind != kindGateway {
			continue
		}

		namespace := route.Namespace
		if ref.Namespace != nil {
			namespace = string(*ref.Namespace)
		}

		var gateway gatewayv1.Gateway
		if err := r.Get(ctx, client.ObjectKey{Name: string(ref.Name), Namespace: namespace}, &gateway); err != nil {
			continue
		}

		if gateway.Spec.GatewayClassName == gatewayv1.ObjectName(r.GatewayClassName) {
			return true
		}
	}

	return false
}

//nolint:funcorder,funlen,noinlineerr // private helper method, status update logic
func (r *GRPCRouteReconciler) updateRouteStatus(
	ctx context.Context,
	route *gatewayv1.GRPCRoute,
	accepted bool,
	message string,
) error {
	routeKey := types.NamespacedName{Name: route.Name, Namespace: route.Namespace}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var freshRoute gatewayv1.GRPCRoute
		if err := r.Get(ctx, routeKey, &freshRoute); err != nil {
			return errors.Wrap(err, "failed to get fresh grpcroute")
		}

		now := metav1.Now()

		status := metav1.ConditionTrue
		reason := string(gatewayv1.RouteReasonAccepted)

		if !accepted {
			status = metav1.ConditionFalse
			reason = string(gatewayv1.RouteReasonNoMatchingParent)

			if message == "" {
				message = routeNotAcceptedMessage
			}
		} else {
			message = routeAcceptedMessage
		}

		freshRoute.Status.Parents = nil

		for _, ref := range freshRoute.Spec.ParentRefs {
			if ref.Kind != nil && *ref.Kind != kindGateway {
				continue
			}

			namespace := freshRoute.Namespace
			if ref.Namespace != nil {
				namespace = string(*ref.Namespace)
			}

			var gateway gatewayv1.Gateway
			if err := r.Get(ctx, client.ObjectKey{Name: string(ref.Name), Namespace: namespace}, &gateway); err != nil {
				continue
			}

			if gateway.Spec.GatewayClassName != gatewayv1.ObjectName(r.GatewayClassName) {
				continue
			}

			parentStatus := gatewayv1.RouteParentStatus{
				ParentRef: gatewayv1.ParentReference{
					Group:       ref.Group,
					Kind:        ref.Kind,
					Namespace:   (*gatewayv1.Namespace)(&namespace),
					Name:        ref.Name,
					SectionName: ref.SectionName,
				},
				ControllerName: gatewayv1.GatewayController(r.ControllerName),
				Conditions: []metav1.Condition{
					{
						Type:               string(gatewayv1.RouteConditionAccepted),
						Status:             status,
						ObservedGeneration: freshRoute.Generation,
						LastTransitionTime: now,
						Reason:             reason,
						Message:            message,
					},
					{
						Type:               string(gatewayv1.RouteConditionResolvedRefs),
						Status:             metav1.ConditionTrue,
						ObservedGeneration: freshRoute.Generation,
						LastTransitionTime: now,
						Reason:             string(gatewayv1.RouteReasonResolvedRefs),
						Message:            resolvedRefsMessage,
					},
				},
			}

			freshRoute.Status.Parents = append(freshRoute.Status.Parents, parentStatus)
		}

		if err := r.Status().Update(ctx, &freshRoute); err != nil {
			return errors.Wrap(err, "failed to update grpcroute status")
		}

		return nil
	})

	return errors.Wrap(err, "failed to update grpcroute status after retries")
}

func (r *GRPCRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapper := &ConfigMapper{
		Client:           r.Client,
		GatewayClassName: r.GatewayClassName,
		ConfigResolver:   r.RouteSyncer.ConfigResolver,
	}

	err := ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.GRPCRoute{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.findRoutesForGateway),
		).
		Watches(
			&v1alpha1.GatewayClassConfig{},
			handler.EnqueueRequestsFromMapFunc(mapper.MapConfigToRequests(r.getAllRelevantRoutes)),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(mapper.MapSecretToRequests(r.getAllRelevantRoutes)),
		).
		Complete(r)
	if err != nil {
		return errors.Wrap(err, "failed to setup grpcroute controller")
	}

	addErr := mgr.Add(r)
	if addErr != nil {
		return errors.Wrap(addErr, "failed to add grpcroute startup sync runnable")
	}

	return nil
}

// Start implements manager.Runnable for startup sync.
func (r *GRPCRouteReconciler) Start(ctx context.Context) error {
	defer r.startupComplete.Store(true)

	logger := slog.Default().With("component", "grpcroute-startup-sync")
	logger.Info("performing startup sync of grpcroute configuration")

	_, err := r.syncAndUpdateStatus(ctx)
	if err != nil {
		logger.Error("grpcroute startup sync failed", "error", err)
	} else {
		logger.Info("grpcroute startup sync completed successfully")
	}

	return nil
}

//nolint:noinlineerr,dupl // inline error handling for controller pattern; similar logic for different route types is intentional
func (r *GRPCRouteReconciler) findRoutesForGateway(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	gateway, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}

	if gateway.Spec.GatewayClassName != gatewayv1.ObjectName(r.GatewayClassName) {
		return nil
	}

	var routeList gatewayv1.GRPCRouteList
	if err := r.List(ctx, &routeList); err != nil {
		return nil
	}

	var requests []reconcile.Request

	for i := range routeList.Items {
		for _, ref := range routeList.Items[i].Spec.ParentRefs {
			if string(ref.Name) == gateway.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKey{
						Name:      routeList.Items[i].Name,
						Namespace: routeList.Items[i].Namespace,
					},
				})

				break
			}
		}
	}

	return requests
}

func (r *GRPCRouteReconciler) getAllRelevantRoutes(ctx context.Context) []reconcile.Request {
	var routeList gatewayv1.GRPCRouteList

	err := r.List(ctx, &routeList)
	if err != nil {
		return nil
	}

	var requests []reconcile.Request

	for i := range routeList.Items {
		if r.isRouteForOurGateway(ctx, &routeList.Items[i]) {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      routeList.Items[i].Name,
					Namespace: routeList.Items[i].Namespace,
				},
			})
		}
	}

	return requests
}
