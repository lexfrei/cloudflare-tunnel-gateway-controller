package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestBackendTLSResolver_PoisonedConfigSaysWhy pins that every poisoned
// config names the policy and the cause, which the converter turns into the
// route's ResolvedRefs=False message.
func TestBackendTLSResolver_PoisonedConfigSaysWhy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		objects func(t *testing.T) []client.Object
		cause   string
	}{
		{
			name: "CA ConfigMap missing",
			objects: func(*testing.T) []client.Object {
				return []client.Object{backendTLSPolicyFor("ns", "p", "svc", "missing-cm", time.Time{})}
			},
			cause: "CA certificate",
		},
		{
			name: "CA malformed",
			objects: func(*testing.T) []client.Object {
				return []client.Object{
					backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{}),
					caConfigMap("ns", "cm", "not actual pem"),
				}
			},
			cause: "CA certificate",
		},
		{
			name: "unsupported SAN type",
			objects: func(t *testing.T) []client.Object {
				t.Helper()

				policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
				policy.Spec.Validation.SubjectAltNames = []gatewayv1.SubjectAltName{{Type: "Email"}}

				return []client.Object{policy, caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))}
			},
			cause: "SubjectAltName",
		},
		{
			name: "only wellKnownCACertificates",
			objects: func(*testing.T) []client.Object {
				policy := backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{})
				policy.Spec.Validation.CACertificateRefs = nil
				policy.Spec.Validation.WellKnownCACertificates = new(gatewayv1.WellKnownCACertificatesSystem)

				return []client.Object{policy}
			},
			cause: "wellKnownCACertificates",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fakeClient := fake.NewClientBuilder().
				WithScheme(newBackendTLSPolicyScheme(t)).
				WithObjects(tt.objects(t)...).
				Build()

			got, err := newBackendTLSResolver(fakeClient)(context.Background(), "ns", "svc", 443, true)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Empty(t, got.CABundlePEM)
			assert.Contains(t, got.Unenforceable, "ns/p")
			assert.Contains(t, got.Unenforceable, `"svc"`)
			assert.Contains(t, got.Unenforceable, tt.cause)
		})
	}
}

// TestBackendTLSResolver_ValidPolicyIsEnforceable pins the other half: a
// policy the proxy can meet carries no Unenforceable message, so the route
// stays ResolvedRefs=True.
func TestBackendTLSResolver_ValidPolicyIsEnforceable(t *testing.T) {
	t.Parallel()

	fakeClient := fake.NewClientBuilder().
		WithScheme(newBackendTLSPolicyScheme(t)).
		WithObjects(backendTLSPolicyFor("ns", "p", "svc", "cm", time.Time{}),
			caConfigMap("ns", "cm", generateSelfSignedCAPEM(t))).
		Build()

	got, err := newBackendTLSResolver(fakeClient)(context.Background(), "ns", "svc", 443, true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotEmpty(t, got.CABundlePEM)
	assert.Empty(t, got.Unenforceable)
}
