package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/pkg/consts"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// gatewayAPICRDNames are the Gateway API CRDs the controller serves, each
// probed for the bundle-version annotation: a partial upgrade can leave them
// from different bundles.
func gatewayAPICRDNames() []string {
	return []string{
		"gatewayclasses.gateway.networking.k8s.io",
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
		"grpcroutes.gateway.networking.k8s.io",
		"referencegrants.gateway.networking.k8s.io",
		"listenersets.gateway.networking.k8s.io",
		"backendtlspolicies.gateway.networking.k8s.io",
	}
}

// GatewayClassReconciler reconciles GatewayClass resources.
// It updates the status with Accepted condition when the GatewayClass
// references this controller.
type GatewayClassReconciler struct {
	client.Client

	// Scheme is the runtime scheme for API type registration.
	Scheme *runtime.Scheme

	// ControllerName is the controller name to match in GatewayClass spec.
	ControllerName string

	// BundleVersionReader reads the installed Gateway API CRDs to verify the
	// bundle version for the SupportedVersion condition. It is an uncached
	// reader (the manager's APIReader) so the controller does not watch every
	// CRD cluster-wide. When nil, the check fails closed: SupportedVersion is
	// set to False with reason UnsupportedVersion ("no reader configured"),
	// not left unset.
	BundleVersionReader client.Reader
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

	if err := r.reconcileFinalizer(ctx, &gatewayClass); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to reconcile GatewayClass finalizer")
	}

	// A class in deletion gets no status writes: removing our finalizer above
	// may have let the object vanish, and stamping conditions onto a
	// terminating object is pointless anyway.
	if !gatewayClass.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if err := r.updateStatus(ctx, req.NamespacedName, gatewayClass.Generation); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to update GatewayClass status")
	}

	return ctrl.Result{}, nil
}

// reconcileFinalizer keeps the spec-defined gateway-exists finalizer in sync
// with actual usage: present while at least one Gateway references the class
// (so a delete cannot pull the class out from under running Gateways), absent
// once nothing uses it. Foreign finalizers are never touched. The finalizer
// is never ADDED to a class already in deletion -- the API server rejects new
// finalizers on deleting objects -- but removal during deletion is exactly
// what unblocks the delete and must always run.
func (r *GatewayClassReconciler) reconcileFinalizer(ctx context.Context, gatewayClass *gatewayv1.GatewayClass) error {
	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways); err != nil {
		return errors.Wrap(err, "failed to list Gateways for finalizer accounting")
	}

	inUse := false

	for i := range gateways.Items {
		if string(gateways.Items[i].Spec.GatewayClassName) == gatewayClass.Name {
			inUse = true

			break
		}
	}

	hasFinalizer := controllerutil.ContainsFinalizer(gatewayClass, gatewayv1.GatewayClassFinalizerGatewaysExist)
	deleting := !gatewayClass.DeletionTimestamp.IsZero()

	switch {
	case inUse && !hasFinalizer && !deleting:
		controllerutil.AddFinalizer(gatewayClass, gatewayv1.GatewayClassFinalizerGatewaysExist)
	case !inUse && hasFinalizer:
		controllerutil.RemoveFinalizer(gatewayClass, gatewayv1.GatewayClassFinalizerGatewaysExist)
	default:
		return nil
	}

	if err := r.Update(ctx, gatewayClass); err != nil {
		return errors.Wrap(err, "failed to update GatewayClass finalizers")
	}

	return nil
}

func (r *GatewayClassReconciler) updateStatus(
	ctx context.Context,
	key types.NamespacedName,
	reconciledGen int64,
) error {
	//nolint:wrapcheck // retry wrapper handles errors internally
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var freshClass gatewayv1.GatewayClass
		if err := r.Get(ctx, key, &freshClass); err != nil {
			return errors.Wrap(err, "failed to get fresh GatewayClass")
		}

		if string(freshClass.Spec.ControllerName) != r.ControllerName {
			return nil
		}

		// Skip if a newer reconcile already advanced this GatewayClass's status
		// past the generation we observed (observedGeneration regression guard).
		// Only our own condition types count — a foreign condition's generation
		// is unrelated and MUST NOT be touched.
		if ownedConditionsStale(freshClass.Status.Conditions, reconciledGen,
			string(gatewayv1.GatewayClassConditionStatusAccepted),
			string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
		) {
			return nil
		}

		condErr := r.setAcceptedConditions(ctx, &freshClass)

		if err := r.Status().Update(ctx, &freshClass); err != nil {
			return errors.Wrap(err, "failed to update GatewayClass status")
		}

		// Whatever setAcceptedConditions set is persisted above. It errors
		// only on a transient read. A failed GatewayClassConfig read leaves
		// both conditions as they stood; a failed CRD read sets Accepted but
		// leaves SupportedVersion unset rather than recording a misleading
		// UnsupportedVersion. Propagating the error requeues the reconcile
		// until the read succeeds.
		return condErr
	})
}

func (r *GatewayClassReconciler) setAcceptedConditions(ctx context.Context, gatewayClass *gatewayv1.GatewayClass) error {
	now := metav1.Now()

	accepted := metav1.Condition{
		Type:               string(gatewayv1.GatewayClassConditionStatusAccepted),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gatewayClass.Generation,
		LastTransitionTime: now,
		Reason:             string(gatewayv1.GatewayClassReasonAccepted),
		Message:            "GatewayClass is accepted by cloudflare-tunnel controller",
	}

	problem, err := r.parametersRefProblem(ctx, gatewayClass.Spec.ParametersRef)
	if err != nil {
		// Transient read error: leave Accepted as it stands rather than blame
		// the spec, and requeue.
		return err
	}

	if problem != "" {
		accepted.Status = metav1.ConditionFalse
		accepted.Reason = string(gatewayv1.GatewayClassReasonInvalidParameters)
		accepted.Message = problem
	}

	meta.SetStatusCondition(&gatewayClass.Status.Conditions, accepted)

	condition, err := r.bundleVersionCondition(ctx, gatewayClass.Generation, now)
	if err != nil {
		// Transient read error: leave SupportedVersion untouched so a momentary
		// failure is not recorded as UnsupportedVersion. Accepted (set above)
		// is still persisted by the caller; the returned error requeues the
		// reconcile to retry the bundle check.
		return err
	}

	meta.SetStatusCondition(&gatewayClass.Status.Conditions, condition)

	return nil
}

// parametersRefProblem says why a GatewayClass parametersRef cannot be served,
// or returns "" when it can. The shape checks are the resolver's, so the
// GatewayClass status and every Gateway on the class agree on what is wrong. A
// non-nil error is a failed read of the referenced config, which says nothing
// about the spec.
func (r *GatewayClassReconciler) parametersRefProblem(
	ctx context.Context,
	ref *gatewayv1.ParametersReference,
) (string, error) {
	if problem := config.ParametersRefProblem(ref); problem != "" {
		return problem, nil
	}

	err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, &v1alpha1.GatewayClassConfig{})
	if apierrors.IsNotFound(err) {
		return fmt.Sprintf("%s %q referenced by spec.parametersRef not found", config.ParametersRefKind, ref.Name), nil
	}

	if err != nil {
		return "", errors.Wrapf(err, "failed to get %s %s", config.ParametersRefKind, ref.Name)
	}

	return "", nil
}

// bundleVersionCondition reads the installed Gateway API CRD bundle version and
// reports whether it is supported. The supported version is the bundle the
// controller is built against (consts.BundleVersion); a CRD bundle with the
// same major.minor is accepted (patch releases are compatible). A deterministic
// mismatch — an older/newer minor, a missing/malformed annotation, or an absent
// CRD — yields UnsupportedVersion so operators see the skew on status rather
// than hitting silent field-drift at runtime. A transient read error
// (apiserver hiccup, RBAC not yet propagated) returns a non-nil error instead,
// so the caller requeues rather than recording a misleading UnsupportedVersion.
func (r *GatewayClassReconciler) bundleVersionCondition(
	ctx context.Context,
	generation int64,
	now metav1.Time,
) (metav1.Condition, error) {
	supported, message, err := r.bundleVersionSupported(ctx)
	if err != nil {
		return metav1.Condition{}, err
	}

	condition := metav1.Condition{
		Type:               string(gatewayv1.GatewayClassConditionStatusSupportedVersion),
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Message:            message,
	}

	if supported {
		condition.Status = metav1.ConditionTrue
		condition.Reason = string(gatewayv1.GatewayClassReasonSupportedVersion)
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = string(gatewayv1.GatewayClassReasonUnsupportedVersion)
	}

	return condition, nil
}

// bundleVersionSupported reads the bundle-version annotation of every served
// Gateway API CRD and compares each major.minor to the version the controller is built against.
// The bool/string pair is the deterministic verdict and a human-readable
// message for the status condition. A non-nil error signals a transient read
// failure (anything other than NotFound): the bundle is unverified, not
// unsupported, so the caller must requeue instead of persisting a verdict.
func (r *GatewayClassReconciler) bundleVersionSupported(ctx context.Context) (bool, string, error) {
	expectedMajor, expectedMinor, ok := parseMajorMinor(consts.BundleVersion)
	if !ok {
		// consts.BundleVersion is a vendored constant; an unparseable value is a
		// build-time bug, not an operator misconfiguration. Fail closed.
		return false, fmt.Sprintf("controller has malformed supported bundle version %q", consts.BundleVersion), nil
	}

	if r.BundleVersionReader == nil {
		return false, "Gateway API CRD bundle version could not be verified: no reader configured", nil
	}

	var installed string

	for _, name := range gatewayAPICRDNames() {
		version, message, err := r.crdBundleVersion(ctx, name)
		if err != nil || message != "" {
			return false, message, err
		}

		installedMajor, installedMinor, ok := parseMajorMinor(version)
		if !ok {
			return false, fmt.Sprintf(
				"Gateway API CRD %q bundle version %q is not a valid version", name, version,
			), nil
		}

		if installedMajor != expectedMajor || installedMinor != expectedMinor {
			return false, fmt.Sprintf(
				"Gateway API CRD %q bundle version %s is not supported; controller requires %d.%d.x",
				name, version, expectedMajor, expectedMinor,
			), nil
		}

		if installed == "" {
			installed = version
		}
	}

	return true, fmt.Sprintf("Gateway API CRD bundle version %s is supported", installed), nil
}

// crdBundleVersion returns the bundle-version annotation of one Gateway API
// CRD. A non-empty message is a deterministic verdict that the CRD is missing
// or unannotated; an error is a transient read failure.
func (r *GatewayClassReconciler) crdBundleVersion(ctx context.Context, name string) (string, string, error) {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := r.BundleVersionReader.Get(ctx, types.NamespacedName{Name: name}, &crd); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Sprintf("Gateway API CRD %q is not installed", name), nil
		}

		return "", "", errors.Wrapf(err, "failed to read Gateway API CRD %q for bundle version", name)
	}

	version := crd.Annotations[consts.BundleVersionAnnotation]
	if version == "" {
		return "", fmt.Sprintf(
			"Gateway API CRD %q is missing the %q annotation", name, consts.BundleVersionAnnotation,
		), nil
	}

	return version, "", nil
}

// parseMajorMinor extracts the major and minor components from a Gateway API
// bundle version string such as "v1.5.1" or "1.5.1". The boolean is false when
// the string lacks at least a major and minor numeric component.
func parseMajorMinor(version string) (int, int, bool) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(version), "v")

	parts := strings.Split(trimmed, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}

	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}

	return major, minor, true
}

// SetupWithManager sets up the controller with the Manager.
//
// It watches GatewayClass and Gateways (for finalizer accounting), but
// deliberately not the Gateway API CRDs:
// the SupportedVersion bundle check is a best-effort signal, and watching CRDs
// would mean a cluster-wide CRD informer plus list/watch RBAC just to react to
// a bundle upgrade. The trade-off is that SupportedVersion is recomputed only
// on the next GatewayClass reconcile (spec change, periodic resync, or
// controller restart), not the instant the CRD bundle changes — a CRD upgrade
// in practice rolls the controller image, which forces that reconcile anyway.
// The Gateway watch keeps the gateway-exists finalizer current: a Gateway
// appearing or disappearing re-reconciles its class so the finalizer is added
// the moment the class comes into use and removed once the last user is gone.
func (r *GatewayClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	//nolint:wrapcheck // controller-runtime builder pattern
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.GatewayClass{}).
		// GenerationChangedPredicate keeps Gateway status writes (every
		// GatewayReconciler pass) from re-reconciling the class; finalizer
		// accounting only needs create/delete and spec changes (which include
		// gatewayClassName moves -- the map func sees both old and new).
		Watches(&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(gatewayClassForGateway),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Only a config's existence feeds Accepted, so create and delete are
		// what matter; the predicate drops status-only updates.
		Watches(&v1alpha1.GatewayClassConfig{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayClassesForConfig),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// gatewayClassesForConfig maps a GatewayClassConfig event to the managed
// GatewayClasses that reference it, so a class waiting on a config that did
// not exist is accepted once it is created, and refused once it is deleted.
func (r *GatewayClassReconciler) gatewayClassesForConfig(ctx context.Context, obj client.Object) []reconcile.Request {
	classes, err := listGatewayClassesForController(ctx, r.Client, r.ControllerName)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to list GatewayClasses for a GatewayClassConfig event")

		return nil
	}

	var requests []reconcile.Request

	for i := range classes {
		if config.RefersToClassConfig(classes[i].Spec.ParametersRef, obj.GetName()) {
			requests = append(requests, reconcile.Request{Name: classes[i].Name})
		}
	}

	return requests
}

// gatewayClassForGateway maps a Gateway event to a reconcile request for the
// GatewayClass it references. The Reconcile controllerName check filters out
// foreign classes, so no filtering is needed here.
func gatewayClassForGateway(_ context.Context, obj client.Object) []reconcile.Request {
	gateway, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}

	return []reconcile.Request{
		{Name: string(gateway.Spec.GatewayClassName)},
	}
}
