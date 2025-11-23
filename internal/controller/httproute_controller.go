package controller

import (
	"context"
	"log/slog"

	"github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/zero_trust"
	"github.com/cockroachdb/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
)

type HTTPRouteReconciler struct {
	client.Client

	Scheme           *runtime.Scheme
	CFClient         *cloudflare.Client
	AccountID        string
	TunnelID         string
	ClusterDomain    string
	GatewayClassName string
	ControllerName   string
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
		if ref.Kind != nil && *ref.Kind != "Gateway" {
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

	config := zero_trust.TunnelCloudflaredConfigurationUpdateParams{
		AccountID: cloudflare.String(r.AccountID),
		Config: cloudflare.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
			Ingress: cloudflare.F(rules),
		}),
	}

	_, err := r.CFClient.ZeroTrust.Tunnels.Cloudflared.Configurations.Update(ctx, r.TunnelID, config)
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

	parentStatus := gatewayv1.RouteParentStatus{
		ParentRef: gatewayv1.ParentReference{
			Name: gatewayv1.ObjectName(r.GatewayClassName),
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

	found := false

	for i, ps := range route.Status.Parents {
		if ps.ControllerName == gatewayv1.GatewayController(r.ControllerName) {
			route.Status.Parents[i] = parentStatus
			found = true

			break
		}
	}

	if !found {
		route.Status.Parents = append(route.Status.Parents, parentStatus)
	}

	if err := r.Status().Update(ctx, route); err != nil {
		return errors.Wrap(err, "failed to update httproute status")
	}

	return nil
}

func (r *HTTPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	//nolint:wrapcheck // controller-runtime builder pattern
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.HTTPRoute{}).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.findRoutesForGateway),
		).
		Complete(r)
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
