package referencegrant_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
)

const (
	coreGroup = ""
)

func TestValidator_IsReferenceAllowed_SameNamespace(t *testing.T) {
	t.Parallel()

	scheme := setupScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	validator := referencegrant.NewValidator(fakeClient)

	from := referencegrant.Reference{
		Group:     gatewayv1.GroupName,
		Kind:      "HTTPRoute",
		Namespace: "default",
		Name:      "test-route",
	}

	to := referencegrant.Reference{
		Group:     coreGroup,
		Kind:      "Service",
		Namespace: "default",
		Name:      "test-service",
	}

	ctx := context.Background()
	allowed, err := validator.IsReferenceAllowed(ctx, from, to)

	require.NoError(t, err)
	assert.True(t, allowed, "same namespace references should always be allowed")
}

func TestValidator_IsReferenceAllowed_CrossNamespaceWithoutGrant(t *testing.T) {
	t.Parallel()

	scheme := setupScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	validator := referencegrant.NewValidator(fakeClient)

	from := referencegrant.Reference{
		Group:     gatewayv1.GroupName,
		Kind:      "HTTPRoute",
		Namespace: "default",
		Name:      "test-route",
	}

	to := referencegrant.Reference{
		Group:     coreGroup,
		Kind:      "Service",
		Namespace: "production",
		Name:      "api-service",
	}

	ctx := context.Background()
	allowed, err := validator.IsReferenceAllowed(ctx, from, to)

	require.NoError(t, err)
	assert.False(t, allowed, "cross-namespace reference without ReferenceGrant should be denied")
}

func TestValidator_IsReferenceAllowed_CrossNamespaceWithGrant(t *testing.T) {
	t.Parallel()

	scheme := setupScheme(t)

	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-default-to-services",
			Namespace: "production",
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1.GroupName,
					Kind:      "HTTPRoute",
					Namespace: "default",
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: coreGroup,
					Kind:  "Service",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(grant).
		Build()

	validator := referencegrant.NewValidator(fakeClient)

	from := referencegrant.Reference{
		Group:     gatewayv1.GroupName,
		Kind:      "HTTPRoute",
		Namespace: "default",
		Name:      "test-route",
	}

	to := referencegrant.Reference{
		Group:     coreGroup,
		Kind:      "Service",
		Namespace: "production",
		Name:      "api-service",
	}

	ctx := context.Background()
	allowed, err := validator.IsReferenceAllowed(ctx, from, to)

	require.NoError(t, err)
	assert.True(t, allowed, "cross-namespace reference with matching ReferenceGrant should be allowed")
}

func TestValidator_IsReferenceAllowed_GrantWithSpecificName(t *testing.T) {
	t.Parallel()

	scheme := setupScheme(t)

	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-specific-service",
			Namespace: "production",
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1.GroupName,
					Kind:      "HTTPRoute",
					Namespace: "default",
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: coreGroup,
					Kind:  "Service",
					Name:  objectNamePtr("allowed-service"),
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(grant).
		Build()

	validator := referencegrant.NewValidator(fakeClient)

	tests := []struct {
		name          string
		targetService string
		shouldAllow   bool
	}{
		{
			name:          "allowed service by name",
			targetService: "allowed-service",
			shouldAllow:   true,
		},
		{
			name:          "different service name denied",
			targetService: "other-service",
			shouldAllow:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			from := referencegrant.Reference{
				Group:     gatewayv1.GroupName,
				Kind:      "HTTPRoute",
				Namespace: "default",
				Name:      "test-route",
			}

			to := referencegrant.Reference{
				Group:     coreGroup,
				Kind:      "Service",
				Namespace: "production",
				Name:      tt.targetService,
			}

			ctx := context.Background()
			allowed, err := validator.IsReferenceAllowed(ctx, from, to)

			require.NoError(t, err)
			assert.Equal(t, tt.shouldAllow, allowed)
		})
	}
}

func TestValidator_IsReferenceAllowed_GRPCRoute(t *testing.T) {
	t.Parallel()

	scheme := setupScheme(t)

	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-grpc-routes",
			Namespace: "production",
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1.GroupName,
					Kind:      "GRPCRoute",
					Namespace: "default",
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: coreGroup,
					Kind:  "Service",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(grant).
		Build()

	validator := referencegrant.NewValidator(fakeClient)

	from := referencegrant.Reference{
		Group:     gatewayv1.GroupName,
		Kind:      "GRPCRoute",
		Namespace: "default",
		Name:      "grpc-route",
	}

	to := referencegrant.Reference{
		Group:     coreGroup,
		Kind:      "Service",
		Namespace: "production",
		Name:      "grpc-service",
	}

	ctx := context.Background()
	allowed, err := validator.IsReferenceAllowed(ctx, from, to)

	require.NoError(t, err)
	assert.True(t, allowed, "GRPCRoute should be allowed with matching ReferenceGrant")
}

func TestValidator_IsReferenceAllowed_MultipleGrants(t *testing.T) {
	t.Parallel()

	scheme := setupScheme(t)

	grant1 := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-http-routes",
			Namespace: "production",
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1.GroupName,
					Kind:      "HTTPRoute",
					Namespace: "default",
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: coreGroup,
					Kind:  "Service",
				},
			},
		},
	}

	grant2 := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-grpc-routes",
			Namespace: "production",
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1.GroupName,
					Kind:      "GRPCRoute",
					Namespace: "staging",
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: coreGroup,
					Kind:  "Service",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(grant1, grant2).
		Build()

	validator := referencegrant.NewValidator(fakeClient)

	tests := []struct {
		name        string
		from        referencegrant.Reference
		to          referencegrant.Reference
		shouldAllow bool
	}{
		{
			name: "HTTPRoute from default allowed",
			from: referencegrant.Reference{
				Group:     gatewayv1.GroupName,
				Kind:      "HTTPRoute",
				Namespace: "default",
			},
			to: referencegrant.Reference{
				Group:     coreGroup,
				Kind:      "Service",
				Namespace: "production",
				Name:      "api-service",
			},
			shouldAllow: true,
		},
		{
			name: "GRPCRoute from staging allowed",
			from: referencegrant.Reference{
				Group:     gatewayv1.GroupName,
				Kind:      "GRPCRoute",
				Namespace: "staging",
			},
			to: referencegrant.Reference{
				Group:     coreGroup,
				Kind:      "Service",
				Namespace: "production",
				Name:      "grpc-service",
			},
			shouldAllow: true,
		},
		{
			name: "HTTPRoute from staging denied",
			from: referencegrant.Reference{
				Group:     gatewayv1.GroupName,
				Kind:      "HTTPRoute",
				Namespace: "staging",
			},
			to: referencegrant.Reference{
				Group:     coreGroup,
				Kind:      "Service",
				Namespace: "production",
				Name:      "api-service",
			},
			shouldAllow: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			allowed, err := validator.IsReferenceAllowed(ctx, tt.from, tt.to)

			require.NoError(t, err)
			assert.Equal(t, tt.shouldAllow, allowed)
		})
	}
}

func TestValidator_IsReferenceAllowed_WrongKind(t *testing.T) {
	t.Parallel()

	scheme := setupScheme(t)

	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-services-only",
			Namespace: "production",
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1.GroupName,
					Kind:      "HTTPRoute",
					Namespace: "default",
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: coreGroup,
					Kind:  "Service",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(grant).
		Build()

	validator := referencegrant.NewValidator(fakeClient)

	from := referencegrant.Reference{
		Group:     gatewayv1.GroupName,
		Kind:      "HTTPRoute",
		Namespace: "default",
		Name:      "test-route",
	}

	to := referencegrant.Reference{
		Group:     coreGroup,
		Kind:      "Secret",
		Namespace: "production",
		Name:      "tls-cert",
	}

	ctx := context.Background()
	allowed, err := validator.IsReferenceAllowed(ctx, from, to)

	require.NoError(t, err)
	assert.False(t, allowed, "reference to wrong kind should be denied")
}

// TestValidator_IsReferenceAllowed_CoreGroupAlias tests that "core" is accepted
// as an alias for empty string in ReferenceGrant To.Group field.
// Per Gateway API documentation, both "" and "core" should work for core resources.
func TestValidator_IsReferenceAllowed_CoreGroupAlias(t *testing.T) {
	t.Parallel()

	scheme := setupScheme(t)

	// ReferenceGrant uses "core" as the group (not empty string)
	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-with-core-alias",
			Namespace: "backend",
		},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{
				{
					Group:     gatewayv1.GroupName,
					Kind:      "HTTPRoute",
					Namespace: "default",
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: "core", // Using "core" alias instead of ""
					Kind:  "Service",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(grant).
		Build()

	validator := referencegrant.NewValidator(fakeClient)

	from := referencegrant.Reference{
		Group:     gatewayv1.GroupName,
		Kind:      "HTTPRoute",
		Namespace: "default",
		Name:      "test-route",
	}

	// Builder always uses empty string for core group
	to := referencegrant.Reference{
		Group:     coreGroup, // empty string
		Kind:      "Service",
		Namespace: "backend",
		Name:      "backend-service",
	}

	ctx := context.Background()
	allowed, err := validator.IsReferenceAllowed(ctx, from, to)

	require.NoError(t, err)
	assert.True(t, allowed, "ReferenceGrant with 'core' group should match references with empty group")
}

// setupScheme creates a scheme with all required types.
func setupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()

	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, gatewayv1.AddToScheme(scheme))
	require.NoError(t, gatewayv1beta1.AddToScheme(scheme))

	return scheme
}

// objectNamePtr returns a pointer to an ObjectName.
func objectNamePtr(name string) *gatewayv1.ObjectName {
	objName := gatewayv1.ObjectName(name)
	return &objName
}
