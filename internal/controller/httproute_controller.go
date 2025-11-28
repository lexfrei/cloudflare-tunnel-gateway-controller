package controller

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
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
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
)

const (
	kindGateway = "Gateway"

	// apiErrorRequeueDelay is the delay before retrying when Cloudflare API calls fail.
	apiErrorRequeueDelay = 15 * time.Second

	// startupPendingRequeueDelay is the delay before retrying when startup sync is not yet complete.
	startupPendingRequeueDelay = 1 * time.Second

	// maxIngressRules is the maximum number of ingress rules allowed per Cloudflare Tunnel.
	// Cloudflare's limit is approximately 1000 rules per tunnel.
	maxIngressRules = 1000
)

// HTTPRouteReconciler reconciles HTTPRoute resources and synchronizes them
// to Cloudflare Tunnel ingress configuration.
//
// Key behaviors:
//   - Watches all HTTPRoute resources in the cluster
//   - Filters routes by parent Gateway's GatewayClass
//   - Reads configuration from GatewayClassConfig via parametersRef
//   - Performs full synchronization on any route change (not incremental)
//   - Updates Cloudflare Tunnel config via API (cloudflared hot-reloads)
//   - Updates HTTPRoute status with acceptance conditions
//
// On startup, the reconciler performs a full sync to ensure tunnel configuration
// matches the current state of HTTPRoute resources. This means any ingress rules
// created outside of this controller will be replaced.
type HTTPRouteReconciler struct {
	client.Client

	// Scheme is the runtime scheme for API type registration.
	Scheme *runtime.Scheme

	// ClusterDomain is used for building service URLs (e.g., "cluster.local").
	ClusterDomain string

	// GatewayClassName filters which routes to process.
	GatewayClassName string

	// ControllerName is reported in HTTPRoute status.
	ControllerName string

	// ConfigResolver resolves configuration from GatewayClassConfig.
	ConfigResolver *config.Resolver

	// startupComplete indicates whether the startup sync has completed.
	// This prevents race conditions between startup sync and reconcile loop.
	startupComplete atomic.Bool
}

//nolint:noinlineerr // inline error handling is fine for controller pattern
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

			return r.syncAllRoutes(ctx)
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to get httproute")
	}

	if !r.isRouteForOurGateway(ctx, &route) {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling httproute")

	return r.syncAllRoutes(ctx)
}

//nolint:funcorder,noinlineerr // private helper method
func (r *HTTPRouteReconciler) isRouteForOurGateway(ctx context.Context, route *gatewayv1.HTTPRoute) bool {
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

//nolint:funcorder,noinlineerr,funlen // private helper method, complex sync logic
func (r *HTTPRouteReconciler) syncAllRoutes(ctx context.Context) (ctrl.Result, error) {
	logger := slog.Default().With("component", "sync")

	// Resolve configuration from GatewayClassConfig
	resolvedConfig, err := r.ConfigResolver.ResolveFromGatewayClassName(ctx, r.GatewayClassName)
	if err != nil {
		logger.Error("failed to resolve config from GatewayClassConfig", "error", err)

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay}, nil
	}

	// Create Cloudflare client with resolved credentials
	cfClient := r.ConfigResolver.CreateCloudflareClient(resolvedConfig)

	// Resolve account ID (auto-detect if not in config)
	accountID, err := r.ConfigResolver.ResolveAccountID(ctx, cfClient, resolvedConfig)
	if err != nil {
		logger.Error("failed to resolve account ID", "error", err)

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay}, nil
	}

	// Get current tunnel configuration
	currentConfig, err := cfClient.ZeroTrust.Tunnels.Cloudflared.Configurations.Get(
		ctx,
		resolvedConfig.TunnelID,
		zero_trust.TunnelCloudflaredConfigurationGetParams{
			AccountID: cloudflare.String(accountID),
		},
	)
	if err != nil {
		logger.Error("failed to get current tunnel configuration", "error", err)

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay}, nil
	}

	var routeList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routeList); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to list httproutes")
	}

	var relevantRoutes []gatewayv1.HTTPRoute

	for i := range routeList.Items {
		if r.isRouteForOurGateway(ctx, &routeList.Items[i]) {
			relevantRoutes = append(relevantRoutes, routeList.Items[i])
		}
	}

	logger.Info("syncing routes to cloudflare", "count", len(relevantRoutes))

	// Build desired rules from HTTPRoutes
	builder := ingress.NewBuilder(r.ClusterDomain)
	desiredRules := builder.Build(relevantRoutes)

	// Compute diff between current and desired
	toAdd, toRemove := ingress.DiffRules(currentConfig.Config.Ingress, desiredRules)

	logger.Info("computed diff", "toAdd", len(toAdd), "toRemove", len(toRemove))

	// Apply diff to get final rules
	finalRules := ingress.ApplyDiff(currentConfig.Config.Ingress, toAdd, toRemove)

	// Ensure catch-all rule exists at the end
	finalRules = ingress.EnsureCatchAll(finalRules)

	// Check ingress rules limit before API call
	if len(finalRules) > maxIngressRules {
		limitErr := errors.Newf("ingress rules limit exceeded: %d rules (max %d)", len(finalRules), maxIngressRules)
		logger.Error("ingress rules limit exceeded", "count", len(finalRules), "max", maxIngressRules)

		for i := range relevantRoutes {
			if updateErr := r.updateRouteStatus(ctx, &relevantRoutes[i], false, limitErr.Error()); updateErr != nil {
				logger.Error("failed to update route status", "error", updateErr)
			}
		}

		// Don't requeue - user must reduce the number of routes
		return ctrl.Result{}, nil
	}

	cfConfig := zero_trust.TunnelCloudflaredConfigurationUpdateParams{
		AccountID: cloudflare.String(accountID),
		Config: cloudflare.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
			Ingress: cloudflare.F(finalRules),
		}),
	}

	_, err = cfClient.ZeroTrust.Tunnels.Cloudflared.Configurations.Update(ctx, resolvedConfig.TunnelID, cfConfig)
	if err != nil {
		logger.Error("failed to update tunnel configuration", "error", err)

		for i := range relevantRoutes {
			if updateErr := r.updateRouteStatus(ctx, &relevantRoutes[i], false, err.Error()); updateErr != nil {
				logger.Error("failed to update route status", "error", updateErr)
			}
		}

		return ctrl.Result{RequeueAfter: apiErrorRequeueDelay}, nil
	}

	logger.Info("successfully updated tunnel configuration", "rules", len(finalRules))

	for i := range relevantRoutes {
		if err := r.updateRouteStatus(ctx, &relevantRoutes[i], true, ""); err != nil {
			logger.Error("failed to update route status", "error", err)
		}
	}

	return ctrl.Result{}, nil
}

//nolint:funcorder,funlen,noinlineerr // private helper method, status update logic
func (r *HTTPRouteReconciler) updateRouteStatus(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	accepted bool,
	message string,
) error {
	routeKey := types.NamespacedName{Name: route.Name, Namespace: route.Namespace}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the route to avoid conflict errors
		var freshRoute gatewayv1.HTTPRoute
		if err := r.Get(ctx, routeKey, &freshRoute); err != nil {
			return errors.Wrap(err, "failed to get fresh httproute")
		}

		now := metav1.Now()

		status := metav1.ConditionTrue
		reason := string(gatewayv1.RouteReasonAccepted)

		if !accepted {
			status = metav1.ConditionFalse
			reason = string(gatewayv1.RouteReasonNoMatchingParent)

			if message == "" {
				message = "Route not accepted"
			}
		} else {
			message = "Route accepted and programmed in Cloudflare Tunnel"
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
						Message:            "All references resolved",
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
	mapper := &ConfigMapper{
		Client:           r.Client,
		GatewayClassName: r.GatewayClassName,
		ConfigResolver:   r.ConfigResolver,
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

	logger := slog.Default().With("component", "startup-sync")
	logger.Info("performing startup sync of tunnel configuration")

	_, err := r.syncAllRoutes(ctx)
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
	gateway, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}

	if gateway.Spec.GatewayClassName != gatewayv1.ObjectName(r.GatewayClassName) {
		return nil
	}

	var routeList gatewayv1.HTTPRouteList
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

func (r *HTTPRouteReconciler) getAllRelevantRoutes(ctx context.Context) []reconcile.Request {
	var routeList gatewayv1.HTTPRouteList

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
