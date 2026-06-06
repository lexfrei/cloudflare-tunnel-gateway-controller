package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// errSimulatedCacheMiss is reused by tests that need an interceptor to fail
// a cache List call deterministically.
var errSimulatedCacheMiss = errors.New("simulated cache miss")

// generateSelfSignedCAPEM produces a tiny self-signed CA cert and returns it
// as a PEM string. Used so tests exercise the same parseCABundle path that
// production controllers run.
func generateSelfSignedCAPEM(t *testing.T) string {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageCertSign,
		IsCA:         true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	require.NoError(t, err)

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	return string(pemBytes)
}

// gatewayClassFor builds a GatewayClass tied to the given controllerName.
func gatewayClassFor(name, controllerName string) *gatewayv1.GatewayClass {
	return &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewayv1.GatewayController(controllerName)},
	}
}

// gatewayFor builds a Gateway in ns with the given GatewayClass reference.
func gatewayFor(namespace, name, gatewayClassName string) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(gatewayClassName)},
	}
}

// httpRouteFor builds an HTTPRoute with one rule pointing at the given backend
// Service. parentGwName is recorded as the only parentRef in the same namespace.
func httpRouteFor(namespace, name, parentGwName, backendServiceName string) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(parentGwName)},
				},
			},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: gatewayv1.ObjectName(backendServiceName),
								},
							},
						},
					},
				},
			},
		},
	}
}

// caConfigMap builds a ConfigMap in ns/name holding the supplied PEM bundle
// under the "ca.crt" key.
func caConfigMap(namespace, name, pemBundle string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Data:       map[string]string{configMapCAKey: pemBundle},
	}
}

// backendTLSPolicyFor builds a BackendTLSPolicy targeting the named Service
// with the given CA-cert ConfigMap reference. Optional creationTimestamp lets
// tests model precedence ordering.
func backendTLSPolicyFor(
	namespace, name, serviceName, configMapName string,
	creationTimestamp time.Time,
) *gatewayv1.BackendTLSPolicy {
	kindService := gatewayv1.Kind(serviceKind)
	kindConfigMap := gatewayv1.Kind(configMapKind)

	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  namespace,
			Name:       name,
			Generation: 1,
		},
		Spec: gatewayv1.BackendTLSPolicySpec{
			TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
				{
					LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{
						Kind: kindService,
						Name: gatewayv1.ObjectName(serviceName),
					},
				},
			},
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{
						Kind: kindConfigMap,
						Name: gatewayv1.ObjectName(configMapName),
					},
				},
				Hostname: "test.example.com",
			},
		},
	}

	if !creationTimestamp.IsZero() {
		policy.CreationTimestamp = metav1.NewTime(creationTimestamp)
	}

	return policy
}

func newBackendTLSPolicyScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	return scheme
}

// ---- parseCABundle ----

func TestParseCABundle_AcceptsValidCert(t *testing.T) {
	t.Parallel()

	pemString := generateSelfSignedCAPEM(t)

	count, err := parseCABundle(pemString)

	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestParseCABundle_RejectsNoCertificateBlocks(t *testing.T) {
	t.Parallel()

	_, err := parseCABundle("not a pem block at all")
	require.Error(t, err)
	assert.True(t, errors.Is(err, errBackendTLSCABundleNoCerts))
}

func TestParseCABundle_RejectsMalformedCert(t *testing.T) {
	t.Parallel()

	garbagePEM := "-----BEGIN CERTIFICATE-----\nQUFBQQ==\n-----END CERTIFICATE-----\n"

	_, err := parseCABundle(garbagePEM)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errBackendTLSCABundleMalformed))
}

// ---- validateCARefs ----

func TestValidateCARefs_NoRefs(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"},
	}

	err := r.validateCARefs(context.Background(), policy)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errBackendTLSNoCARef))
}

func TestValidateCARefs_UnsupportedGroup(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	badGroup := gatewayv1.Group("apps")
	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Group: badGroup, Kind: configMapKind, Name: "x"},
				},
			},
		},
	}

	err := r.validateCARefs(context.Background(), policy)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errBackendTLSUnsupportedGroup))
}

func TestValidateCARefs_UnsupportedKind(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p"},
		Spec: gatewayv1.BackendTLSPolicySpec{
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Kind: "Secret", Name: "x"},
				},
			},
		},
	}

	err := r.validateCARefs(context.Background(), policy)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errBackendTLSUnsupportedKind))
}

func TestValidateCARefs_ConfigMapNotFound(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	policy := backendTLSPolicyFor("ns", "p", "svc", "missing-cm", time.Time{})

	err := r.validateCARefs(context.Background(), policy)
	require.Error(t, err)
	// Not-found errors aren't wrapped by a sentinel — only verify it errors.
}

func TestValidateCARefs_EmptyCAKey(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	configMap := caConfigMap("ns", "cm", "")
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})

	err := r.validateCARefs(context.Background(), policy)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errBackendTLSCAKeyMissing))
}

func TestValidateCARefs_MalformedPEM(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	configMap := caConfigMap("ns", "cm", "not actually pem")
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})

	err := r.validateCARefs(context.Background(), policy)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errBackendTLSCABundleNoCerts))
}

func TestValidateCARefs_HappyPath(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})

	err := r.validateCARefs(context.Background(), policy)
	require.NoError(t, err)
}

// ---- computeConditions ----

func TestComputeConditions_HappyPath(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	policy.Generation = 5

	conditions := r.computeConditions(context.Background(), policy)

	require.Len(t, conditions, 2)
	assert.Equal(t, string(gatewayv1.PolicyConditionAccepted), conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, conditions[0].Status)
	assert.Equal(t, string(gatewayv1.PolicyReasonAccepted), conditions[0].Reason)
	assert.Equal(t, int64(5), conditions[0].ObservedGeneration)
	assert.Equal(t, string(gatewayv1.BackendTLSPolicyConditionResolvedRefs), conditions[1].Type)
	assert.Equal(t, metav1.ConditionTrue, conditions[1].Status)
}

func TestComputeConditions_InvalidKind(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", Generation: 3},
		Spec: gatewayv1.BackendTLSPolicySpec{
			Validation: gatewayv1.BackendTLSPolicyValidation{
				CACertificateRefs: []gatewayv1.LocalObjectReference{
					{Kind: "Secret", Name: "secret-as-ca"},
				},
			},
		},
	}

	conditions := r.computeConditions(context.Background(), policy)

	require.Len(t, conditions, 2)
	assert.Equal(t, metav1.ConditionFalse, conditions[0].Status)
	assert.Equal(t, string(gatewayv1.BackendTLSPolicyReasonNoValidCACertificate), conditions[0].Reason)
	assert.Equal(t, string(gatewayv1.BackendTLSPolicyReasonInvalidKind), conditions[1].Reason)
	assert.Equal(t, int64(3), conditions[0].ObservedGeneration)
	assert.Equal(t, int64(3), conditions[1].ObservedGeneration)
}

func TestComputeConditions_InvalidCACertificateRef(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	policy := backendTLSPolicyFor("ns", "p", "svc", "nonexistent-cm", time.Time{})

	conditions := r.computeConditions(context.Background(), policy)

	require.Len(t, conditions, 2)
	assert.Equal(t, metav1.ConditionFalse, conditions[0].Status)
	assert.Equal(t, string(gatewayv1.BackendTLSPolicyReasonNoValidCACertificate), conditions[0].Reason)
	assert.Equal(t, string(gatewayv1.BackendTLSPolicyReasonInvalidCACertificateRef), conditions[1].Reason)
}

// TestComputeConditions_WellKnownUnsupportedEmitsInvalid pins the Gateway API
// requirement (backendtlspolicy_types.go:206-209): if an implementation does not
// support the WellKnownCACertificates field, it MUST set Accepted=False with
// Reason=Invalid. This controller supports only explicit CACertificateRefs, so a
// WellKnown-only policy — which the CRD CEL admits (empty caCertificateRefs +
// wellKnownCACertificates set) — must surface Reason=Invalid, not the generic
// NoValidCACertificate.
func TestComputeConditions_WellKnownUnsupportedEmitsInvalid(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	system := gatewayv1.WellKnownCACertificatesSystem
	policy := &gatewayv1.BackendTLSPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", Generation: 7},
		Spec: gatewayv1.BackendTLSPolicySpec{
			Validation: gatewayv1.BackendTLSPolicyValidation{
				WellKnownCACertificates: &system,
			},
		},
	}

	conditions := r.computeConditions(context.Background(), policy)

	accepted := findCondition(conditions, string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.PolicyReasonInvalid), accepted.Reason,
		"unsupported WellKnownCACertificates MUST yield Accepted=False/Invalid per backendtlspolicy_types.go:206-209")
	assert.Equal(t, int64(7), accepted.ObservedGeneration)
}

// TestComputeConditions_NeverEmitsPolicyReasonInvalid pins the invariant
// that — since this controller fully supports both Hostname and URI
// SubjectAltNames — `computeConditions` MUST NOT stamp the Accepted Reason
// with `PolicyReasonInvalid` for a policy with valid CA references. The
// previous controller emitted that Reason when it rejected URI SANs; that
// path is gone. (The unsupported-WellKnownCACertificates path legitimately
// does emit Invalid — see TestComputeConditions_WellKnownUnsupportedEmitsInvalid;
// every policy here carries explicit CACertificateRefs so it is unaffected.)
// A regression that silently reintroduces the SAN rejection branch must
// surface here, not at conformance time on homelab.
func TestComputeConditions_NeverEmitsPolicyReasonInvalid(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	noSAN := backendTLSPolicyFor("ns", "no-san", "svc", "cm", time.Time{})

	hostOnly := backendTLSPolicyFor("ns", "host-only", "svc", "cm", time.Time{})
	hostOnly.Spec.Validation.SubjectAltNames = []gatewayv1.SubjectAltName{
		{Type: gatewayv1.HostnameSubjectAltNameType, Hostname: "alt.example.com"},
	}

	uriOnly := backendTLSPolicyFor("ns", "uri-only", "svc", "cm", time.Time{})
	uriOnly.Spec.Validation.SubjectAltNames = []gatewayv1.SubjectAltName{
		{Type: gatewayv1.URISubjectAltNameType, URI: "spiffe://example/identity"},
	}

	mixed := backendTLSPolicyFor("ns", "mixed", "svc", "cm", time.Time{})
	mixed.Spec.Validation.SubjectAltNames = []gatewayv1.SubjectAltName{
		{Type: gatewayv1.HostnameSubjectAltNameType, Hostname: "alt.example.com"},
		{Type: gatewayv1.URISubjectAltNameType, URI: "spiffe://example/identity"},
	}

	for _, policy := range []*gatewayv1.BackendTLSPolicy{noSAN, hostOnly, uriOnly, mixed} {
		t.Run(policy.Name, func(t *testing.T) {
			t.Parallel()

			conds := r.computeConditions(context.Background(), policy)
			require.NotEmpty(t, conds)

			for _, condition := range conds {
				if condition.Type == string(gatewayv1.PolicyConditionAccepted) {
					assert.NotEqual(t, string(gatewayv1.PolicyReasonInvalid), condition.Reason,
						"controller MUST NOT emit Reason=Invalid on Accepted — that path was removed when URI SANs became supported")
				}
			}
		})
	}
}

// TestBackendTLSResolver_UnknownSANType_ReturnsPoisonedConfig pins the
// CRD-newer-than-controller defence: if a future Gateway API release adds a
// SubjectAltName type (Email, IP, etc.) and a cluster's CRD ships that enum
// value, the resolver MUST fail closed rather than silently enforce the
// subset it understands. Otherwise an operator who writes a policy requiring
// the new SAN type would get plaintext-equivalent enforcement, downgrading
// their stated intent.
func TestBackendTLSResolver_UnknownSANType_ReturnsPoisonedConfig(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	// Fabricated SAN type — not Hostname or URI. Mimics the case where the
	// cluster CRD enum is ahead of this controller's compiled spec.
	policy.Spec.Validation.SubjectAltNames = []gatewayv1.SubjectAltName{
		{Type: gatewayv1.SubjectAltNameType("Email")},
	}

	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, configMap).
		Build()

	resolver := newBackendTLSResolver(fakeClient)

	got := resolver(context.Background(), "ns", "svc", 443)
	require.NotNil(t, got, "policy targets the Service — resolver MUST NOT return nil (would downgrade to plaintext)")
	assert.Empty(t, got.CABundlePEM,
		"unknown SAN type → poisoned config (empty CA pool) so handshake fails closed")
	assert.Empty(t, got.SubjectAltNames,
		"poisoned config drops the partial SAN list")
	assert.Empty(t, got.SubjectAltNameURIs,
		"poisoned config drops URI SAN list too")
}

// TestComputeConditions_URISANAccepted verifies that a BackendTLSPolicy
// carrying URI-type SubjectAltNames is accepted end-to-end (Accepted=True,
// ResolvedRefs=True). URI SANs (e.g. SPIFFE IDs) are matched against the
// leaf cert's URIs[] by the proxy.
func TestComputeConditions_URISANAccepted(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	policy.Generation = 7
	policy.Spec.Validation.SubjectAltNames = []gatewayv1.SubjectAltName{
		{Type: gatewayv1.URISubjectAltNameType, URI: "spiffe://abc.example.com/test-identity"},
	}

	conditions := r.computeConditions(context.Background(), policy)

	require.Len(t, conditions, 2)
	assert.Equal(t, metav1.ConditionTrue, conditions[0].Status,
		"URI SubjectAltName is now fully supported — Accepted MUST be True")
	assert.Equal(t, string(gatewayv1.PolicyReasonAccepted), conditions[0].Reason)
	assert.Equal(t, metav1.ConditionTrue, conditions[1].Status)
	assert.Equal(t, string(gatewayv1.BackendTLSPolicyReasonResolvedRefs), conditions[1].Reason)
	assert.Equal(t, int64(7), conditions[0].ObservedGeneration)
}

// ---- computeConditions: GEP-713 conflict resolution ----

// TestComputeConditions_LoserStampedConflicted pins GEP-713 semantics:
// when two BackendTLSPolicies target the same Service, the policy with the
// older creationTimestamp wins (Accepted=True) and the newer one is stamped
// Accepted=False, Reason=Conflicted, with a Message naming the winner so
// operators can find the conflicting policy without grepping the whole
// namespace.
func TestComputeConditions_LoserStampedConflicted(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))
	winner := backendTLSPolicyFor("ns", "winner", "svc", "cm", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	loser := backendTLSPolicyFor("ns", "loser", "svc", "cm", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(configMap, winner, loser).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	loserConds := r.computeConditions(context.Background(), loser)
	require.Len(t, loserConds, 2)

	accepted := findCondition(loserConds, string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, accepted, "Accepted condition must be present on the losing policy")
	assert.Equal(t, metav1.ConditionFalse, accepted.Status, "loser MUST be Accepted=False")
	assert.Equal(t, string(gatewayv1.PolicyReasonConflicted), accepted.Reason)
	assert.Contains(t, accepted.Message, "ns/winner",
		"Conflicted Message must name the winner so operators can locate the conflict")

	winnerConds := r.computeConditions(context.Background(), winner)
	winnerAccepted := findCondition(winnerConds, string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, winnerAccepted)
	assert.Equal(t, metav1.ConditionTrue, winnerAccepted.Status, "older policy must remain Accepted=True")
}

// TestComputeConditions_NotConflictedWithDistinctSectionName pins the
// third sub-test of BackendTLSPolicyConflictResolution: one policy with
// SectionName scoping itself to a specific named Service port, another
// without SectionName covering all ports. Per GEP-713 these are
// different scopes — both stay Accepted=True. The literal SectionName
// comparison in normalizePolicyTargets implements this rule.
func TestComputeConditions_NotConflictedWithDistinctSectionName(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))

	scoped := backendTLSPolicyFor("ns", "scoped", "svc", "cm", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	sectionName := gatewayv1.SectionName("https")
	scoped.Spec.TargetRefs[0].SectionName = &sectionName

	unscoped := backendTLSPolicyFor("ns", "unscoped", "svc", "cm", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(configMap, scoped, unscoped).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	for _, policy := range []*gatewayv1.BackendTLSPolicy{scoped, unscoped} {
		t.Run(policy.Name, func(t *testing.T) {
			t.Parallel()

			conds := r.computeConditions(context.Background(), policy)

			accepted := findCondition(conds, string(gatewayv1.PolicyConditionAccepted))
			require.NotNil(t, accepted)
			assert.Equal(t, metav1.ConditionTrue, accepted.Status,
				"scoped (with SectionName) and unscoped (no SectionName) policies are different scopes per GEP-713 — neither conflicts with the other")
			assert.Equal(t, string(gatewayv1.PolicyReasonAccepted), accepted.Reason)
		})
	}
}

// TestComputeConditions_TieBreakAlphabetical pins the deterministic
// tie-break used when two conflicting policies share a creationTimestamp:
// the lexicographically smaller name wins (Accepted=True), the larger
// loses (Accepted=False, Reason=Conflicted). Without a deterministic
// tie-break, the controller would flap between two "winners" depending
// on List ordering, which is informer-cache-dependent and produces
// non-deterministic Status.
func TestComputeConditions_TieBreakAlphabetical(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))
	sameTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	alpha := backendTLSPolicyFor("ns", "alpha", "svc", "cm", sameTime)
	beta := backendTLSPolicyFor("ns", "beta", "svc", "cm", sameTime)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(configMap, alpha, beta).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	alphaAccepted := findCondition(r.computeConditions(context.Background(), alpha), string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, alphaAccepted)
	assert.Equal(t, metav1.ConditionTrue, alphaAccepted.Status,
		"lexicographically smaller name wins the tie")

	betaAccepted := findCondition(r.computeConditions(context.Background(), beta), string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, betaAccepted)
	assert.Equal(t, metav1.ConditionFalse, betaAccepted.Status)
	assert.Equal(t, string(gatewayv1.PolicyReasonConflicted), betaAccepted.Reason)
	assert.Contains(t, betaAccepted.Message, "ns/alpha")
}

// TestComputeConditions_CAInvalidPrecedenceOverConflicted pins the
// precedence between two failure modes: a policy with an invalid CA ref
// MUST surface Reason=NoValidCACertificate / InvalidCACertificateRef
// even if it would also lose a conflict against an older sibling. The
// CA-invalid error is more actionable for the operator ("fix your CA
// first") and a broken-CA policy cannot be Accepted=True regardless of
// peer state, so Conflicted is only emitted on policies that would
// otherwise be Accepted=True.
func TestComputeConditions_CAInvalidPrecedenceOverConflicted(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))

	// Winner has a valid CA. Loser points at a non-existent ConfigMap.
	winner := backendTLSPolicyFor("ns", "winner", "svc", "cm", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	loser := backendTLSPolicyFor("ns", "loser", "svc", "missing-cm", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(configMap, winner, loser).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	conds := r.computeConditions(context.Background(), loser)
	accepted := findCondition(conds, string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.BackendTLSPolicyReasonNoValidCACertificate), accepted.Reason,
		"CA invalidity MUST dominate over Conflicted — fix-your-CA is the actionable error")

	resolvedRefs := findCondition(conds, string(gatewayv1.BackendTLSPolicyConditionResolvedRefs))
	require.NotNil(t, resolvedRefs)
	assert.Equal(t, metav1.ConditionFalse, resolvedRefs.Status,
		"loser's CA refs do not resolve — ResolvedRefs MUST also be False, unlike the pure-Conflicted path")
}

// grpcRouteFor builds a GRPCRoute with one rule pointing at the given
// backend Service. parentGwName is recorded as the only parentRef in
// the same namespace.
func grpcRouteFor(namespace, name, parentGwName, backendServiceName string) *gatewayv1.GRPCRoute {
	return &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: gatewayv1.ObjectName(parentGwName)},
				},
			},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: gatewayv1.ObjectName(backendServiceName),
								},
							},
						},
					},
				},
			},
		},
	}
}

// TestReconcile_GRPCRouteAttachment_PopulatesAncestor pins parity with
// HTTPRoute on the status side: a BackendTLSPolicy whose target Service
// is referenced by a managed GRPCRoute MUST surface the parent Gateway
// in Status.Ancestors, just like the HTTPRoute path does. Without this,
// gatewaysForPolicy returns an empty set and the policy reconciles with
// Ancestors: [] — non-conformant per GEP-713, which requires the
// Ancestor to name each Gateway the policy affects.
func TestReconcile_GRPCRouteAttachment_PopulatesAncestor(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	const controllerName = "github.com/lexfrei/test"

	gatewayClass := gatewayClassFor("cf-class", controllerName)
	gateway := gatewayFor("ns", "gw", "cf-class")
	grpcRoute := grpcRouteFor("ns", "r", "gw", "svc")
	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))
	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, grpcRoute, configMap, policy).
		WithStatusSubresource(policy).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: controllerName}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "p"}})
	require.NoError(t, err)

	var refreshed gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "p"}, &refreshed))
	require.Len(t, refreshed.Status.Ancestors, 1,
		"policy targeting a Service referenced only by a GRPCRoute MUST still populate Status.Ancestors with the parent Gateway")
	require.NotNil(t, refreshed.Status.Ancestors[0].AncestorRef.Name)
	assert.Equal(t, "gw", string(refreshed.Status.Ancestors[0].AncestorRef.Name))
}

// TestPoliciesForGRPCRouteChange_OnlyMatchingPolicy mirrors the existing
// HTTPRoute variant for the GRPCRoute watch wiring. A change to a
// GRPCRoute referencing a policy's target Service MUST enqueue that
// policy and only that policy.
func TestPoliciesForGRPCRouteChange_OnlyMatchingPolicy(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	matching := backendTLSPolicyFor("ns", "matching", "svc", "cm", time.Time{})
	unrelated := backendTLSPolicyFor("ns", "unrelated", "other-svc", "cm", time.Time{})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(matching, unrelated).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	grpcRoute := grpcRouteFor("ns", "r", "gw", "svc")

	requests := r.policiesForGRPCRouteChange(context.Background(), grpcRoute)

	names := make([]string, 0, len(requests))
	for _, req := range requests {
		names = append(names, req.Name)
	}

	assert.ElementsMatch(t, []string{"matching"}, names,
		"GRPCRoute change MUST enqueue only the policy whose target Service the route references")
}

// TestCollectGatewayKeys_SkipsNonGatewayParents pins the filter that
// strips non-Gateway parentRefs (ListenerSet, future kinds) out of the
// BackendTLSPolicy Ancestor walk. Without the filter, the subsequent
// r.Get(&Gateway{}) would 404 on the ListenerSet name and silently drop
// the entry — masking the leak but leaving noisy reconciles.
func TestCollectGatewayKeys_SkipsNonGatewayParents(t *testing.T) {
	t.Parallel()

	listenerSetKind := gatewayv1.Kind("ListenerSet")
	gatewayKind := gatewayv1.Kind("Gateway")

	httpRoute := httpRouteFor("ns", "r", "gw", "svc")
	httpRoute.Spec.ParentRefs = []gatewayv1.ParentReference{
		{Name: gatewayv1.ObjectName("gw"), Kind: &gatewayKind},
		{Name: gatewayv1.ObjectName("listenerset"), Kind: &listenerSetKind},
	}

	keys := collectGatewayKeys(
		[]gatewayv1.HTTPRoute{*httpRoute},
		nil,
		map[string]struct{}{"svc": {}},
		"ns",
	)

	require.Len(t, keys, 1, "non-Gateway parents MUST be filtered out before the Gateway Get")
	assert.Equal(t, "gw", keys[0].Name)
	assert.Equal(t, "ns", keys[0].Namespace)
}

// TestPoliciesForPeerChange_DeletedWinnerEnqueuesLoser pins the deletion
// recovery path that the For(&BackendTLSPolicy{}) watch alone cannot
// cover: when the winning sibling is deleted, the loser keeps its
// Accepted=False, Reason=Conflicted Status forever unless something
// outside its own object change triggers a reconcile. A peer-change
// mapper takes the deleted winner, lists policies in the same namespace
// that share at least one (Service, SectionName) target, and enqueues
// them so they re-evaluate the conflict and flip back to Accepted=True.
func TestPoliciesForPeerChange_DeletedWinnerEnqueuesLoser(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	winner := backendTLSPolicyFor("ns", "winner", "svc", "cm", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	loser := backendTLSPolicyFor("ns", "loser", "svc", "cm", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	unrelated := backendTLSPolicyFor("ns", "other-target", "other-svc", "cm", time.Time{})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(loser, unrelated).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	requests := r.policiesForPeerChange(context.Background(), winner)

	names := make([]string, 0, len(requests))
	for _, req := range requests {
		names = append(names, req.Name)
	}

	assert.ElementsMatch(t, []string{"loser"}, names,
		"peer-change mapper MUST enqueue the loser policy on winner deletion — and only the loser, not the unrelated sibling on a different Service")
}

// TestReconcile_ConflictResolution_DeletedWinnerFlipsLoserToAccepted
// drives the full deletion-recovery flow against the live Reconcile
// pipeline. After both policies have stamped their status, the winner is
// deleted; the loser is then re-reconciled (mimicking the peer-change
// mapper firing) and must flip from Accepted=False/Conflicted back to
// Accepted=True with the ResolvedRefs=True it has always carried.
func TestReconcile_ConflictResolution_DeletedWinnerFlipsLoserToAccepted(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	const controllerName = "github.com/lexfrei/test"

	gatewayClass := gatewayClassFor("cf-class", controllerName)
	gateway := gatewayFor("ns", "gw", "cf-class")
	route := httpRouteFor("ns", "r", "gw", "svc")
	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))

	winner := backendTLSPolicyFor("ns", "winner", "svc", "cm", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	loser := backendTLSPolicyFor("ns", "loser", "svc", "cm", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route, configMap, winner, loser).
		WithStatusSubresource(winner, loser).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: controllerName}

	// First wave: both policies reconcile while both exist. Loser ends up Conflicted.
	for _, name := range []string{"winner", "loser"} {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: name}})
		require.NoError(t, err, "reconcile %s", name)
	}

	var loserBefore gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "loser"}, &loserBefore))
	require.Len(t, loserBefore.Status.Ancestors, 1)
	loserAcceptedBefore := findCondition(loserBefore.Status.Ancestors[0].Conditions, string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, loserAcceptedBefore)
	require.Equal(t, metav1.ConditionFalse, loserAcceptedBefore.Status, "precondition: loser starts Accepted=False")
	require.Equal(t, string(gatewayv1.PolicyReasonConflicted), loserAcceptedBefore.Reason)

	// Delete the winner. The peer-change mapper would normally re-enqueue
	// the loser here; in this test we drive the reconcile manually.
	require.NoError(t, fakeClient.Delete(context.Background(), winner))

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "loser"}})
	require.NoError(t, err)

	var loserAfter gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "loser"}, &loserAfter))
	require.Len(t, loserAfter.Status.Ancestors, 1)
	loserAcceptedAfter := findCondition(loserAfter.Status.Ancestors[0].Conditions, string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, loserAcceptedAfter)
	assert.Equal(t, metav1.ConditionTrue, loserAcceptedAfter.Status,
		"after winner deletion the loser MUST flip back to Accepted=True — staying Conflicted would mislead operators about which policy is enforcing TLS on the Service")
	assert.Equal(t, string(gatewayv1.PolicyReasonAccepted), loserAcceptedAfter.Reason)
}

// ---- selectPolicyForService / isPolicyOlder / policyTargetsService ----

// alwaysEmptyPortName is a resolvePortName stub for tests where SectionName
// is not expected to matter (no SectionName on the policies under test).
func alwaysEmptyPortName() string { return "" }

// TestReconcile_ConflictResolution_LoserStampedConflictedEndToEnd drives
// the full Reconcile pipeline against two BackendTLSPolicies that target
// the same Service. After reconciling each policy, the older one's
// Status.Ancestors carries Accepted=True and the newer one's carries
// Accepted=False, Reason=Conflicted with a Message naming the winner.
//
// This pins the GEP-713 conflict-resolution behavior end to end —
// computeConditions + updateStatus + the surrounding Reconcile setup —
// against the regression where updateStatus would be called with
// acceptedConditions on a loser.
func TestReconcile_ConflictResolution_LoserStampedConflictedEndToEnd(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	const controllerName = "github.com/lexfrei/test"

	gatewayClass := gatewayClassFor("cf-class", controllerName)
	gateway := gatewayFor("ns", "gw", "cf-class")
	route := httpRouteFor("ns", "r", "gw", "svc")
	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))

	winner := backendTLSPolicyFor("ns", "winner", "svc", "cm", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	loser := backendTLSPolicyFor("ns", "loser", "svc", "cm", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route, configMap, winner, loser).
		WithStatusSubresource(winner, loser).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: controllerName}

	for _, name := range []string{"winner", "loser"} {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: name}})
		require.NoError(t, err, "reconcile %s", name)
	}

	var refreshedWinner gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "winner"}, &refreshedWinner))
	require.Len(t, refreshedWinner.Status.Ancestors, 1)
	winnerAccepted := findCondition(refreshedWinner.Status.Ancestors[0].Conditions, string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, winnerAccepted)
	assert.Equal(t, metav1.ConditionTrue, winnerAccepted.Status, "older policy MUST stay Accepted=True")

	var refreshedLoser gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "loser"}, &refreshedLoser))
	require.Len(t, refreshedLoser.Status.Ancestors, 1)
	loserAccepted := findCondition(refreshedLoser.Status.Ancestors[0].Conditions, string(gatewayv1.PolicyConditionAccepted))
	require.NotNil(t, loserAccepted)
	assert.Equal(t, metav1.ConditionFalse, loserAccepted.Status, "newer policy MUST be Accepted=False")
	assert.Equal(t, string(gatewayv1.PolicyReasonConflicted), loserAccepted.Reason)
	assert.Contains(t, loserAccepted.Message, "ns/winner")
}

func TestSelectPolicyForServicePort_OlderWins(t *testing.T) {
	t.Parallel()

	older := *backendTLSPolicyFor("ns", "policy-z", "svc", "cm", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	newer := *backendTLSPolicyFor("ns", "policy-a", "svc", "cm", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	scheme := newBackendTLSPolicyScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	winner := selectPolicyForServicePort(context.Background(), fakeClient,
		[]gatewayv1.BackendTLSPolicy{newer, older}, "ns", "svc", 443)
	require.NotNil(t, winner)
	assert.Equal(t, "policy-z", winner.Name)
}

func TestSelectPolicyForServicePort_TieBreaksAlphabetically(t *testing.T) {
	t.Parallel()

	sameTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	policyA := *backendTLSPolicyFor("ns", "alpha", "svc", "cm", sameTime)
	policyB := *backendTLSPolicyFor("ns", "beta", "svc", "cm", sameTime)

	scheme := newBackendTLSPolicyScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	winner := selectPolicyForServicePort(context.Background(), fakeClient,
		[]gatewayv1.BackendTLSPolicy{policyB, policyA}, "ns", "svc", 443)
	require.NotNil(t, winner)
	assert.Equal(t, "alpha", winner.Name)
}

func TestSelectPolicyForServicePort_NoMatchReturnsNil(t *testing.T) {
	t.Parallel()

	policy := *backendTLSPolicyFor("ns", "p", "other-svc", "cm", time.Time{})

	scheme := newBackendTLSPolicyScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	winner := selectPolicyForServicePort(context.Background(), fakeClient,
		[]gatewayv1.BackendTLSPolicy{policy}, "ns", "svc", 443)
	assert.Nil(t, winner)
}

func TestSelectPolicyForServicePort_SectionNameMatchesNamedPort(t *testing.T) {
	t.Parallel()

	policy := *backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	sectionName := gatewayv1.SectionName("https")
	policy.Spec.TargetRefs[0].SectionName = &sectionName

	scheme := newBackendTLSPolicyScheme(t)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "svc"},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80},
				{Name: "https", Port: 8443},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()

	// Matching named port → policy applies.
	winner := selectPolicyForServicePort(context.Background(), fakeClient,
		[]gatewayv1.BackendTLSPolicy{policy}, "ns", "svc", 8443)
	require.NotNil(t, winner, "SectionName 'https' matches port 8443 (named 'https') → policy applies")

	// Different port on the same Service → policy must NOT apply.
	winner = selectPolicyForServicePort(context.Background(), fakeClient,
		[]gatewayv1.BackendTLSPolicy{policy}, "ns", "svc", 80)
	assert.Nil(t, winner, "SectionName 'https' must NOT match port 80 (named 'http') — multi-port spec invariant")
}

// TestSelectPolicyForServicePort_ScopedAndUnscopedDoBothApplyAtRuntime
// pins the runtime-side half of the documented status-vs-runtime
// SectionName divergence. The status mapper in normalizePolicyTargets
// treats scoped (SectionName="https") and unscoped policies on the same
// Service as different scopes per GEP-713, so both get
// Accepted=True. The runtime selector here walks ALL policies targeting
// the Service and picks the older one regardless of scope.
//
// Concrete consequence pinned by this test: when both a scoped
// SectionName="https" policy and an unscoped policy target the same
// Service with a port named "https", the older policy wins for that
// port at runtime — even if the status mapper considers them
// non-overlapping. A future refactor that aligns the two layers (either
// by tightening the runtime selector to prefer scoped on port-name
// match, or by widening the status mapper to treat the overlap as
// conflict) MUST update both call sites at once; this test is the
// runtime anchor.
func TestSelectPolicyForServicePort_ScopedAndUnscopedDoBothApplyAtRuntime(t *testing.T) {
	t.Parallel()

	scopedSectionName := gatewayv1.SectionName("https")

	scoped := *backendTLSPolicyFor("ns", "scoped", "svc", "cm", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	scoped.Spec.TargetRefs[0].SectionName = &scopedSectionName

	unscoped := *backendTLSPolicyFor("ns", "unscoped", "svc", "cm", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	scheme := newBackendTLSPolicyScheme(t)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "svc"},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80},
				{Name: "https", Port: 8443},
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()

	winner := selectPolicyForServicePort(context.Background(), fakeClient,
		[]gatewayv1.BackendTLSPolicy{scoped, unscoped}, "ns", "svc", 8443)
	require.NotNil(t, winner, "at runtime, port 8443 (named 'https') overlaps BOTH the scoped and the unscoped policy")
	assert.Equal(t, "scoped", winner.Name,
		"older scoped policy wins on the matching named port even though the status mapper considers it a different scope from the unscoped peer")
}

func TestPolicyTargetsServicePort_KindAliases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind     string
		expected bool
	}{
		{"Service", true},
		{"", true},
		{"Pod", false},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()

			policy := &gatewayv1.BackendTLSPolicy{
				Spec: gatewayv1.BackendTLSPolicySpec{
					TargetRefs: []gatewayv1.LocalPolicyTargetReferenceWithSectionName{
						{
							LocalPolicyTargetReference: gatewayv1.LocalPolicyTargetReference{
								Kind: gatewayv1.Kind(tc.kind),
								Name: "svc",
							},
						},
					},
				},
			}

			assert.Equal(t, tc.expected, policyTargetsServicePort(policy, "svc", alwaysEmptyPortName))
		})
	}
}

// ---- routeReferencesAnyService ----

func TestRouteReferencesAnyService_MatchesServiceBackend(t *testing.T) {
	t.Parallel()

	route := httpRouteFor("ns", "r", "gw", "target")
	targets := map[string]struct{}{"target": {}}

	assert.True(t, routeReferencesAnyService(route, targets, "ns"))
}

func TestRouteReferencesAnyService_IgnoresNonServiceKind(t *testing.T) {
	t.Parallel()

	route := httpRouteFor("ns", "r", "gw", "target")
	// Override the kind to a non-Service value.
	nonService := gatewayv1.Kind("Pod")
	route.Spec.Rules[0].BackendRefs[0].Kind = &nonService

	targets := map[string]struct{}{"target": {}}
	assert.False(t, routeReferencesAnyService(route, targets, "ns"))
}

func TestRouteReferencesAnyService_EmptyTargets(t *testing.T) {
	t.Parallel()

	route := httpRouteFor("ns", "r", "gw", "target")
	assert.False(t, routeReferencesAnyService(route, map[string]struct{}{}, "ns"))
}

// TestRouteReferencesAnyService_CrossNamespaceBackendDoesNotMatch pins the
// namespace-aware contract: a route whose backendRef points at a Service
// in another namespace MUST NOT be considered "covered" by a policy
// targeting a same-named Service in the policy's own namespace. Without
// this check the policy's Status.Ancestors would falsely list the route's
// parent Gateway, advertising enforcement of a Service the route does
// not actually use.
func TestRouteReferencesAnyService_CrossNamespaceBackendDoesNotMatch(t *testing.T) {
	t.Parallel()

	route := httpRouteFor("route-ns", "r", "gw", "svc")
	otherNS := gatewayv1.Namespace("other-ns")
	route.Spec.Rules[0].BackendRefs[0].Namespace = &otherNS

	// Policy targets "svc" in policy-ns. Route's backend points to other-ns/svc.
	assert.False(t, routeReferencesAnyService(route, map[string]struct{}{"svc": {}}, "policy-ns"),
		"backendRef in other-ns MUST NOT match a policy targeting svc in policy-ns")
}

// TestGRPCRouteReferencesAnyService_CrossNamespaceBackendDoesNotMatch is
// the GRPCRoute counterpart of the namespace-deny pin above.
func TestGRPCRouteReferencesAnyService_CrossNamespaceBackendDoesNotMatch(t *testing.T) {
	t.Parallel()

	route := grpcRouteFor("route-ns", "r", "gw", "svc")
	otherNS := gatewayv1.Namespace("other-ns")
	route.Spec.Rules[0].BackendRefs[0].Namespace = &otherNS

	assert.False(t, grpcRouteReferencesAnyService(route, map[string]struct{}{"svc": {}}, "policy-ns"),
		"GRPC backendRef in other-ns MUST NOT match a policy targeting svc in policy-ns")
}

// ---- policyReferencesConfigMap / isConfigMapReferencedByBackendTLSPolicy ----

func TestPolicyReferencesConfigMap_MatchesByName(t *testing.T) {
	t.Parallel()

	policy := backendTLSPolicyFor("ns", "p", "svc", "ca-cm", time.Time{})
	assert.True(t, policyReferencesConfigMap(policy, "ca-cm"))
	assert.False(t, policyReferencesConfigMap(policy, "other-cm"))
}

func TestPolicyReferencesConfigMap_IgnoresNonConfigMapKind(t *testing.T) {
	t.Parallel()

	policy := backendTLSPolicyFor("ns", "p", "svc", "ca-cm", time.Time{})
	policy.Spec.Validation.CACertificateRefs[0].Kind = "Secret"
	assert.False(t, policyReferencesConfigMap(policy, "ca-cm"))
}

func TestIsConfigMapReferencedByBackendTLSPolicy(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	policy := backendTLSPolicyFor("ns", "p", "svc", "ca-cm", time.Time{})
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		Build()

	matching := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ca-cm"}}
	other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "unrelated"}}

	assert.True(t, isConfigMapReferencedByBackendTLSPolicy(context.Background(), fakeClient, matching))
	assert.False(t, isConfigMapReferencedByBackendTLSPolicy(context.Background(), fakeClient, other))
}

// ---- collectGatewayKeys ----

func TestCollectGatewayKeys_DeterministicSort(t *testing.T) {
	t.Parallel()

	// Build several HTTPRoutes referencing the same Service, fanning out to
	// Gateways named in non-alphabetical order, to confirm the result is
	// alphabetised by {namespace, name}.
	routes := []gatewayv1.HTTPRoute{
		*httpRouteFor("ns", "r-zeta", "gw-z", "svc"),
		*httpRouteFor("ns", "r-alpha", "gw-a", "svc"),
		*httpRouteFor("ns", "r-mu", "gw-m", "svc"),
	}

	keys := collectGatewayKeys(routes, nil, map[string]struct{}{"svc": {}}, "ns")

	require.Len(t, keys, 3)
	assert.Equal(t, "gw-a", keys[0].Name)
	assert.Equal(t, "gw-m", keys[1].Name)
	assert.Equal(t, "gw-z", keys[2].Name)
}

func TestCollectGatewayKeys_SkipsUnrelatedRoutes(t *testing.T) {
	t.Parallel()

	routes := []gatewayv1.HTTPRoute{
		*httpRouteFor("ns", "r-other", "gw", "different-svc"),
	}
	keys := collectGatewayKeys(routes, nil, map[string]struct{}{"svc": {}}, "ns")

	assert.Empty(t, keys)
}

// ---- updateStatus ----

func TestUpdateStatus_PreservesOtherControllers(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	other := gatewayv1.PolicyAncestorStatus{
		AncestorRef:    gatewayv1.ParentReference{Name: "other-gw"},
		ControllerName: gatewayv1.GatewayController("other-controller"),
		Conditions: []metav1.Condition{
			{Type: string(gatewayv1.PolicyConditionAccepted), Status: metav1.ConditionTrue},
		},
	}

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	policy.Status.Ancestors = []gatewayv1.PolicyAncestorStatus{other}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test-controller"}

	gateway := *gatewayFor("ns", "our-gw", "cf-class")
	conditions := acceptedConditions(policy.Generation)

	err := r.updateStatus(context.Background(), client.ObjectKey{Namespace: "ns", Name: "p"}, []gatewayv1.Gateway{gateway}, conditions, policy.Generation)
	require.NoError(t, err)

	var refreshed gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "p"}, &refreshed))
	require.Len(t, refreshed.Status.Ancestors, 2)
	// Other controller's entry preserved.
	assert.Equal(t, gatewayv1.GatewayController("other-controller"), refreshed.Status.Ancestors[0].ControllerName)
	assert.Equal(t, gatewayv1.GatewayController("test-controller"), refreshed.Status.Ancestors[1].ControllerName)
	assert.Equal(t, gatewayv1.ObjectName("our-gw"), refreshed.Status.Ancestors[1].AncestorRef.Name)
}

// TestUpdateStatus_SkipsObservedGenerationRegression pins the Gateway API rule
// that a reconcile MUST NOT overwrite an ancestor status condition already
// stamped with an observedGeneration newer than the generation this reconcile
// observed.
func TestUpdateStatus_SkipsObservedGenerationRegression(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	const newerGen = 5

	// Our own ancestor entry, already stamped with a generation newer than the
	// one this reconcile observed.
	ours := gatewayv1.PolicyAncestorStatus{
		AncestorRef:    gatewayv1.ParentReference{Name: "our-gw"},
		ControllerName: gatewayv1.GatewayController("test-controller"),
		Conditions: []metav1.Condition{
			{
				Type:               string(gatewayv1.PolicyConditionAccepted),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: newerGen,
				Reason:             "Accepted",
				Message:            "written by a newer reconcile",
			},
		},
	}

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	policy.Status.Ancestors = []gatewayv1.PolicyAncestorStatus{ours}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test-controller"}

	gateway := *gatewayFor("ns", "our-gw", "cf-class")

	// reconciledGen 3 < stored 5 → the write is skipped.
	require.NoError(t, r.updateStatus(context.Background(),
		client.ObjectKey{Namespace: "ns", Name: "p"},
		[]gatewayv1.Gateway{gateway},
		acceptedConditions(3),
		3,
	))

	var refreshed gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "p"}, &refreshed))
	require.Len(t, refreshed.Status.Ancestors, 1)
	require.Len(t, refreshed.Status.Ancestors[0].Conditions, 1)
	assert.Equal(t, int64(newerGen), refreshed.Status.Ancestors[0].Conditions[0].ObservedGeneration,
		"stale reconcile must not overwrite a newer ancestor status")
	assert.Equal(t, "written by a newer reconcile", refreshed.Status.Ancestors[0].Conditions[0].Message)
}

func TestUpdateStatus_PreservesOtherControllersUnderCap(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	// Seed three other-controller ancestor claims so the policy starts with a
	// non-trivial foreign footprint. With 20 incoming OUR-managed Gateways
	// the truncation MUST drop OUR entries first, never the foreigners.
	otherClaims := []gatewayv1.PolicyAncestorStatus{
		{
			AncestorRef:    gatewayv1.ParentReference{Name: "foreign-1"},
			ControllerName: gatewayv1.GatewayController("foreign-controller-1"),
			Conditions:     []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue}},
		},
		{
			AncestorRef:    gatewayv1.ParentReference{Name: "foreign-2"},
			ControllerName: gatewayv1.GatewayController("foreign-controller-2"),
			Conditions:     []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue}},
		},
		{
			AncestorRef:    gatewayv1.ParentReference{Name: "foreign-3"},
			ControllerName: gatewayv1.GatewayController("foreign-controller-3"),
			Conditions:     []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue}},
		},
	}

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	policy.Status.Ancestors = otherClaims

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test-controller"}

	gateways := make([]gatewayv1.Gateway, 0, 20)
	for i := range 20 {
		gateways = append(gateways, *gatewayFor("ns", "gw-"+zeroPadded(i), "cf-class"))
	}

	require.NoError(t, r.updateStatus(context.Background(),
		client.ObjectKey{Namespace: "ns", Name: "p"},
		gateways,
		acceptedConditions(policy.Generation),
		policy.Generation,
	))

	var refreshed gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "ns", Name: "p"}, &refreshed))
	assert.Len(t, refreshed.Status.Ancestors, policyAncestorStatusMaxCount,
		"combined set capped at 16")

	// Every foreign claim must survive the truncation — count them up.
	foreignSurvivors := 0

	for _, ancestor := range refreshed.Status.Ancestors {
		if string(ancestor.ControllerName) != r.ControllerName {
			foreignSurvivors++
		}
	}

	assert.Equal(t, len(otherClaims), foreignSurvivors,
		"other controllers' ancestor entries MUST NOT be dropped by our truncation; "+
			"only OUR entries shrink to fit within the cap")
}

func TestUpdateStatus_CapsAncestorsAt16(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test-controller"}

	// 20 ancestor gateways → updateStatus must truncate to 16.
	gateways := make([]gatewayv1.Gateway, 0, 20)
	for i := range 20 {
		// Use zero-padded names so alphabetical truncation is well-defined.
		name := "gw-" + zeroPadded(i)
		gateways = append(gateways, *gatewayFor("ns", name, "cf-class"))
	}

	conditions := acceptedConditions(policy.Generation)

	err := r.updateStatus(context.Background(), client.ObjectKey{Namespace: "ns", Name: "p"}, gateways, conditions, policy.Generation)
	require.NoError(t, err)

	var refreshed gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "p"}, &refreshed))
	assert.Len(t, refreshed.Status.Ancestors, policyAncestorStatusMaxCount)
}

func zeroPadded(idx int) string {
	letters := []byte{byte('0' + idx/10), byte('0' + idx%10)}

	return string(letters)
}

func TestUpdateStatus_SetsLastTransitionTime(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test-controller"}

	gateway := *gatewayFor("ns", "gw", "cf-class")
	conditions := acceptedConditions(policy.Generation)

	require.NoError(t, r.updateStatus(context.Background(), client.ObjectKey{Namespace: "ns", Name: "p"}, []gatewayv1.Gateway{gateway}, conditions, policy.Generation))

	var refreshed gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "p"}, &refreshed))
	require.Len(t, refreshed.Status.Ancestors, 1)
	require.NotEmpty(t, refreshed.Status.Ancestors[0].Conditions)
	for _, condition := range refreshed.Status.Ancestors[0].Conditions {
		assert.False(t, condition.LastTransitionTime.IsZero(),
			"LastTransitionTime must be populated by meta.SetStatusCondition")
	}
}

// ---- policiesForRouteChange ----

func TestPoliciesForRouteChange_OnlyMatchingPolicy(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	matching := backendTLSPolicyFor("ns", "match", "svc", "cm", time.Time{})
	unrelated := backendTLSPolicyFor("ns", "unrelated", "other-svc", "cm", time.Time{})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(matching, unrelated).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	route := httpRouteFor("ns", "r", "gw", "svc")
	requests := r.policiesForRouteChange(context.Background(), route)

	require.Len(t, requests, 1)
	assert.Equal(t, "match", requests[0].Name)
}

// ---- policiesForConfigMapChange ----

func TestPoliciesForConfigMapChange_OnlyReferencingPolicies(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	referencing := backendTLSPolicyFor("ns", "ref", "svc", "ca-cm", time.Time{})
	other := backendTLSPolicyFor("ns", "other", "svc-2", "different-cm", time.Time{})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(referencing, other).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "ca-cm"}}
	requests := r.policiesForConfigMapChange(context.Background(), configMap)

	require.Len(t, requests, 1)
	assert.Equal(t, "ref", requests[0].Name)
}

// ---- newBackendTLSResolver: fail-closed semantics ----

func TestBackendTLSResolver_NoPolicyReturnsNil(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	resolver := newBackendTLSResolver(fakeClient)

	got := resolver(context.Background(), "ns", "svc", 443)
	assert.Nil(t, got, "no matching policy → nil so the proxy uses plaintext")
}

func TestBackendTLSResolver_PolicyTargetsButCAMissing_ReturnsPoisonedConfig(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	policy := backendTLSPolicyFor("ns", "p", "svc", "missing-cm", time.Time{})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		Build()

	resolver := newBackendTLSResolver(fakeClient)

	got := resolver(context.Background(), "ns", "svc", 443)
	require.NotNil(t, got,
		"policy targets the Service but CA cannot be resolved — resolver MUST NOT return nil "+
			"(that would downgrade to plaintext). Return a poisoned config so the handshake fails.")
	assert.Empty(t, got.CABundlePEM,
		"poisoned config carries an empty CA bundle so x509 verification fails closed")
}

func TestBackendTLSResolver_PolicyTargetsButCAMalformed_ReturnsPoisonedConfig(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	configMap := caConfigMap("ns", "cm", "not actual pem")

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, configMap).
		Build()

	resolver := newBackendTLSResolver(fakeClient)

	got := resolver(context.Background(), "ns", "svc", 443)
	require.NotNil(t, got, "policy targets the Service — resolver must NOT return nil for malformed CA")
	assert.Empty(t, got.CABundlePEM)
}

// TestBackendTLSResolver_URISubjectAltName_ForwardsURIToProxy verifies that
// URI-type SubjectAltNames are forwarded to the proxy BackendTLSConfig as
// plain strings on the SubjectAltNameURIs field. Hostname-type SANs go to
// SubjectAltNames. Both lists are OR-matched by the proxy at handshake time.
func TestBackendTLSResolver_URISubjectAltName_ForwardsURIToProxy(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	policy.Spec.Validation.SubjectAltNames = []gatewayv1.SubjectAltName{
		{Type: gatewayv1.URISubjectAltNameType, URI: "spiffe://abc.example.com/identity"},
		{Type: gatewayv1.HostnameSubjectAltNameType, Hostname: "alt.example.com"},
	}

	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, configMap).
		Build()

	resolver := newBackendTLSResolver(fakeClient)

	got := resolver(context.Background(), "ns", "svc", 443)
	require.NotNil(t, got, "happy-path policy must produce a real TLS config")
	assert.NotEmpty(t, got.CABundlePEM, "valid CA bundle must be forwarded — not a poisoned config")
	assert.Equal(t, []string{"alt.example.com"}, got.SubjectAltNames,
		"Hostname SANs go to SubjectAltNames")
	assert.Equal(t, []string{"spiffe://abc.example.com/identity"}, got.SubjectAltNameURIs,
		"URI SANs go to SubjectAltNameURIs")
}

// TestBackendTLSResolver_ListErrorFailsOpen pins the documented asymmetry
// between cache errors (fail OPEN) and per-policy validation errors (fail
// CLOSED). When `client.List` itself errors before the policy list can be
// inspected, the resolver returns nil — the proxy dials plaintext for THIS
// request rather than poisoning every route in the namespace. The decision
// is documented in newBackendTLSResolver's godoc; this test pins it.
func TestBackendTLSResolver_ListErrorFailsOpen(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				if _, ok := list.(*gatewayv1.BackendTLSPolicyList); ok {
					return errSimulatedCacheMiss
				}

				return nil
			},
		}).
		Build()

	resolver := newBackendTLSResolver(fakeClient)

	got := resolver(context.Background(), "ns", "svc", 443)
	assert.Nil(t, got,
		"List error → fail OPEN (nil → proxy dials plaintext). "+
			"Poisoning every namespace on a transient cache miss would be a worse failure mode; "+
			"the asymmetry is documented in newBackendTLSResolver's godoc and surfaced via a WARN log.")
}

func TestBackendTLSResolver_MultipleCARefs_Concatenates(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	caOne := generateSelfSignedCAPEM(t)
	caTwo := generateSelfSignedCAPEM(t)

	cmOne := caConfigMap("ns", "ca-one", caOne)
	cmTwo := caConfigMap("ns", "ca-two", caTwo)

	policy := backendTLSPolicyFor("ns", "p", "svc", "ca-one", time.Time{})
	policy.Spec.Validation.CACertificateRefs = append(policy.Spec.Validation.CACertificateRefs,
		gatewayv1.LocalObjectReference{
			Kind: configMapKind,
			Name: "ca-two",
		},
	)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, cmOne, cmTwo).
		Build()

	resolver := newBackendTLSResolver(fakeClient)

	got := resolver(context.Background(), "ns", "svc", 443)
	require.NotNil(t, got, "two valid CACertificateRefs must produce a real (non-poisoned) config")
	assert.Contains(t, got.CABundlePEM, caOne,
		"first CA bundle must appear in the concatenated trust pool")
	assert.Contains(t, got.CABundlePEM, caTwo,
		"second CA bundle must appear in the concatenated trust pool — multiple refs cannot silently 'first-wins'")
}

func TestBackendTLSResolver_HappyPath_ReturnsRealConfig(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	policy.Spec.Validation.SubjectAltNames = []gatewayv1.SubjectAltName{
		{Type: gatewayv1.HostnameSubjectAltNameType, Hostname: "alt.example.com"},
	}
	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy, configMap).
		Build()

	resolver := newBackendTLSResolver(fakeClient)

	got := resolver(context.Background(), "ns", "svc", 443)
	require.NotNil(t, got)
	assert.NotEmpty(t, got.CABundlePEM, "valid policy + CA → real CA bundle")
	assert.Equal(t, "test.example.com", got.ServerName)
	assert.Equal(t, []string{"alt.example.com"}, got.SubjectAltNames)
}

// ---- ObservedGeneration ----

func TestUpdateStatus_ObservedGenerationMatchesPolicyGeneration(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
	policy.Generation = 42

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test-controller"}

	gateway := *gatewayFor("ns", "gw", "cf-class")
	conditions := acceptedConditions(policy.Generation)

	require.NoError(t, r.updateStatus(context.Background(),
		client.ObjectKey{Namespace: "ns", Name: "p"},
		[]gatewayv1.Gateway{gateway},
		conditions,
		policy.Generation,
	))

	var refreshed gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Namespace: "ns", Name: "p"}, &refreshed))
	require.Len(t, refreshed.Status.Ancestors, 1)
	require.NotEmpty(t, refreshed.Status.Ancestors[0].Conditions)

	for _, condition := range refreshed.Status.Ancestors[0].Conditions {
		assert.Equal(t, int64(42), condition.ObservedGeneration,
			"every condition must carry the current policy.Generation; "+
				"the upstream BackendTLSPolicyObservedGenerationBump conformance test pins this")
	}
}

// ---- Reconcile (integration of the pieces above) ----

func TestReconcile_NotFound(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "missing", Namespace: "ns"}})
	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestReconcile_NoMatchingGateway_IsNoop(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	// Policy targets a Service that no route under our managed Gateway references.
	policy := backendTLSPolicyFor("ns", "p", "lonely-svc", "cm", time.Time{})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: "test"}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "p"}})
	require.NoError(t, err)

	var refreshed gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "p"}, &refreshed))
	assert.Empty(t, refreshed.Status.Ancestors)
}

func TestReconcile_HappyPath_StampsAcceptedAndResolvedRefs(t *testing.T) {
	t.Parallel()

	scheme := newBackendTLSPolicyScheme(t)

	const controllerName = "github.com/lexfrei/test"

	gatewayClass := gatewayClassFor("cf-class", controllerName)
	gateway := gatewayFor("ns", "gw", "cf-class")
	route := httpRouteFor("ns", "r", "gw", "svc")
	configMap := caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))
	policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route, configMap, policy).
		WithStatusSubresource(policy).
		Build()
	r := &BackendTLSPolicyReconciler{Client: fakeClient, Scheme: scheme, ControllerName: controllerName}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "p"}})
	require.NoError(t, err)

	var refreshed gatewayv1.BackendTLSPolicy
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "p"}, &refreshed))
	require.Len(t, refreshed.Status.Ancestors, 1)
	require.Len(t, refreshed.Status.Ancestors[0].Conditions, 2)

	for _, condition := range refreshed.Status.Ancestors[0].Conditions {
		assert.Equal(t, metav1.ConditionTrue, condition.Status)
		assert.Equal(t, refreshed.Generation, condition.ObservedGeneration)
	}
}
