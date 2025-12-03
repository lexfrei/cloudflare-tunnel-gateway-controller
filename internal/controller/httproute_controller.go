package controller

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

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
	routeNotAcceptedMessage = "Route not accepted"
	routeAcceptedMessage    = "Route accepted and programmed in Cloudflare Tunnel"
	resolvedRefsMessage     = "All references resolved"
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

	// GatewayClassName filters which routes to process.
	GatewayClassName string

	// ControllerName is reported in HTTPRoute status.
	ControllerName string

	// RouteSyncer provides unified sync for both HTTP and GRPC routes.
	RouteSyncer *RouteSyncer

	// bindingValidator validates route binding to Gateway listeners.
	bindingValidator *routebinding.Validator

	// startupComplete indicates whether the startup sync has completed.
	// This prevents race conditions between startup sync and reconcile loop.
	startupComplete atomic.Bool
}

//nolint:noinlineerr // inline error handling for controller pattern
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Wait for startup sync to complete before processing reconcile events
	// to prevent race conditions with Cloudflare API updates
	if !r.startupComplete.Load() {
		return ctrl.Result{RequeueAfter: startupPendingRequeueDelay}, nil
	}

	logger := slog.Default().With("httproute", req.NamespacedName)

	var route gatewayv1.HTTPRoute
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("httproute deleted, triggering full sync")

			return r.syncAndUpdateStatus(ctx)
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to get httproute")
	}

	if !r.isRouteForOurGateway(ctx, &route) {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling httproute")

	return r.syncAndUpdateStatus(ctx)
}

//nolint:noinlineerr,funcorder // inline error handling for controller pattern; placed near Reconcile for readability
func (r *HTTPRouteReconciler) syncAndUpdateStatus(ctx context.Context) (ctrl.Result, error) {
	logger := slog.Default().With("component", "httproute-sync")

	result, syncResult, syncErr := r.RouteSyncer.SyncAllRoutes(ctx)

	// Update status for all HTTP routes with per-parent binding results
	if syncResult != nil {
		for i := range syncResult.HTTPRoutes {
			route := &syncResult.HTTPRoutes[i]
			routeKey := route.Namespace + "/" + route.Name
			bindingInfo := syncResult.HTTPRouteBindings[routeKey]

			// Filter failed refs for this route
			var routeFailedRefs []ingress.BackendRefError

			for _, failedRef := range syncResult.HTTPFailedRefs {
				if failedRef.RouteNamespace == route.Namespace && failedRef.RouteName == route.Name {
					routeFailedRefs = append(routeFailedRefs, failedRef)
				}
			}

			if err := r.updateRouteStatus(ctx, route, bindingInfo, routeFailedRefs, syncErr); err != nil {
				logger.Error("failed to update httproute status", "error", err)
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
func (r *HTTPRouteReconciler) isRouteForOurGateway(ctx context.Context, route *gatewayv1.HTTPRoute) bool {
	return IsRouteAcceptedByGateway(ctx, r.Client, r.bindingValidator, r.GatewayClassName, HTTPRouteWrapper{route})
}

//nolint:funcorder,funlen,noinlineerr,gocognit,dupl // private helper method, status update logic; dupl with GRPCRoute
func (r *HTTPRouteReconciler) updateRouteStatus(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	bindingInfo routeBindingInfo,
	failedRefs []ingress.BackendRefError,
	syncErr error,
) error {
	routeKey := types.NamespacedName{Name: route.Name, Namespace: route.Namespace}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the route to avoid conflict errors
		var freshRoute gatewayv1.HTTPRoute
		if err := r.Get(ctx, routeKey, &freshRoute); err != nil {
			return errors.Wrap(err, "failed to get fresh httproute")
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
			return errors.Wrap(err, "failed to update httproute status")
		}

		return nil
	})

	return errors.Wrap(err, "failed to update httproute status after retries")
}

func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.bindingValidator = routebinding.NewValidator(r.Client)

	mapper := &ConfigMapper{
		Client:           r.Client,
		GatewayClassName: r.GatewayClassName,
		ConfigResolver:   r.RouteSyncer.ConfigResolver,
	}

	err := ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		// Filter out status-only updates to prevent infinite reconciliation loops.
		// We only care about spec changes (generation changes) or deletions.
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.findRoutesForGateway),
		).
		// Watch GatewayClassConfig for config changes
		Watches(
			&v1alpha1.GatewayClassConfig{},
			handler.EnqueueRequestsFromMapFunc(mapper.MapConfigToRequests(r.getAllRelevantRoutes)),
		).
		// Watch Secrets for credential changes
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
		return errors.Wrap(err, "failed to setup httproute controller")
	}

	// Add startup runnable for initial sync
	addErr := mgr.Add(r)
	if addErr != nil {
		return errors.Wrap(addErr, "failed to add startup sync runnable")
	}

	return nil
}

// Start implements manager.Runnable for startup sync.
func (r *HTTPRouteReconciler) Start(ctx context.Context) error {
	// Mark startup as complete when this function returns,
	// regardless of success or failure
	defer r.startupComplete.Store(true)

	logger := slog.Default().With("component", "httproute-startup-sync")
	logger.Info("performing startup sync of tunnel configuration")

	_, err := r.syncAndUpdateStatus(ctx)
	if err != nil {
		logger.Error("startup sync failed", "error", err)
		// Don't return error - allow controller to start even if initial sync fails
	} else {
		logger.Info("startup sync completed successfully")
	}

	return nil
}

//nolint:noinlineerr // inline error handling for controller pattern
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

	return FindRoutesForGateway(obj, r.GatewayClassName, routes)
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

	return FilterAcceptedRoutes(ctx, r.Client, r.bindingValidator, r.GatewayClassName, routes)
}
