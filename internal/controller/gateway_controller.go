package controller

import (
	"context"

	"github.com/cockroachdb/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type GatewayReconciler struct {
	client.Client

	Scheme           *runtime.Scheme
	GatewayClassName string
	ControllerName   string
}

//nolint:noinlineerr // inline error handling is fine for controller pattern
func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var gateway gatewayv1.Gateway
	if err := r.Get(ctx, req.NamespacedName, &gateway); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to get gateway")
	}

	if gateway.Spec.GatewayClassName != gatewayv1.ObjectName(r.GatewayClassName) {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling gateway", "name", gateway.Name, "namespace", gateway.Namespace)

	if err := r.updateStatus(ctx, &gateway); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to update gateway status")
	}

	return ctrl.Result{}, nil
}

//nolint:funcorder,noinlineerr // private helper method
func (r *GatewayReconciler) updateStatus(ctx context.Context, gateway *gatewayv1.Gateway) error {
	now := metav1.Now()

	gateway.Status.Conditions = []metav1.Condition{
		{
			Type:               string(gatewayv1.GatewayConditionAccepted),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gateway.Generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.GatewayReasonAccepted),
			Message:            "Gateway accepted by cloudflare-tunnel controller",
		},
		{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: gateway.Generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.GatewayReasonProgrammed),
			Message:            "Gateway programmed in Cloudflare Tunnel",
		},
	}

	listenerStatuses := make([]gatewayv1.ListenerStatus, 0, len(gateway.Spec.Listeners))
	for _, listener := range gateway.Spec.Listeners {
		listenerStatuses = append(listenerStatuses, gatewayv1.ListenerStatus{
			Name: listener.Name,
			SupportedKinds: []gatewayv1.RouteGroupKind{
				{
					Group: (*gatewayv1.Group)(&gatewayv1.GroupVersion.Group),
					Kind:  "HTTPRoute",
				},
			},
			AttachedRoutes: 0,
			Conditions: []metav1.Condition{
				{
					Type:               string(gatewayv1.ListenerConditionAccepted),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonAccepted),
					Message:            "Listener accepted",
				},
				{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: gateway.Generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonProgrammed),
					Message:            "Listener programmed",
				},
			},
		})
	}

	gateway.Status.Listeners = listenerStatuses

	if err := r.Status().Update(ctx, gateway); err != nil {
		return errors.Wrap(err, "failed to update gateway status")
	}

	return nil
}

func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	//nolint:wrapcheck // controller-runtime builder pattern
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.Gateway{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}
