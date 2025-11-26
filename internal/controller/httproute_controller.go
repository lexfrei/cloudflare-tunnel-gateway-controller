package controller

import (
	"context"
	"log/slog"

	"github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/zero_trust"
	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
)

const kindGateway = "Gateway"

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
}

//nolint:noinlineerr // inline error handling is fine for controller pattern
func (r *HTTPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

//nolint:funcorder,noinlineerr // private helper method
func (r *HTTPRouteReconciler) syncAllRoutes(ctx context.Context) (ctrl.Result, error) {
	logger := slog.Default().With("component", "sync")

	// Resolve configuration from GatewayClassConfig
	resolvedConfig, err := r.ConfigResolver.ResolveFromGatewayClassName(ctx, r.GatewayClassName)
	if err != nil {
		logger.Error("failed to resolve config from GatewayClassConfig", "error", err)
		return ctrl.Result{}, errors.Wrap(err, "failed to resolve GatewayClassConfig")
	}

	// Create Cloudflare client with resolved credentials
	cfClient := r.ConfigResolver.CreateCloudflareClient(resolvedConfig)

	// Resolve account ID (auto-detect if not in config)
	accountID, err := r.ConfigResolver.ResolveAccountID(ctx, cfClient, resolvedConfig)
	if err != nil {
		logger.Error("failed to resolve account ID", "error", err)
		return ctrl.Result{}, errors.Wrap(err, "failed to resolve account ID")
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

	builder := ingress.NewBuilder(r.ClusterDomain)
	rules := builder.Build(relevantRoutes)

	cfConfig := zero_trust.TunnelCloudflaredConfigurationUpdateParams{
		AccountID: cloudflare.String(accountID),
		Config: cloudflare.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
			Ingress: cloudflare.F(rules),
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

		return ctrl.Result{}, errors.Wrap(err, "failed to update cloudflare tunnel configuration")
	}

	logger.Info("successfully updated tunnel configuration", "rules", len(rules))

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

	route.Status.Parents = nil

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
					ObservedGeneration: route.Generation,
					LastTransitionTime: now,
					Reason:             reason,
					Message:            message,
				},
				{
					Type:               string(gatewayv1.RouteConditionResolvedRefs),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: route.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.RouteReasonResolvedRefs),
					Message:            "All references resolved",
				},
			},
		}

		route.Status.Parents = append(route.Status.Parents, parentStatus)
	}

	if err := r.Status().Update(ctx, route); err != nil {
		return errors.Wrap(err, "failed to update httproute status")
	}

	return nil
}

func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.findRoutesForGateway),
		).
		// Watch GatewayClassConfig for config changes
		Watches(
			&v1alpha1.GatewayClassConfig{},
			handler.EnqueueRequestsFromMapFunc(r.configToRoutes),
		).
		// Watch Secrets for credential changes
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToRoutes),
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

// configToRoutes maps GatewayClassConfig events to HTTPRoute reconcile requests.
func (r *HTTPRouteReconciler) configToRoutes(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	cfg, ok := obj.(*v1alpha1.GatewayClassConfig)
	if !ok {
		return nil
	}

	// Check if this config is referenced by our GatewayClass
	gatewayClass := &gatewayv1.GatewayClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: r.GatewayClassName}, gatewayClass); err != nil {
		return nil
	}

	if gatewayClass.Spec.ParametersRef == nil {
		return nil
	}

	if gatewayClass.Spec.ParametersRef.Name != cfg.Name {
		return nil
	}

	return r.getAllRelevantRoutes(ctx)
}

// secretToRoutes maps Secret events to HTTPRoute reconcile requests.
func (r *HTTPRouteReconciler) secretToRoutes(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	// Get the GatewayClassConfig for our class
	gatewayClass := &gatewayv1.GatewayClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: r.GatewayClassName}, gatewayClass); err != nil {
		return nil
	}

	cfg, err := r.ConfigResolver.GetConfigForGatewayClass(ctx, gatewayClass)
	if err != nil {
		return nil
	}

	// Check if this secret is referenced by the config
	if r.secretMatchesConfig(secret, cfg) {
		return r.getAllRelevantRoutes(ctx)
	}

	return nil
}

func (r *HTTPRouteReconciler) secretMatchesConfig(secret *corev1.Secret, cfg *v1alpha1.GatewayClassConfig) bool {
	// Check credentials secret
	credRef := cfg.Spec.CloudflareCredentialsSecretRef
	if secret.Name == credRef.Name && (credRef.Namespace == "" || credRef.Namespace == secret.Namespace) {
		return true
	}

	// Check tunnel token secret
	if cfg.Spec.TunnelTokenSecretRef != nil {
		tokenRef := cfg.Spec.TunnelTokenSecretRef
		if secret.Name == tokenRef.Name && (tokenRef.Namespace == "" || tokenRef.Namespace == secret.Namespace) {
			return true
		}
	}

	return false
}

func (r *HTTPRouteReconciler) getAllRelevantRoutes(ctx context.Context) []reconcile.Request {
	var routeList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routeList); err != nil {
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
