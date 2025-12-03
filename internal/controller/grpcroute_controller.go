package controller

import (
	"context"
	"strings"
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
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
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

//nolint:noinlineerr // inline error handling for controller pattern
func (r *GRPCRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Wait for startup sync to complete before processing reconcile events
	if !r.startupComplete.Load() {
		return ctrl.Result{RequeueAfter: startupPendingRequeueDelay}, nil
	}

	ctx = logging.WithReconcileID(ctx)
	logger := logging.Component(ctx, "grpcroute-reconciler").With("grpcroute", req.String())
	ctx = logging.WithLogger(ctx, logger)

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
	logger := logging.FromContext(ctx)

	result, syncResult, syncErr := r.RouteSyncer.SyncAllRoutes(ctx)

	// Update status for all GRPC routes with per-parent binding results
	if syncResult != nil {
		for i := range syncResult.GRPCRoutes {
			route := &syncResult.GRPCRoutes[i]
			routeKey := route.Namespace + "/" + route.Name
			bindingInfo := syncResult.GRPCRouteBindings[routeKey]

			// Filter failed refs for this route
			var routeFailedRefs []ingress.BackendRefError

			for _, failedRef := range syncResult.GRPCFailedRefs {
				if failedRef.RouteNamespace == route.Namespace && failedRef.RouteName == route.Name {
					routeFailedRefs = append(routeFailedRefs, failedRef)
				}
			}

			if err := r.updateRouteStatus(ctx, route, bindingInfo, routeFailedRefs, syncErr); err != nil {
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

//nolint:funcorder // private helper method
func (r *GRPCRouteReconciler) isRouteForOurGateway(ctx context.Context, route *gatewayv1.GRPCRoute) bool {
	return IsRouteAcceptedByGateway(ctx, r.Client, r.bindingValidator, r.GatewayClassName, GRPCRouteWrapper{route})
}

//nolint:funcorder,funlen,noinlineerr,gocognit,dupl // private helper method, status update logic; dupl with HTTPRoute
func (r *GRPCRouteReconciler) updateRouteStatus(
	ctx context.Context,
	route *gatewayv1.GRPCRoute,
	bindingInfo routeBindingInfo,
	failedRefs []ingress.BackendRefError,
	syncErr error,
) error {
	routeKey := types.NamespacedName{Name: route.Name, Namespace: route.Namespace}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var freshRoute gatewayv1.GRPCRoute
		if err := r.Get(ctx, routeKey, &freshRoute); err != nil {
			return errors.Wrap(err, "failed to get fresh grpcroute")
		}

		now := metav1.Now()
		freshRoute.Status.Parents = nil

		for refIdx, ref := range freshRoute.Spec.ParentRefs {
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

			// Get binding result for this parent ref
			bindingResult, hasBinding := bindingInfo.bindingResults[refIdx]

			status := metav1.ConditionTrue
			reason := string(gatewayv1.RouteReasonAccepted)
			message := routeAcceptedMessage

			if syncErr != nil {
				status = metav1.ConditionFalse
				reason = string(gatewayv1.RouteReasonPending)
				message = syncErr.Error()
			} else if hasBinding && !bindingResult.Accepted {
				status = metav1.ConditionFalse
				reason = string(bindingResult.Reason)
				message = bindingResult.Message
			}

			// Check if there are any failed backend refs
			resolvedRefsStatus := metav1.ConditionTrue
			resolvedRefsReason := string(gatewayv1.RouteReasonResolvedRefs)
			resolvedRefsMessage := resolvedRefsMessage

			if len(failedRefs) > 0 {
				resolvedRefsStatus = metav1.ConditionFalse
				resolvedRefsReason = string(gatewayv1.RouteReasonRefNotPermitted)

				// Build message with list of denied backends
				var msgBuilder strings.Builder

				msgBuilder.WriteString("Backend references not permitted: ")

				for i, failedRef := range failedRefs {
					if i > 0 {
						msgBuilder.WriteString(", ")
					}

					msgBuilder.WriteString(failedRef.BackendNS + "/" + failedRef.BackendName)
				}

				resolvedRefsMessage = msgBuilder.String()
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
						Status:             resolvedRefsStatus,
						ObservedGeneration: freshRoute.Generation,
						LastTransitionTime: now,
						Reason:             resolvedRefsReason,
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
	r.bindingValidator = routebinding.NewValidator(r.Client)

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
		// Watch ReferenceGrant for cross-namespace permission changes
		Watches(
			&gatewayv1beta1.ReferenceGrant{},
			handler.EnqueueRequestsFromMapFunc(r.findRoutesForReferenceGrant),
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

//nolint:noinlineerr // inline error handling for controller pattern
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
