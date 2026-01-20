package controller

import (
	"context"

	"github.com/cockroachdb/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// GatewayClassReconciler reconciles GatewayClass resources.
// It updates the status with Accepted condition when the GatewayClass
// references this controller.
type GatewayClassReconciler struct {
	client.Client

	// Scheme is the runtime scheme for API type registration.
	Scheme *runtime.Scheme

	// ControllerName is the controller name to match in GatewayClass spec.
	ControllerName string
}

func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var gatewayClass gatewayv1.GatewayClass

	err := r.Get(ctx, req.NamespacedName, &gatewayClass)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to get GatewayClass")
	}

	if string(gatewayClass.Spec.ControllerName) != r.ControllerName {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling GatewayClass", "name", gatewayClass.Name)

	if err := r.updateStatus(ctx, req.NamespacedName); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to update GatewayClass status")
	}

	return ctrl.Result{}, nil
}

func (r *GatewayClassReconciler) updateStatus(ctx context.Context, key types.NamespacedName) error {
	//nolint:wrapcheck // retry wrapper handles errors internally
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var freshClass gatewayv1.GatewayClass
		if err := r.Get(ctx, key, &freshClass); err != nil {
			return errors.Wrap(err, "failed to get fresh GatewayClass")
		}

		if string(freshClass.Spec.ControllerName) != r.ControllerName {
			return nil
		}

		r.setAcceptedConditions(&freshClass)

		if err := r.Status().Update(ctx, &freshClass); err != nil {
			return errors.Wrap(err, "failed to update GatewayClass status")
		}

		return nil
	})
}

func (r *GatewayClassReconciler) setAcceptedConditions(gatewayClass *gatewayv1.GatewayClass) {
	now := metav1.Now()

	meta.SetStatusCondition(&gatewayClass.Status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayClassConditionStatusAccepted),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gatewayClass.Generation,
		LastTransitionTime: now,
		Reason:             string(gatewayv1.GatewayClassReasonAccepted),
		Message:            "GatewayClass is accepted by cloudflare-tunnel controller",
	})

	meta.SetStatusCondition(&gatewayClass.Status.Conditions, metav1.Condition{
		Type:               string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gatewayClass.Generation,
		LastTransitionTime: now,
		Reason:             string(gatewayv1.GatewayClassReasonSupportedVersion),
		Message:            "Gateway API CRD version is supported",
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	//nolint:wrapcheck // controller-runtime builder pattern
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.GatewayClass{}).
		Complete(r)
}
