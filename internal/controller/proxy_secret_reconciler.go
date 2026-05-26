package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/cockroachdb/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
)

// tokenRevisionAnnotation is stamped onto the proxy Deployment's pod
// template every time the tunnel-token Secret changes. Its value is the
// hex SHA-256 of the Secret's data map; toggling it forces the Deployment
// controller to roll the pods because the pod template hash changes.
//
// The annotation is namespaced to this project so it cannot collide with
// any Stakater Reloader / kustomize / external-restart tool the operator
// might also be running on the same Deployment.
//
//nolint:gosec // G101: this is an annotation key, not a credential.
const tokenRevisionAnnotation = "cf.k8s.lex.la/tunnel-token-revision"

// Static sentinels for config-parse failures so err113 stays happy and
// callers can `errors.Is` on them.
var (
	errEmptyProxyTokenSecret       = errors.New("--proxy-token-secret must not be empty")
	errInvalidProxyTokenSecret     = errors.New("--proxy-token-secret must be in `<namespace>/<name>` format")
	errInvalidProxyDeploymentLabel = errors.New("--proxy-deployment-label must be in `key=value` format")
)

// ProxySecretReconciler watches the tunnel-token Secret named in
// --proxy-token-secret. On any change to the Secret it patches the proxy
// Deployment(s)' pod template with a fresh revision annotation, which
// triggers a native rolling restart so cloudflared picks up the rotated
// credential.
//
// This closes the gap left by ESO / External Secrets Operator rotating
// the source Secret out-of-band: Kubernetes does NOT restart pods on
// Secret data change by itself; the env-from-secretKeyRef stays at the
// stale value until something bumps the pod template. Issue #114.
//
// When ProxyTokenSecret is empty (no --proxy-token-secret flag), the
// reconciler skips registration entirely.
type ProxySecretReconciler struct {
	Client client.Client

	// TokenSecretNamespace is the namespace of the tunnel-token Secret.
	TokenSecretNamespace string

	// TokenSecretName is the name of the tunnel-token Secret.
	TokenSecretName string

	// DeploymentLabelKey / DeploymentLabelValue select the proxy
	// Deployment(s) to roll on Secret change. When unset, defaults to
	// `app.kubernetes.io/component=proxy`.
	DeploymentLabelKey   string
	DeploymentLabelValue string
}

// NewProxySecretReconciler parses the controller config's
// `<namespace>/<name>` token-secret reference and optional
// `key=value` deployment label. Caller MUST ensure proxyTokenSecret is
// non-empty before invoking; empty is treated as a programmer error,
// not a runtime "watcher disabled" path (the manager's wiring layer
// filters that case out before getting here).
func NewProxySecretReconciler(c client.Client, proxyTokenSecret, proxyDeploymentLabel string) (*ProxySecretReconciler, error) {
	proxyTokenSecret = strings.TrimSpace(proxyTokenSecret)
	if proxyTokenSecret == "" {
		return nil, errEmptyProxyTokenSecret
	}

	parts := strings.SplitN(proxyTokenSecret, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("%w: got %q", errInvalidProxyTokenSecret, proxyTokenSecret)
	}

	labelKey := "app.kubernetes.io/component"
	labelValue := "proxy"

	if trimmed := strings.TrimSpace(proxyDeploymentLabel); trimmed != "" {
		labelParts := strings.SplitN(trimmed, "=", 2)
		if len(labelParts) != 2 || labelParts[0] == "" || labelParts[1] == "" {
			return nil, fmt.Errorf("%w: got %q", errInvalidProxyDeploymentLabel, proxyDeploymentLabel)
		}

		labelKey = labelParts[0]
		labelValue = labelParts[1]
	}

	return &ProxySecretReconciler{
		Client:               c,
		TokenSecretNamespace: parts[0],
		TokenSecretName:      parts[1],
		DeploymentLabelKey:   labelKey,
		DeploymentLabelValue: labelValue,
	}, nil
}

// Reconcile fires on any change to the watched tunnel-token Secret. It
// hashes the Secret's data map and patches every matching proxy
// Deployment's pod template annotation with that hash. A no-op patch
// (same hash) is filtered out by the kube-apiserver before the
// Deployment controller observes it, so there is no spurious rollout
// on benign Secret events (e.g. resourceVersion-only churn from a
// metadata controller).
func (r *ProxySecretReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logging.Component(ctx, "proxy-secret-reconciler")
	ctx = logging.WithLogger(ctx, logger)

	var secret corev1.Secret
	if err := r.Client.Get(ctx, req.NamespacedName, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			// Secret was deleted; nothing to propagate. The proxy will
			// fail open on its next restart anyway -- we don't preemptively
			// scramble the Deployment annotation.
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, errors.Wrap(err, "fetch tunnel-token Secret")
	}

	revision := hashSecretData(secret.Data)

	var deployments appsv1.DeploymentList

	listOpts := []client.ListOption{
		client.InNamespace(secret.Namespace),
		client.MatchingLabels{r.DeploymentLabelKey: r.DeploymentLabelValue},
	}

	if err := r.Client.List(ctx, &deployments, listOpts...); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "list proxy Deployments")
	}

	for idx := range deployments.Items {
		dep := &deployments.Items[idx]
		if err := r.patchRevision(ctx, dep, revision); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "patch Deployment %s/%s", dep.Namespace, dep.Name)
		}
	}

	logger.Info("proxy tunnel-token Secret reconciled",
		"secret", req.String(),
		"revision", revision,
		"deployments_patched", len(deployments.Items),
	)

	return ctrl.Result{}, nil
}

// patchRevision sets the token-revision annotation on the Deployment's
// pod template. If the existing value already matches the new revision
// the call is skipped -- a server-side apply with an identical patch
// body still bumps the Deployment's generation in some Kubernetes
// versions, which would spuriously roll the pods.
func (r *ProxySecretReconciler) patchRevision(ctx context.Context, dep *appsv1.Deployment, revision string) error {
	current := ""
	if dep.Spec.Template.Annotations != nil {
		current = dep.Spec.Template.Annotations[tokenRevisionAnnotation]
	}

	if current == revision {
		return nil
	}

	patch := client.MergeFrom(dep.DeepCopy())

	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}

	dep.Spec.Template.Annotations[tokenRevisionAnnotation] = revision

	if err := r.Client.Patch(ctx, dep, patch); err != nil {
		return errors.Wrap(err, "merge-patch Deployment annotation")
	}

	return nil
}

// SetupWithManager registers the watcher with the manager. The
// predicate filters EVERY non-target Secret event out before it
// enqueues a Reconcile request -- the controller is per-Secret, so
// any other Secret churn in the cluster is irrelevant.
func (r *ProxySecretReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("proxy-secret-reconciler").
		For(&corev1.Secret{}, builder.WithPredicates(r.matchesTokenSecret())).
		Complete(r); err != nil {
		return errors.Wrap(err, "setup proxy secret reconciler")
	}

	return nil
}

func (r *ProxySecretReconciler) matchesTokenSecret() predicate.Predicate {
	matches := func(obj client.Object) bool {
		return obj.GetNamespace() == r.TokenSecretNamespace && obj.GetName() == r.TokenSecretName
	}

	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return matches(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return matches(e.ObjectNew) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return matches(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return matches(e.Object) },
	}
}

// TokenSecretKey returns the NamespacedName of the watched Secret;
// exposed for tests so they can synthesise reconcile requests without
// reaching into the struct directly.
func (r *ProxySecretReconciler) TokenSecretKey() types.NamespacedName {
	return types.NamespacedName{Namespace: r.TokenSecretNamespace, Name: r.TokenSecretName}
}

// hashSecretData returns the hex SHA-256 of the Secret's data map,
// computed in a key-sorted order so the result is deterministic and
// independent of map iteration order. Used as the value of the
// pod-template revision annotation.
func hashSecretData(data map[string][]byte) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	hasher := sha256.New()
	for _, k := range keys {
		_, _ = fmt.Fprintf(hasher, "%s=", k)
		_, _ = hasher.Write(data[k])
		_, _ = hasher.Write([]byte{0})
	}

	return hex.EncodeToString(hasher.Sum(nil))
}
