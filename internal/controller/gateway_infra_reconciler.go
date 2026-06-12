package controller

import (
	"context"

	"github.com/cockroachdb/errors"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// Event reasons emitted by the infra reconciler.
const (
	eventReasonProxyProvisioned = "ProxyProvisioned"
	eventReasonRenderFailed     = "RenderFailed"
	eventActionRender           = "Render"
)

// GatewayInfraReconciler owns the per-Gateway data plane: for every managed
// Gateway carrying infrastructure.parametersRef it renders and keeps in sync
// a dedicated proxy Deployment and headless config Service (and, when
// autoscaling is configured, an HPA). Resources are controller-owned via
// ownerReferences, so Gateway deletion garbage-collects them; opting back out
// (removing the parametersRef) is cleaned up explicitly because the owner
// stays alive.
//
// This reconciler writes NO Gateway status — GatewayReconciler stays the
// single status writer (two writers would race on conditions). Its failure
// surface is Kubernetes Events plus the absence of rendered resources, which
// GatewayReconciler folds into Programmed=False.
type GatewayInfraReconciler struct {
	client.Client

	Scheme         *runtime.Scheme
	ControllerName string
	ConfigResolver *config.Resolver
	// Recorder emits provisioning/failure Events. Nil is a no-op (unit tests).
	Recorder events.EventRecorder
	// RenderDefaults carries the Helm-wired defaults (proxy image, tunnel
	// protocol) for rendered data planes.
	RenderDefaults render.Defaults
}

func (r *GatewayInfraReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var gateway gatewayv1.Gateway
	if err := r.Get(ctx, req.NamespacedName, &gateway); err != nil {
		if apierrors.IsNotFound(err) {
			// Gateway gone: ownerRef GC collects the rendered resources.
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to get gateway")
	}

	if !isGatewayManagedByController(ctx, r.Client, &gateway, r.ControllerName) {
		return ctrl.Result{}, nil
	}

	if !gateway.DeletionTimestamp.IsZero() {
		// ownerRef GC handles the rendered resources.
		return ctrl.Result{}, nil
	}

	perGateway, err := r.ConfigResolver.ResolveForGateway(ctx, &gateway)
	if err != nil {
		if errors.Is(err, config.ErrInvalidParameters) {
			// User-fixable: render nothing, surface via Event; the
			// GatewayConfig/Secret watches re-trigger when the referent heals,
			// and GatewayReconciler surfaces InvalidParameters on the status.
			logger.Info("per-gateway data plane not rendered: invalid parametersRef", "error", err.Error())
			r.event(&gateway, corev1.EventTypeWarning, eventReasonRenderFailed, err.Error())

			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to resolve per-gateway config")
	}

	if perGateway == nil {
		// Shared mode. Clean up anything a previous opt-in rendered — the
		// Gateway is alive, so ownerRef GC alone cannot.
		return ctrl.Result{}, r.cleanupRendered(ctx, &gateway)
	}

	return ctrl.Result{}, r.applyRendered(ctx, &gateway, perGateway)
}

// event emits via the recorder when one is wired; no-op otherwise.
func (r *GatewayInfraReconciler) event(gateway *gatewayv1.Gateway, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}

	r.Recorder.Eventf(gateway, nil, eventType, reason, eventActionRender, "%s", message)
}

// applyRendered creates or updates the per-Gateway resources to match the
// rendered desired state.
func (r *GatewayInfraReconciler) applyRendered(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	perGateway *config.PerGatewayConfig,
) error {
	input := render.Input{
		Gateway:     gateway,
		Config:      perGateway.GatewayConfig,
		TunnelToken: perGateway.TunnelToken,
		Defaults:    r.RenderDefaults,
	}

	// Misconfiguration guard: with no controller-level --proxy-image and no
	// per-Gateway image override, the rendered Deployment would carry an
	// empty image the apiserver rejects on every reconcile. Render nothing
	// and surface the problem instead (manual installs without the flag).
	if perGateway.GatewayConfig.Spec.Image == "" && r.RenderDefaults.ProxyImage == "" {
		message := "per-gateway data plane not rendered: no proxy image configured — " +
			"set the controller's --proxy-image flag or GatewayConfig.spec.image"
		log.FromContext(ctx).Info(message)
		r.event(gateway, corev1.EventTypeWarning, eventReasonRenderFailed, message)

		return nil
	}

	deploymentOp, err := r.applyDeployment(ctx, gateway, input)
	if err != nil {
		r.event(gateway, corev1.EventTypeWarning, eventReasonRenderFailed, err.Error())

		return err
	}

	if _, err := r.applyService(ctx, gateway, input); err != nil {
		r.event(gateway, corev1.EventTypeWarning, eventReasonRenderFailed, err.Error())

		return err
	}

	if err := r.applyAutoscaler(ctx, gateway, input); err != nil {
		r.event(gateway, corev1.EventTypeWarning, eventReasonRenderFailed, err.Error())

		return err
	}

	if deploymentOp == controllerutil.OperationResultCreated {
		r.event(gateway, corev1.EventTypeNormal, eventReasonProxyProvisioned,
			"dedicated proxy data plane provisioned for this Gateway")
	}

	return nil
}

// applyDeployment renders and applies the proxy Deployment. Replica
// ownership: when the render leaves replicas nil (autoscaling mode), the
// existing count — set by the HPA — is preserved instead of being reset.
func (r *GatewayInfraReconciler) applyDeployment(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	input render.Input,
) (controllerutil.OperationResult, error) {
	desired := render.ProxyDeployment(input)

	existing := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace},
	}

	operation, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		hpaOwnedReplicas := existing.Spec.Replicas

		existing.Labels = desired.Labels
		existing.Annotations = desired.Annotations
		existing.Spec = desired.Spec

		if desired.Spec.Replicas == nil {
			// Autoscaling mode: keep whatever the HPA decided. A nil value on
			// update would be re-defaulted to 1 by the apiserver and fight the
			// autoscaler on every reconcile.
			existing.Spec.Replicas = hpaOwnedReplicas
		}

		return controllerutil.SetControllerReference(gateway, existing, r.Scheme)
	})
	if err != nil {
		return operation, errors.Wrap(err, "failed to apply per-gateway proxy deployment")
	}

	return operation, nil
}

// applyService renders and applies the headless config Service, preserving
// the immutable clusterIP fields on update.
func (r *GatewayInfraReconciler) applyService(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	input render.Input,
) (controllerutil.OperationResult, error) {
	desired := render.ConfigService(input)

	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace},
	}

	operation, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels
		existing.Annotations = desired.Annotations
		existing.Spec.Type = desired.Spec.Type
		existing.Spec.Selector = desired.Spec.Selector
		existing.Spec.Ports = desired.Spec.Ports
		existing.Spec.PublishNotReadyAddresses = desired.Spec.PublishNotReadyAddresses

		if existing.Spec.ClusterIP == "" {
			// Only settable on create; immutable afterwards.
			existing.Spec.ClusterIP = desired.Spec.ClusterIP
		}

		return controllerutil.SetControllerReference(gateway, existing, r.Scheme)
	})
	if err != nil {
		return operation, errors.Wrap(err, "failed to apply per-gateway config service")
	}

	return operation, nil
}

// applyAutoscaler renders and applies the HPA when autoscaling is set, and
// deletes a previously-rendered (owned) HPA when it is not.
func (r *GatewayInfraReconciler) applyAutoscaler(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	input render.Input,
) error {
	desired := render.Autoscaler(input)
	if desired == nil {
		return r.deleteIfOwned(ctx, gateway, &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name: render.DeploymentName(gateway), Namespace: gateway.Namespace,
			},
		})
	}

	existing := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		existing.Labels = desired.Labels
		existing.Annotations = desired.Annotations
		existing.Spec = desired.Spec

		return controllerutil.SetControllerReference(gateway, existing, r.Scheme)
	})
	if err != nil {
		return errors.Wrap(err, "failed to apply per-gateway autoscaler")
	}

	return nil
}

// cleanupRendered removes the per-Gateway resources after an opt-out. Missing
// objects are fine; objects NOT owned by this Gateway are left untouched so a
// name collision with user resources cannot turn into a deletion.
func (r *GatewayInfraReconciler) cleanupRendered(ctx context.Context, gateway *gatewayv1.Gateway) error {
	objects := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: render.DeploymentName(gateway), Namespace: gateway.Namespace,
		}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: render.ConfigServiceName(gateway), Namespace: gateway.Namespace,
		}},
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{
			Name: render.DeploymentName(gateway), Namespace: gateway.Namespace,
		}},
	}

	for _, obj := range objects {
		if err := r.deleteIfOwned(ctx, gateway, obj); err != nil {
			return err
		}
	}

	return nil
}

// deleteIfOwned deletes obj only when it exists AND is controller-owned by
// the Gateway.
func (r *GatewayInfraReconciler) deleteIfOwned(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	obj client.Object,
) error {
	key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}

	if err := r.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return errors.Wrapf(err, "failed to get %T %s for cleanup", obj, key)
	}

	owner := metav1.GetControllerOf(obj)
	if owner == nil || owner.UID != gateway.UID {
		return nil
	}

	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "failed to delete %T %s on data-plane opt-out", obj, key)
	}

	return nil
}

// SetupWithManager registers the reconciler: the Gateway itself, ownership of
// the rendered resources (drift heal), and the per-Gateway inputs
// (GatewayConfig, token/auth Secrets) whose changes must re-render.
func (r *GatewayInfraReconciler) SetupWithManager(mgr ctrl.Manager) error {
	//nolint:wrapcheck // controller-runtime builder pattern
	return ctrl.NewControllerManagedBy(mgr).
		// controller-runtime derives controller names from the For type;
		// GatewayReconciler already owns the implicit "gateway" name, so this
		// second Gateway-typed controller MUST carry an explicit name or
		// manager startup fails with a duplicate-name error.
		Named("gateway-infra").
		For(&gatewayv1.Gateway{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Watches(
			&v1alpha1.GatewayConfig{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceInfraGateways),
		).
		// The Secret watch is deliberately unfiltered: tenant token/auth
		// Secrets carry no identifying label this controller could predicate
		// on, and rotation must roll the rendered pods. The cost is bounded
		// by the mapper, not a predicate — every Secret write runs one
		// cache-served namespace List and enqueues nothing unless the
		// namespace holds an opted-in Gateway.
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceInfraGateways),
		).
		Complete(r)
}

// namespaceInfraGateways enqueues every opted-in Gateway in the event
// object's namespace. GatewayConfig and the per-Gateway Secrets are
// namespace-local to their Gateway by construction, so the namespace bound
// keeps the fan-out tight without per-object reference bookkeeping.
func (r *GatewayInfraReconciler) namespaceInfraGateways(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0)

	for i := range gateways.Items {
		gateway := &gateways.Items[i]
		if !config.HasInfrastructureParametersRef(gateway) {
			continue
		}

		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: gateway.Name, Namespace: gateway.Namespace,
		}})
	}

	return requests
}
