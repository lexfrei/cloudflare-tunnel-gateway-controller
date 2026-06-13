package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"maps"
	"slices"
	"strings"

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

// Generated config-API auth Secret parameters.
const (
	generatedAuthTokenKey   = "auth-token"
	generatedAuthTokenBytes = 32
)

// errRefusedAdoption is returned when a rendered resource's name collides with
// an existing object this Gateway does not own.
var errRefusedAdoption = errors.New("refusing to adopt an existing object not owned by this Gateway")

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
	// TriggerRouteSync runs a full route sync (cache + push to every
	// partition). It is invoked once when a data plane is first CREATED so the
	// new partition's config is cached and delivered — a per-Gateway proxy
	// needs an initial config push to pass /readyz, and route reconciles are
	// route-event-driven, so a data plane with no routes would otherwise never
	// be synced. Nil is a no-op (unit tests without the route syncer wired).
	TriggerRouteSync func(context.Context) error
}

func (r *GatewayInfraReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

	if !config.HasInfrastructureParametersRef(&gateway) {
		// Shared mode. Clean up anything a previous opt-in rendered — the
		// Gateway is alive, so ownerRef GC alone cannot.
		return ctrl.Result{}, r.cleanupRendered(ctx, &gateway)
	}

	// The generated auth Secret must exist BEFORE ResolveForGateway can read
	// it (the resolver returns a transient error otherwise), so this
	// controller — the Secret's owner — ensures it first. Bootstrapping it
	// here, not in the resolver, keeps the resolver read-only for its other
	// callers (route syncer, status writer).
	gwConfig, err := r.ConfigResolver.GetGatewayConfig(ctx, &gateway)
	if err != nil {
		return r.handleResolveError(ctx, &gateway, err)
	}

	if err := r.ensureGeneratedAuthSecret(ctx, &gateway, gwConfig); err != nil {
		r.event(&gateway, corev1.EventTypeWarning, eventReasonRenderFailed, err.Error())

		return ctrl.Result{}, err
	}

	perGateway, err := r.ConfigResolver.ResolveForGateway(ctx, &gateway)
	if err != nil {
		return r.handleResolveError(ctx, &gateway, err)
	}

	return ctrl.Result{}, r.applyRendered(ctx, &gateway, perGateway)
}

// handleResolveError maps a resolution failure onto the right outcome: a
// deterministic ErrInvalidParameters renders nothing and surfaces an Event
// (the GatewayConfig/Secret watches re-trigger on heal, and GatewayReconciler
// stamps InvalidParameters on the status); a transient error propagates for
// backoff.
//
// Deliberately, neither path deletes already-rendered resources: a Gateway
// whose config breaks AFTER a healthy render keeps its last-good data plane
// running (fail-closed-keep-last-state) rather than tearing down a serving
// proxy on a transient blip or a mid-edit invalid spec. Cleanup happens only
// on an explicit opt-out (parametersRef removed) or Gateway deletion.
func (r *GatewayInfraReconciler) handleResolveError(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	err error,
) (ctrl.Result, error) {
	if errors.Is(err, config.ErrInvalidParameters) {
		log.FromContext(ctx).Info("per-gateway data plane not rendered: invalid parametersRef", "error", err.Error())
		r.event(gateway, corev1.EventTypeWarning, eventReasonRenderFailed, err.Error())

		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, errors.Wrap(err, "failed to resolve per-gateway config")
}

// ensureGeneratedAuthSecret guarantees a config-API auth Secret exists for a
// Gateway whose GatewayConfig declares no explicit authTokenSecretRef. It is
// CREATE-ONLY: the token is generated once and never rotated on subsequent
// reconciles (a fresh token every reconcile would roll the proxy pods
// endlessly). The Secret is controller-owned so Gateway deletion GCs it.
func (r *GatewayInfraReconciler) ensureGeneratedAuthSecret(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	gwConfig *v1alpha1.GatewayConfig,
) error {
	if gwConfig.Spec.AuthTokenSecretRef != nil {
		return nil // tenant supplied its own token Secret
	}

	name := render.GeneratedAuthSecretName(gateway)
	key := types.NamespacedName{Name: name, Namespace: gateway.Namespace}

	var existing corev1.Secret
	if err := r.Get(ctx, key, &existing); err == nil {
		// Never adopt a Secret at our deterministic name that we do not own:
		// wiring the data plane's push auth to unverified material would break
		// the same never-adopt invariant the apply paths enforce.
		if err := assertAdoptable(&existing, gateway); err != nil {
			return err
		}

		// Owned: reuse as-is and never write to it. The controller holds only
		// secrets create (no update/delete) by deliberate least privilege, and
		// the create-once token is never rotated (a fresh token every reconcile
		// would roll the proxy pods endlessly). An owned Secret with an empty
		// token can only arise from external mutation; it fails closed
		// downstream (readAuthToken returns ErrInvalidParameters -> the data
		// plane is not rendered, a RenderFailed Event is surfaced) and heals
		// when the Secret is deleted — the create path below regenerates it.
		return nil
	} else if !apierrors.IsNotFound(err) {
		return errors.Wrapf(err, "checking generated auth secret %s", key)
	}

	token, err := generateAuthToken()
	if err != nil {
		return errors.Wrap(err, "generating config-api auth token")
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gateway.Namespace},
		Data:       map[string][]byte{generatedAuthTokenKey: []byte(token)},
	}

	if err := controllerutil.SetControllerReference(gateway, secret, r.Scheme); err != nil {
		return errors.Wrap(err, "setting owner on generated auth secret")
	}

	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrapf(err, "creating generated auth secret %s", key)
	}

	return nil
}

// generateAuthToken returns a 32-byte cryptographically-random bearer token,
// hex-encoded.
func generateAuthToken() (string, error) {
	buf := make([]byte, generatedAuthTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", errors.Wrap(err, "reading random bytes")
	}

	return hex.EncodeToString(buf), nil
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

	if _, err := r.applyAutoscaler(ctx, gateway, input); err != nil {
		r.event(gateway, corev1.EventTypeWarning, eventReasonRenderFailed, err.Error())

		return err
	}

	if deploymentOp == controllerutil.OperationResultCreated {
		r.event(gateway, corev1.EventTypeNormal, eventReasonProxyProvisioned,
			"dedicated proxy data plane provisioned for this Gateway")
	}

	// Trigger a full route sync whenever the data plane's SPEC changed (Created
	// OR Updated), not just on creation. No route controller watches
	// GatewayConfig, so a config edit that rotates the connector or push-auth
	// token re-renders the Deployment (new token hash / env secret ref) but
	// would otherwise leave the Cloudflare document and the proxy push on the
	// OLD tunnel/token until an unrelated route or Secret event. A
	// readiness-only reconcile returns None and does not re-sync, so this does
	// not fire on every Deployment status flip. Best-effort: a failure here is
	// retried by the next route reconcile; never fail the render over it.
	if deploymentOp != controllerutil.OperationResultNone && r.TriggerRouteSync != nil {
		if err := r.TriggerRouteSync(ctx); err != nil {
			log.FromContext(ctx).Info("route sync after data-plane render failed; will retry on next route reconcile",
				"error", err.Error())
		}
	}

	return nil
}

// applyDeployment renders and applies the proxy Deployment. Replica
// managedAnnotationsKey records which annotation keys THIS controller
// rendered onto an object, so a subsequent reconcile can drop ones the tenant
// removed without clobbering annotations set by other actors.
const managedAnnotationsKey = "cf.k8s.lex.la/managed-annotations"

// mergeManagedAnnotations overlays the desired (controller-rendered)
// annotations onto the existing ones WITHOUT replacing the whole map. A flat
// replace wipes annotations other controllers own — notably
// deployment.kubernetes.io/revision, which kube-controller-manager re-adds on
// every Deployment sync, producing an endless write ping-pong. Keys this
// controller previously rendered (tracked in managedAnnotationsKey) but no
// longer desires are removed; foreign keys are preserved.
func mergeManagedAnnotations(existing, desired map[string]string) map[string]string {
	result := make(map[string]string, len(existing)+len(desired))
	maps.Copy(result, existing)

	for key := range strings.SplitSeq(existing[managedAnnotationsKey], ",") {
		if key == "" {
			continue
		}

		if _, stillDesired := desired[key]; !stillDesired {
			delete(result, key)
		}
	}

	managed := make([]string, 0, len(desired))

	for key, value := range desired {
		result[key] = value
		managed = append(managed, key)
	}

	slices.Sort(managed)

	if len(managed) == 0 {
		delete(result, managedAnnotationsKey)
	} else {
		result[managedAnnotationsKey] = strings.Join(managed, ",")
	}

	return result
}

// assertAdoptable guards against silently adopting (and later GC-deleting) a
// user-created object that happens to share a rendered resource's name: a
// CreateOrUpdate mutate runs on an existing object with a populated
// ResourceVersion, and SetControllerReference would adopt it unless it
// already has a DIFFERENT controller. An existing object NOT owned by this
// Gateway is refused.
func assertAdoptable(existing client.Object, gateway *gatewayv1.Gateway) error {
	if existing.GetResourceVersion() == "" {
		return nil // being created
	}

	owner := metav1.GetControllerOf(existing)
	if owner != nil && owner.UID == gateway.UID {
		return nil // already ours
	}

	return errors.Wrapf(errRefusedAdoption, "%T %s/%s (rename it or the Gateway)",
		existing, existing.GetNamespace(), existing.GetName())
}

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
		if err := assertAdoptable(existing, gateway); err != nil {
			return err
		}

		hpaOwnedReplicas := existing.Spec.Replicas

		existing.Labels = desired.Labels
		existing.Annotations = mergeManagedAnnotations(existing.Annotations, desired.Annotations)
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
		if err := assertAdoptable(existing, gateway); err != nil {
			return err
		}

		existing.Labels = desired.Labels
		existing.Annotations = mergeManagedAnnotations(existing.Annotations, desired.Annotations)
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
) (controllerutil.OperationResult, error) {
	desired := render.Autoscaler(input)
	if desired == nil {
		return controllerutil.OperationResultNone, r.deleteIfOwned(ctx, gateway, &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name: render.DeploymentName(gateway), Namespace: gateway.Namespace,
			},
		})
	}

	existing := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: desired.Name, Namespace: desired.Namespace},
	}

	operation, err := controllerutil.CreateOrUpdate(ctx, r.Client, existing, func() error {
		if err := assertAdoptable(existing, gateway); err != nil {
			return err
		}

		existing.Labels = desired.Labels
		existing.Annotations = mergeManagedAnnotations(existing.Annotations, desired.Annotations)
		existing.Spec = desired.Spec

		return controllerutil.SetControllerReference(gateway, existing, r.Scheme)
	})
	if err != nil {
		return operation, errors.Wrap(err, "failed to apply per-gateway autoscaler")
	}

	return operation, nil
}

// cleanupRendered removes the per-Gateway resources after an opt-out. Missing
// objects are fine; objects NOT owned by this Gateway are left untouched so a
// name collision with user resources cannot turn into a deletion.
//
// The generated auth Secret is DELIBERATELY not deleted here: the controller's
// RBAC grants create-but-not-delete on Secrets (least privilege), and the
// Secret is ownerRef'd to the Gateway, so Gateway deletion GCs it. A stale
// auth Secret on an opted-out-but-alive Gateway is harmless — if the Gateway
// opts back in, ensureGeneratedAuthSecret reuses it.
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
// the Gateway. CONTRACT: opt-out cleanup is not guaranteed to converge to zero
// — if the ownerRef has been stripped (by a tenant or a foreign controller),
// the object is left as an orphan rather than deleted. This is deliberate:
// "never delete what we cannot prove we own" outranks "always clean up", so a
// name collision or a re-parented object can never turn opt-out into the
// deletion of a resource this controller no longer owns. An operator who wants
// the orphan gone deletes it by hand.
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
		log.FromContext(ctx).Error(err, "listing Gateways to map a watched object; re-render trigger dropped",
			"namespace", obj.GetNamespace())

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
