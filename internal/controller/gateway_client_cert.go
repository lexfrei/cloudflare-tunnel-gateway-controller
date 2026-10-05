package controller

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// Sentinel errors describing why a Gateway-level client certificate ref could
// not be resolved into a usable keypair. Callers (controller status emit,
// proxy resolver) match on these with errors.Is and map each to the matching
// gatewayv1.GatewayReason* constant on the Gateway's ResolvedRefs condition.
var (
	// errGatewayClientCertUnsupportedRef means the SecretObjectReference points
	// at a kind that's not core/v1 Secret. Per spec we only implement the
	// "Core" support level (kubernetes.io/tls Secret).
	errGatewayClientCertUnsupportedRef = errors.New(
		"gateway client cert: unsupported ref Group/Kind (only core/v1 Secret is supported)")

	// errGatewayClientCertRefNotPermitted means the ref targets a different
	// namespace and no ReferenceGrant authorises the access.
	errGatewayClientCertRefNotPermitted = errors.New(
		"gateway client cert: cross-namespace reference not permitted by any ReferenceGrant")

	// errGatewayClientCertSecretNotFound means the referenced Secret does not
	// exist in the target namespace at the time of resolution.
	errGatewayClientCertSecretNotFound = errors.New(
		"gateway client cert: referenced Secret not found")

	// errGatewayClientCertWrongType means the Secret exists but its Type is
	// not kubernetes.io/tls; per spec only that type is in the Core support
	// level for clientCertificateRef.
	errGatewayClientCertWrongType = errors.New(
		"gateway client cert: Secret is not kubernetes.io/tls")

	// errGatewayClientCertMissingKey means the Secret is missing either the
	// tls.crt or tls.key data entry.
	errGatewayClientCertMissingKey = errors.New(
		"gateway client cert: Secret missing tls.crt or tls.key")

	// errGatewayClientCertInvalidPEM means tls.crt + tls.key are present but
	// do not parse as a valid keypair (tls.X509KeyPair rejects them).
	errGatewayClientCertInvalidPEM = errors.New(
		"gateway client cert: tls.crt/tls.key is not a valid PEM keypair")

	// errGatewayClientCertTransientError marks a transient API-server failure
	// (network, auth refresh, 5xx) returned when fetching the Secret. The ref
	// itself is not necessarily invalid, so callers MUST NOT stamp the Gateway
	// with InvalidClientCertificateRef on this — they leave the previous
	// ResolvedRefs verdict in place and rely on the next reconcile to retry.
	errGatewayClientCertTransientError = errors.New(
		"gateway client cert: transient error fetching Secret")
)

// secretRefGrantChecker is the callback shape loadGatewayClientCertPEM uses to
// authorise cross-namespace Secret references. Production code passes the
// GatewayReconciler's own checkSecretReferenceGrant method so the existing
// ReferenceGrant logic is reused verbatim; tests pass stubs to drive the
// allow / deny branches deterministically.
type secretRefGrantChecker func(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	targetNamespace string,
	ref gatewayv1.SecretObjectReference,
) (bool, error)

// loadGatewayClientCertPEM resolves gateway.spec.tls.backend.clientCertificateRef
// into the tls.crt and tls.key PEM byte slices that the proxy transport layer
// expects in BackendTLSConfig.ClientCertPEM / ClientKeyPEM.
//
// Returns (nil, nil, nil) when the Gateway does not configure a backend client
// cert (TLS, Backend, or ClientCertificateRef is nil). Any other failure
// returns one of the package-level err* sentinels so callers can map to the
// correct GatewayReason on the ResolvedRefs condition without parsing error
// strings.
func loadGatewayClientCertPEM(
	ctx context.Context,
	c client.Client,
	gateway *gatewayv1.Gateway,
	grantChecker secretRefGrantChecker,
) ([]byte, []byte, error) {
	ref := gatewayClientCertRef(gateway)
	if ref == nil {
		return nil, nil, nil
	}

	targetNS, verdict, err := classifyCertRef(gateway.Namespace, *ref, func(targetNamespace string) (bool, error) {
		return grantChecker(ctx, gateway, targetNamespace, *ref)
	})

	switch {
	case err != nil:
		// A grant that could not be read says nothing about the ref, so
		// the status emit path keeps the previous ResolvedRefs verdict.
		return nil, nil, errors.Wrapf(errGatewayClientCertTransientError, "%s", err.Error())
	case verdict == certRefNotPermitted:
		return nil, nil, errGatewayClientCertRefNotPermitted
	case verdict == certRefUnsupportedKind:
		return nil, nil, errGatewayClientCertUnsupportedRef
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: targetNS, Name: string(ref.Name)}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, errGatewayClientCertSecretNotFound
		}

		// Any non-NotFound error from the cached client is transient — auth
		// refresh, API-server 5xx, or pre-informer-sync warmup. The ref itself
		// is not necessarily invalid; wrap as the transient sentinel so the
		// status emit path leaves the previous ResolvedRefs verdict alone
		// instead of falsely declaring InvalidClientCertificateRef.
		return nil, nil, errors.Wrapf(errGatewayClientCertTransientError, "Get %s/%s: %s", targetNS, string(ref.Name), err.Error())
	}

	if secret.Type != corev1.SecretTypeTLS {
		return nil, nil, errGatewayClientCertWrongType
	}

	certPEM, keyPEM := secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey]
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, nil, errGatewayClientCertMissingKey
	}

	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return nil, nil, errors.Wrapf(errGatewayClientCertInvalidPEM, "X509KeyPair: %s", err.Error())
	}

	return certPEM, keyPEM, nil
}

// gatewayClientCertRef returns the configured ClientCertificateRef or nil
// when the Gateway does not configure backend TLS at all.
func gatewayClientCertRef(gateway *gatewayv1.Gateway) *gatewayv1.SecretObjectReference {
	if gateway.Spec.TLS == nil || gateway.Spec.TLS.Backend == nil {
		return nil
	}

	return gateway.Spec.TLS.Backend.ClientCertificateRef
}

// applyGatewayConditions merges the controller-owned Gateway-level conditions
// (Accepted, Programmed, and the optional client-cert ResolvedRefs) into the
// existing condition slice via meta.SetStatusCondition. Merging by type — rather
// than reassigning the whole slice — preserves condition types this controller
// does not own, as the Gateway API spec requires: "Implementations MUST NOT
// remove or reorder Conditions that they are not directly responsible for"
// (gateway_types.go:994-997). meta.SetStatusCondition also manages
// LastTransitionTime, so a condition's timestamp only moves on an actual
// status change.
//
// A nil clientCertCond means the client-cert outcome was transient: the
// previous ResolvedRefs condition is left untouched (not overwritten), since
// SetStatusCondition is simply not called for it.
func applyGatewayConditions(conditions *[]metav1.Condition, owned []metav1.Condition, clientCertCond *metav1.Condition) {
	for i := range owned {
		meta.SetStatusCondition(conditions, owned[i])
	}

	if clientCertCond != nil {
		meta.SetStatusCondition(conditions, *clientCertCond)
	}
}

// buildClientCertResolvedRefsCondition maps the outcome of
// loadGatewayClientCertPEM onto a Gateway-level ResolvedRefs condition, or
// returns nil when the outcome should not move the condition at all.
//
// Per Gateway API spec on the GatewayBackendTLS type:
//
//   - nil error → ConditionTrue / Reason=ResolvedRefs.
//   - cross-namespace denial → ConditionFalse / Reason=RefNotPermitted.
//   - any other ref-validity error (unsupported kind, missing Secret, wrong
//     type, missing tls.crt|tls.key, malformed PEM) →
//     ConditionFalse / Reason=InvalidClientCertificateRef.
//   - a transient API-server error (network, auth, 5xx) → returns nil so the
//     caller preserves the previous verdict. The spec reserves
//     InvalidClientCertificateRef for actual data problems, not transient
//     control-plane hiccups — the next reconcile will retry.
//
// The FIRST-PR scope is intentionally narrow: this condition reflects only
// the client-cert outcome. The spec also allows Gateway ResolvedRefs to be a
// positive-polarity summary across listener-level ResolvedRefs; that broader
// semantic is left for a follow-up since the upstream conformance test only
// asserts the client-cert path.
func buildClientCertResolvedRefsCondition(generation int64, now metav1.Time, err error) *metav1.Condition {
	if err == nil {
		return &metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionResolvedRefs),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.GatewayReasonResolvedRefs),
			Message:            "All references resolved",
		}
	}

	if errors.Is(err, errGatewayClientCertTransientError) {
		// Leave the previous condition in place — the ref itself is fine,
		// the API server just failed to answer. Next reconcile retries.
		return nil
	}

	reason := gatewayv1.GatewayReasonInvalidClientCertificateRef
	if errors.Is(err, errGatewayClientCertRefNotPermitted) {
		reason = gatewayv1.GatewayReasonRefNotPermitted
	}

	return &metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionResolvedRefs),
		Status:             metav1.ConditionFalse,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             string(reason),
		Message:            err.Error(),
	}
}

// checkSecretReferenceGrantForGateway walks the ReferenceGrants in the target
// namespace and reports whether any grants the Gateway's namespace access to
// the referenced object. A free function so the ProxySyncer can authorise the
// same cross-namespace path without holding a GatewayReconciler reference.
func checkSecretReferenceGrantForGateway(
	ctx context.Context,
	c client.Client,
	gateway *gatewayv1.Gateway,
	targetNamespace string,
	ref gatewayv1.SecretObjectReference,
) (bool, error) {
	return referenceGrantPermitsRef(ctx, c, targetNamespace, ref, func(grant *gatewayv1beta1.ReferenceGrant) bool {
		return grantAllowsGatewayFromNamespace(grant, gateway.Namespace)
	})
}

// referenceGrantPermitsRef reports whether a ReferenceGrant in targetNamespace
// admits the referrer (decided by fromAllowed) and names the ref's own group
// and kind: a grant to Secrets does not permit a reference to anything else.
func referenceGrantPermitsRef(
	ctx context.Context,
	c client.Client,
	targetNamespace string,
	ref gatewayv1.SecretObjectReference,
	fromAllowed func(*gatewayv1beta1.ReferenceGrant) bool,
) (bool, error) {
	var grants gatewayv1beta1.ReferenceGrantList
	if err := c.List(ctx, &grants, client.InNamespace(targetNamespace)); err != nil {
		return false, errors.Wrapf(err, "listing ReferenceGrants in %s", targetNamespace)
	}

	group, kind := secretRefGroupKind(&ref)

	for i := range grants.Items {
		if !fromAllowed(&grants.Items[i]) {
			continue
		}

		for _, to := range grants.Items[i].Spec.To {
			if string(to.Group) != group || string(to.Kind) != kind {
				continue
			}

			if to.Name == nil || *to.Name == "" || string(*to.Name) == string(ref.Name) {
				return true, nil
			}
		}
	}

	return false, nil
}

// certRefVerdict is what a certificate reference earns before its target is
// read.
type certRefVerdict int

const (
	certRefAllowed certRefVerdict = iota
	certRefNotPermitted
	certRefUnsupportedKind
)

// classifyCertRef resolves the ref's namespace and checks the ReferenceGrant
// before the kind: the spec reserves InvalidCertificateRef and
// InvalidClientCertificateRef for a reference that is allowed, so a
// cross-namespace reference no grant permits is RefNotPermitted whatever it
// points at. granted is consulted only for a cross-namespace reference.
func classifyCertRef(
	ownerNamespace string,
	ref gatewayv1.SecretObjectReference,
	granted func(targetNamespace string) (bool, error),
) (string, certRefVerdict, error) {
	targetNamespace := ownerNamespace
	if ref.Namespace != nil {
		targetNamespace = string(*ref.Namespace)
	}

	if targetNamespace != ownerNamespace {
		allowed, err := granted(targetNamespace)
		if err != nil {
			return targetNamespace, certRefNotPermitted, err
		}

		if !allowed {
			return targetNamespace, certRefNotPermitted, nil
		}
	}

	if !isCoreSecretRef(&ref) {
		return targetNamespace, certRefUnsupportedKind, nil
	}

	return targetNamespace, certRefAllowed, nil
}

// grantAllowsGatewayFromNamespace reports whether the grant admits Gateways
// from gatewayNamespace.
func grantAllowsGatewayFromNamespace(grant *gatewayv1beta1.ReferenceGrant, gatewayNamespace string) bool {
	for _, from := range grant.Spec.From {
		if from.Group == gatewayv1.GroupName &&
			from.Kind == kindGateway &&
			string(from.Namespace) == gatewayNamespace {
			return true
		}
	}

	return false
}

// isCoreSecretRef reports whether the ref targets a core/v1 Secret. nil
// Group/Kind are treated as the spec defaults.
func isCoreSecretRef(ref *gatewayv1.SecretObjectReference) bool {
	return isCoreSecret(secretRefGroupKind(ref))
}

func unsupportedCertRefMessage(ref *gatewayv1.SecretObjectReference) string {
	group, kind := secretRefGroupKind(ref)

	return fmt.Sprintf("Unsupported certificate ref kind: %s/%s", group, kind)
}

// secretRefGroupKind returns the ref's group and kind with the spec defaults
// (core group, Secret) applied.
func secretRefGroupKind(ref *gatewayv1.SecretObjectReference) (string, string) {
	group := ""
	if ref.Group != nil {
		group = string(*ref.Group)
	}

	kind := kindSecret
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}

	return group, kind
}

// isCoreSecret is the one group/kind test for a Secret reference and for the
// ReferenceGrant entry authorising it. Unlike backendRefs (see coregroup), only
// the canonical empty group names the core group here.
func isCoreSecret(group, kind string) bool {
	return group == "" && kind == kindSecret
}
