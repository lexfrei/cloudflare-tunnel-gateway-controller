package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

func TestConfigMapper_MapConfigToRequests_ValidConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "test-tunnel",
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "test-config",
			},
		},
	}

	fakeClient := setupMapperFakeClient(gatewayClassConfig, gatewayClass)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	expectedRequests := []reconcile.Request{
		{NamespacedName: client.ObjectKey{Name: "test", Namespace: "default"}},
	}

	mapFunc := mapper.MapConfigToRequests(func(_ context.Context) []reconcile.Request {
		return expectedRequests
	})

	result := mapFunc(ctx, gatewayClassConfig)

	require.NotNil(t, result)
	assert.Equal(t, expectedRequests, result)
}

func TestConfigMapper_MapConfigToRequests_WrongConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "wrong-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "test-tunnel",
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "correct-config",
			},
		},
	}

	fakeClient := setupMapperFakeClient(gatewayClassConfig, gatewayClass)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	mapFunc := mapper.MapConfigToRequests(func(_ context.Context) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: "test"}}}
	})

	result := mapFunc(ctx, gatewayClassConfig)

	assert.Nil(t, result)
}

func TestConfigMapper_MapConfigToRequests_WrongType(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupMapperFakeClient()
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
	}

	mapFunc := mapper.MapConfigToRequests(func(_ context.Context) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: "test"}}}
	})

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-a-config",
			Namespace: "default",
		},
	}

	result := mapFunc(ctx, secret)

	assert.Nil(t, result)
}

func TestConfigMapper_MapConfigToRequests_GatewayClassNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
	}

	fakeClient := setupMapperFakeClient(gatewayClassConfig)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "non-existent-class",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	mapFunc := mapper.MapConfigToRequests(func(_ context.Context) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: "test"}}}
	})

	result := mapFunc(ctx, gatewayClassConfig)

	assert.Nil(t, result)
}

func TestConfigMapper_MapSecretToRequests_ValidSecret(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-token": []byte("test-token"),
		},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelID: "test-tunnel",
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "test-config",
			},
		},
	}

	fakeClient := setupMapperFakeClient(secret, gatewayClassConfig, gatewayClass)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	expectedRequests := []reconcile.Request{
		{NamespacedName: client.ObjectKey{Name: "test", Namespace: "default"}},
	}

	mapFunc := mapper.MapSecretToRequests(func(_ context.Context) []reconcile.Request {
		return expectedRequests
	})

	result := mapFunc(ctx, secret)

	require.NotNil(t, result)
	assert.Equal(t, expectedRequests, result)
}

func TestConfigMapper_MapSecretToRequests_WrongSecret(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "wrong-secret",
			Namespace: "default",
		},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelID: "test-tunnel",
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "test-config",
			},
		},
	}

	fakeClient := setupMapperFakeClient(secret, gatewayClassConfig, gatewayClass)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	mapFunc := mapper.MapSecretToRequests(func(_ context.Context) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: "test"}}}
	})

	result := mapFunc(ctx, secret)

	assert.Nil(t, result)
}

func TestConfigMapper_MapSecretToRequests_WrongType(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupMapperFakeClient()
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
	}

	mapFunc := mapper.MapSecretToRequests(func(_ context.Context) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: "test"}}}
	})

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "not-a-secret",
		},
	}

	result := mapFunc(ctx, gatewayClassConfig)

	assert.Nil(t, result)
}

func TestConfigMapper_MapSecretToRequests_GatewayClassNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
	}

	fakeClient := setupMapperFakeClient(secret)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "non-existent-class",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	mapFunc := mapper.MapSecretToRequests(func(_ context.Context) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: "test"}}}
	})

	result := mapFunc(ctx, secret)

	assert.Nil(t, result)
}

func TestSecretMatchesConfig_CredentialsSecret(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "default",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
		},
	}

	assert.True(t, SecretMatchesConfig(secret, cfg))
}

func TestSecretMatchesConfig_CredentialsSecretEmptyNamespace(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "any-ns",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "",
			},
		},
	}

	assert.True(t, SecretMatchesConfig(secret, cfg))
}

func TestSecretMatchesConfig_TunnelTokenSecret(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "default",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "default",
			},
		},
	}

	assert.True(t, SecretMatchesConfig(secret, cfg))
}

func TestSecretMatchesConfig_TunnelTokenSecretEmptyNamespace(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "other-ns",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name: "cf-credentials",
			},
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "",
			},
		},
	}

	assert.True(t, SecretMatchesConfig(secret, cfg))
}

func TestSecretMatchesConfig_NoMatch(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "random-secret",
			Namespace: "default",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelTokenSecretRef: &v1alpha1.SecretReference{
				Name:      "tunnel-token",
				Namespace: "default",
			},
		},
	}

	assert.False(t, SecretMatchesConfig(secret, cfg))
}

func TestSecretMatchesConfig_NoTunnelTokenRef(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tunnel-token",
			Namespace: "default",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
			TunnelTokenSecretRef: nil,
		},
	}

	assert.False(t, SecretMatchesConfig(secret, cfg))
}

func TestSecretMatchesConfig_WrongNamespace(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cf-credentials",
			Namespace: "wrong-ns",
		},
	}

	cfg := &v1alpha1.GatewayClassConfig{
		Spec: v1alpha1.GatewayClassConfigSpec{
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
			},
		},
	}

	assert.False(t, SecretMatchesConfig(secret, cfg))
}

func TestConfigMapper_IsConfigForOurClass_NoParametersRef(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef:  nil,
		},
	}

	fakeClient := setupMapperFakeClient(gatewayClassConfig, gatewayClass)
	mapper := &ConfigMapper{
		Client:           fakeClient,
		GatewayClassName: "cloudflare-tunnel",
		ConfigResolver:   config.NewResolver(fakeClient, "default"),
	}

	result := mapper.isConfigForOurClass(ctx, gatewayClassConfig)

	assert.False(t, result)
}

func setupMapperFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}

func TestFindRoutesForReferenceGrant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		refGrant       client.Object
		routes         []RouteWithCrossNamespaceRefs
		expectedCount  int
		expectedRoutes []string
	}{
		{
			name: "matches routes with cross-namespace refs to target namespace",
			refGrant: &gatewayv1beta1.ReferenceGrant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "allow-cross-ns",
					Namespace: "backend-ns",
				},
			},
			routes: []RouteWithCrossNamespaceRefs{
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "app-ns"},
					Spec: gatewayv1.HTTPRouteSpec{
						Rules: []gatewayv1.HTTPRouteRule{
							{BackendRefs: []gatewayv1.HTTPBackendRef{
								{BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Namespace: ptr(gatewayv1.Namespace("backend-ns")),
									},
								}},
							}},
						},
					},
				}},
			},
			expectedCount:  1,
			expectedRoutes: []string{"app-ns/route1"},
		},
		{
			name: "does not match routes without cross-namespace refs",
			refGrant: &gatewayv1beta1.ReferenceGrant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "allow-cross-ns",
					Namespace: "backend-ns",
				},
			},
			routes: []RouteWithCrossNamespaceRefs{
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "app-ns"},
					Spec: gatewayv1.HTTPRouteSpec{
						Rules: []gatewayv1.HTTPRouteRule{
							{BackendRefs: []gatewayv1.HTTPBackendRef{
								{BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Name: "svc",
									},
								}},
							}},
						},
					},
				}},
			},
			expectedCount: 0,
		},
		{
			name: "wrong object type returns nil",
			refGrant: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "not-a-refgrant"},
			},
			routes:        []RouteWithCrossNamespaceRefs{},
			expectedCount: 0,
		},
		{
			name: "grpc routes with cross-namespace refs",
			refGrant: &gatewayv1beta1.ReferenceGrant{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "allow-grpc",
					Namespace: "grpc-backend",
				},
			},
			routes: []RouteWithCrossNamespaceRefs{
				GRPCRouteWrapper{&gatewayv1.GRPCRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "grpc-route", Namespace: "frontend"},
					Spec: gatewayv1.GRPCRouteSpec{
						Rules: []gatewayv1.GRPCRouteRule{
							{BackendRefs: []gatewayv1.GRPCBackendRef{
								{BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Namespace: ptr(gatewayv1.Namespace("grpc-backend")),
									},
								}},
							}},
						},
					},
				}},
			},
			expectedCount:  1,
			expectedRoutes: []string{"frontend/grpc-route"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := FindRoutesForReferenceGrant(tt.refGrant, tt.routes)

			assert.Len(t, result, tt.expectedCount)

			for i, expected := range tt.expectedRoutes {
				assert.Equal(t, expected, result[i].NamespacedName.String())
			}
		})
	}
}

func TestHTTPRouteWrapper_GetCrossNamespaceBackendNamespaces(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		route    *gatewayv1.HTTPRoute
		expected []string
	}{
		{
			name: "returns cross-namespace backend namespaces",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "app-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: []gatewayv1.HTTPRouteRule{
						{BackendRefs: []gatewayv1.HTTPBackendRef{
							{BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Namespace: ptr(gatewayv1.Namespace("backend-ns")),
								},
							}},
						}},
					},
				},
			},
			expected: []string{"backend-ns"},
		},
		{
			name: "excludes same-namespace backends",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "app-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: []gatewayv1.HTTPRouteRule{
						{BackendRefs: []gatewayv1.HTTPBackendRef{
							{BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Namespace: ptr(gatewayv1.Namespace("app-ns")),
								},
							}},
						}},
					},
				},
			},
			expected: nil,
		},
		{
			name: "deduplicates namespaces",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "app-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: []gatewayv1.HTTPRouteRule{
						{BackendRefs: []gatewayv1.HTTPBackendRef{
							{BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Namespace: ptr(gatewayv1.Namespace("backend-ns")),
								},
							}},
							{BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Namespace: ptr(gatewayv1.Namespace("backend-ns")),
								},
							}},
						}},
					},
				},
			},
			expected: []string{"backend-ns"},
		},
		{
			name: "handles nil namespace (same namespace)",
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "app-ns"},
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: []gatewayv1.HTTPRouteRule{
						{BackendRefs: []gatewayv1.HTTPBackendRef{
							{BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "svc",
								},
							}},
						}},
					},
				},
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wrapper := HTTPRouteWrapper{tt.route}
			result := wrapper.GetCrossNamespaceBackendNamespaces()

			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGRPCRouteWrapper_GetCrossNamespaceBackendNamespaces(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		route    *gatewayv1.GRPCRoute
		expected []string
	}{
		{
			name: "returns cross-namespace backend namespaces",
			route: &gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "app-ns"},
				Spec: gatewayv1.GRPCRouteSpec{
					Rules: []gatewayv1.GRPCRouteRule{
						{BackendRefs: []gatewayv1.GRPCBackendRef{
							{BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Namespace: ptr(gatewayv1.Namespace("grpc-backend")),
								},
							}},
						}},
					},
				},
			},
			expected: []string{"grpc-backend"},
		},
		{
			name: "handles empty rules",
			route: &gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{Namespace: "app-ns"},
				Spec:       gatewayv1.GRPCRouteSpec{},
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wrapper := GRPCRouteWrapper{tt.route}
			result := wrapper.GetCrossNamespaceBackendNamespaces()

			assert.Equal(t, tt.expected, result)
		})
	}
}
