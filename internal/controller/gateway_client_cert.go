package controller

import (
	"context"
	"crypto/tls"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	if !isCoreSecretRef(ref) {
		return nil, nil, errGatewayClientCertUnsupportedRef
	}

	targetNS := gateway.Namespace
	if ref.Namespace != nil {
		targetNS = string(*ref.Namespace)
	}

	if targetNS != gateway.Namespace {
		allowed, err := grantChecker(ctx, gateway, targetNS, *ref)
		if err != nil {
			return nil, nil, err
		}

		if !allowed {
			return nil, nil, errGatewayClientCertRefNotPermitted
		}
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: targetNS, Name: string(ref.Name)}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, errGatewayClientCertSecretNotFound
		}

		return nil, nil, errors.Wrap(err, "failed to get client certificate Secret")
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

// buildClientCertResolvedRefsCondition maps the outcome of
// loadGatewayClientCertPEM onto a Gateway-level ResolvedRefs condition.
// Per Gateway API spec on the GatewayBackendTLS type:
//
//   - nil error → ConditionTrue / Reason=ResolvedRefs.
//   - cross-namespace denial → ConditionFalse / Reason=RefNotPermitted.
//   - any other resolution error (unsupported kind, missing Secret, wrong
//     type, missing tls.crt|tls.key, malformed PEM) →
//     ConditionFalse / Reason=InvalidClientCertificateRef.
//
// The FIRST-PR scope is intentionally narrow: this condition reflects only
// the client-cert outcome. The spec also allows Gateway ResolvedRefs to be a
// positive-polarity summary across listener-level ResolvedRefs; that broader
// semantic is left for a follow-up since the upstream conformance test only
// asserts the client-cert path.
func buildClientCertResolvedRefsCondition(generation int64, now metav1.Time, err error) metav1.Condition {
	if err == nil {
		return metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionResolvedRefs),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.GatewayReasonResolvedRefs),
			Message:            "All references resolved",
		}
	}

	reason := gatewayv1.GatewayReasonInvalidClientCertificateRef
	if errors.Is(err, errGatewayClientCertRefNotPermitted) {
		reason = gatewayv1.GatewayReasonRefNotPermitted
	}

	return metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionResolvedRefs),
		Status:             metav1.ConditionFalse,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             string(reason),
		Message:            err.Error(),
	}
}

// checkSecretReferenceGrantForGateway is the standalone equivalent of
// GatewayReconciler.checkSecretReferenceGrant — both walk ReferenceGrants in
// the target namespace and report whether any grants the Gateway's namespace
// access to the referenced Secret. Extracted as a free function so the
// ProxySyncer can authorise the same cross-namespace path without holding a
// GatewayReconciler reference. The behaviour is byte-identical to the
// receiver-method version.
func checkSecretReferenceGrantForGateway(
	ctx context.Context,
	c client.Client,
	gateway *gatewayv1.Gateway,
	targetNamespace string,
	ref gatewayv1.SecretObjectReference,
) (bool, error) {
	var grants gatewayv1beta1.ReferenceGrantList
	if err := c.List(ctx, &grants, client.InNamespace(targetNamespace)); err != nil {
		return false, errors.Wrap(err, "failed to list ReferenceGrants")
	}

	for i := range grants.Items {
		if !grantAllowsGatewayFromNamespace(&grants.Items[i], gateway.Namespace) {
			continue
		}

		for _, to := range grants.Items[i].Spec.To {
			if to.Group != "" || to.Kind != kindSecret {
				continue
			}

			if to.Name == nil || *to.Name == "" || string(*to.Name) == string(ref.Name) {
				return true, nil
			}
		}
	}

	return false, nil
}

// grantAllowsGatewayFromNamespace mirrors GatewayReconciler.grantAllowsGateway
// as a free function so the standalone reference-grant check above can reuse
// the same predicate.
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

// isCoreSecretRef reports whether the ref targets a core/v1 Secret (Group ""
// and Kind "Secret"). nil Group/Kind are treated as the spec defaults.
func isCoreSecretRef(ref *gatewayv1.SecretObjectReference) bool {
	group := ""
	if ref.Group != nil {
		group = string(*ref.Group)
	}

	kind := kindSecret
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}

	return group == "" && kind == kindSecret
}
