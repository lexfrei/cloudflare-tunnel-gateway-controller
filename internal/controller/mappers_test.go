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
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
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
		Client:         fakeClient,
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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
		Client:         fakeClient,
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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
		Client:         fakeClient,
		ControllerName: "test-controller",
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
		Client:         fakeClient,
		ControllerName: "non-existent-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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
		Client:         fakeClient,
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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
		Client:         fakeClient,
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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
		Client:         fakeClient,
		ControllerName: "test-controller",
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
		Client:         fakeClient,
		ControllerName: "non-existent-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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
		Client:         fakeClient,
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
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
		routes         []Route
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
			routes: []Route{
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "app-ns"},
					Spec: gatewayv1.HTTPRouteSpec{
						Rules: []gatewayv1.HTTPRouteRule{
							{BackendRefs: []gatewayv1.HTTPBackendRef{
								{BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Namespace: new(gatewayv1.Namespace("backend-ns")),
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
			routes: []Route{
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
			routes:        []Route{},
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
			routes: []Route{
				GRPCRouteWrapper{&gatewayv1.GRPCRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "grpc-route", Namespace: "frontend"},
					Spec: gatewayv1.GRPCRouteSpec{
						Rules: []gatewayv1.GRPCRouteRule{
							{BackendRefs: []gatewayv1.GRPCBackendRef{
								{BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Namespace: new(gatewayv1.Namespace("grpc-backend")),
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
				assert.Equal(t, expected, result[i].String())
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
									Namespace: new(gatewayv1.Namespace("backend-ns")),
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
									Namespace: new(gatewayv1.Namespace("app-ns")),
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
									Namespace: new(gatewayv1.Namespace("backend-ns")),
								},
							}},
							{BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Namespace: new(gatewayv1.Namespace("backend-ns")),
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
									Namespace: new(gatewayv1.Namespace("grpc-backend")),
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

func TestIsRouteAcceptedByGateway(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		controllerName    string
		gateway           *gatewayv1.Gateway
		gwClassName       string
		gwClassController string
		route             Route
		expected          bool
	}{
		{
			name:              "http_route_accepted_by_gateway",
			controllerName:    "test-controller",
			gwClassName:       "cloudflare-tunnel",
			gwClassController: "test-controller",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
						},
					},
				},
			},
			route: HTTPRouteWrapper{&gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway"},
						},
					},
				},
			}},
			expected: true,
		},
		{
			name:              "grpc_route_accepted_by_gateway",
			controllerName:    "test-controller",
			gwClassName:       "cloudflare-tunnel",
			gwClassController: "test-controller",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners: []gatewayv1.Listener{
						{
							Name:     "grpc",
							Port:     443,
							Protocol: gatewayv1.HTTPSProtocolType,
						},
					},
				},
			},
			route: GRPCRouteWrapper{&gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.GRPCRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway"},
						},
					},
				},
			}},
			expected: true,
		},
		{
			name:              "route_rejected_hostname_mismatch",
			controllerName:    "test-controller",
			gwClassName:       "cloudflare-tunnel",
			gwClassController: "test-controller",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							Hostname: new(gatewayv1.Hostname("*.example.com")),
						},
					},
				},
			},
			route: HTTPRouteWrapper{&gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.HTTPRouteSpec{
					Hostnames: []gatewayv1.Hostname{"other.org"},
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway"},
						},
					},
				},
			}},
			expected: false,
		},
		{
			name:              "route_rejected_namespace_not_allowed",
			controllerName:    "test-controller",
			gwClassName:       "cloudflare-tunnel",
			gwClassController: "test-controller",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "gateway-ns",
				},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: new(gatewayv1.NamespacesFromSame),
								},
							},
						},
					},
				},
			},
			route: HTTPRouteWrapper{&gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "other-ns",
				},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{
								Name:      "test-gateway",
								Namespace: new(gatewayv1.Namespace("gateway-ns")),
							},
						},
					},
				},
			}},
			expected: false,
		},
		{
			name:              "route_rejected_different_gateway_class",
			controllerName:    "test-controller",
			gwClassName:       "other-class",
			gwClassController: "other-controller",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "other-class",
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
						},
					},
				},
			},
			route: HTTPRouteWrapper{&gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway"},
						},
					},
				},
			}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			require.NoError(t, gatewayv1.Install(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))

			gwClass := &gatewayv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: tt.gwClassName},
				Spec:       gatewayv1.GatewayClassSpec{ControllerName: gatewayv1.GatewayController(tt.gwClassController)},
			}

			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gwClass)
			if tt.gateway != nil {
				builder = builder.WithObjects(tt.gateway)
			}

			fakeClient := builder.Build()

			validator := routebinding.NewValidator(fakeClient)

			result := IsRouteAcceptedByGateway(
				context.Background(),
				fakeClient,
				validator,
				tt.controllerName,
				tt.route,
			)

			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFindRoutesForGateway(t *testing.T) {
	t.Parallel()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}

	otherGatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "other-class"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "other-controller"},
	}

	fakeClient := setupMapperFakeClient(gatewayClass, otherGatewayClass)

	tests := []struct {
		name           string
		obj            client.Object
		controllerName string
		routes         []Route
		expectedCount  int
		expectedRoutes []string
	}{
		{
			name: "returns nil for non-gateway object",
			obj: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "not-a-gateway"},
			},
			controllerName: "test-controller",
			routes: []Route{
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "default"},
				}},
			},
			expectedCount: 0,
		},
		{
			name: "returns nil for non-matching gateway class",
			obj: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "other-class",
				},
			},
			controllerName: "test-controller",
			routes: []Route{
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "default"},
					Spec: gatewayv1.HTTPRouteSpec{
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{{Name: "test-gw"}},
						},
					},
				}},
			},
			expectedCount: 0,
		},
		{
			name: "returns routes matching gateway by parent ref",
			obj: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
				},
			},
			controllerName: "test-controller",
			routes: []Route{
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "default"},
					Spec: gatewayv1.HTTPRouteSpec{
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{{Name: "test-gw"}},
						},
					},
				}},
			},
			expectedCount:  1,
			expectedRoutes: []string{"default/route1"},
		},
		{
			name: "returns empty for routes not referencing gateway",
			obj: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
				},
			},
			controllerName: "test-controller",
			routes: []Route{
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "default"},
					Spec: gatewayv1.HTTPRouteSpec{
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{{Name: "other-gateway"}},
						},
					},
				}},
			},
			expectedCount: 0,
		},
		{
			name: "handles multiple routes with different parent refs",
			obj: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
				},
			},
			controllerName: "test-controller",
			routes: []Route{
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: "default"},
					Spec: gatewayv1.HTTPRouteSpec{
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{{Name: "test-gw"}},
						},
					},
				}},
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "route2", Namespace: "default"},
					Spec: gatewayv1.HTTPRouteSpec{
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{{Name: "other-gw"}},
						},
					},
				}},
				GRPCRouteWrapper{&gatewayv1.GRPCRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "grpc-route", Namespace: "app"},
					Spec: gatewayv1.GRPCRouteSpec{
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{{
								Name:      "test-gw",
								Namespace: (*gatewayv1.Namespace)(new(string)),
							}},
						},
					},
				}},
			},
			// grpc-route in "app" references test-gw with empty namespace string,
			// which doesn't match the gateway in "default".
			expectedCount:  1,
			expectedRoutes: []string{"default/route1"},
		},
		{
			name: "cross-namespace route without explicit namespace does not match",
			obj: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
				},
			},
			controllerName: "test-controller",
			routes: []Route{
				// Route in "app" references "test-gw" without namespace.
				// Per Gateway API spec, nil namespace means route's own namespace.
				// So it looks for test-gw in "app", not "default".
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "cross-ns-route", Namespace: "app"},
					Spec: gatewayv1.HTTPRouteSpec{
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{{Name: "test-gw"}},
						},
					},
				}},
			},
			expectedCount: 0,
		},
		{
			name: "returns empty for empty routes slice",
			obj: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gw"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel"},
			},
			controllerName: "test-controller",
			routes:         []Route{},
			expectedCount:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := FindRoutesForGateway(context.Background(), fakeClient, tt.obj, tt.controllerName, tt.routes)

			assert.Len(t, result, tt.expectedCount)

			for i, expected := range tt.expectedRoutes {
				assert.Equal(t, expected, result[i].String())
			}
		})
	}
}

func TestFilterAcceptedRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		controllerName string
		gateway        *gatewayv1.Gateway
		routes         []Route
		expectedCount  int
		expectedRoutes []string
	}{
		{
			name:           "returns empty for empty routes slice",
			controllerName: "test-controller",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
				},
			},
			routes:        []Route{},
			expectedCount: 0,
		},
		{
			name:           "returns only accepted routes",
			controllerName: "test-controller",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
				},
			},
			routes: []Route{
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "accepted-route", Namespace: "default"},
					Spec: gatewayv1.HTTPRouteSpec{
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{{Name: "test-gw"}},
						},
					},
				}},
			},
			expectedCount:  1,
			expectedRoutes: []string{"default/accepted-route"},
		},
		{
			name:           "filters out routes referencing non-existent gateway",
			controllerName: "test-controller",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
				},
			},
			routes: []Route{
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "orphan-route", Namespace: "default"},
					Spec: gatewayv1.HTTPRouteSpec{
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{{Name: "non-existent-gw"}},
						},
					},
				}},
			},
			expectedCount: 0,
		},
		{
			name:           "handles mixed accepted and rejected routes",
			controllerName: "test-controller",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "default"},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners: []gatewayv1.Listener{{
						Name:     "http",
						Port:     80,
						Protocol: gatewayv1.HTTPProtocolType,
						Hostname: new(gatewayv1.Hostname("*.example.com")),
					}},
				},
			},
			routes: []Route{
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "matching-route", Namespace: "default"},
					Spec: gatewayv1.HTTPRouteSpec{
						Hostnames: []gatewayv1.Hostname{"app.example.com"},
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{{Name: "test-gw"}},
						},
					},
				}},
				HTTPRouteWrapper{&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "mismatched-route", Namespace: "default"},
					Spec: gatewayv1.HTTPRouteSpec{
						Hostnames: []gatewayv1.Hostname{"other.org"},
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{{Name: "test-gw"}},
						},
					},
				}},
			},
			expectedCount:  1,
			expectedRoutes: []string{"default/matching-route"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			require.NoError(t, gatewayv1.Install(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))

			gwClass := &gatewayv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
				Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
			}

			builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gwClass)
			if tt.gateway != nil {
				builder = builder.WithObjects(tt.gateway)
			}

			fakeClient := builder.Build()
			validator := routebinding.NewValidator(fakeClient)

			result := FilterAcceptedRoutes(
				context.Background(),
				fakeClient,
				validator,
				tt.controllerName,
				tt.routes,
			)

			assert.Len(t, result, tt.expectedCount)

			for i, expected := range tt.expectedRoutes {
				assert.Equal(t, expected, result[i].String())
			}
		})
	}
}

func TestIsRouteAcceptedByGateway_NonGatewayKind(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gateway, gatewayClass).Build()
	validator := routebinding.NewValidator(fakeClient)

	serviceKind := gatewayv1.Kind("Service")
	route := HTTPRouteWrapper{&gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "test-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &serviceKind, Name: "test-service"},
				},
			},
		},
	}}

	result := IsRouteAcceptedByGateway(context.Background(), fakeClient, validator, "test-controller", route)
	assert.False(t, result)
}

func TestIsRouteAcceptedByGateway_GatewayNotFound(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	validator := routebinding.NewValidator(fakeClient)

	route := HTTPRouteWrapper{&gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "test-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "missing-gateway"},
				},
			},
		},
	}}

	result := IsRouteAcceptedByGateway(context.Background(), fakeClient, validator, "test-controller", route)
	assert.False(t, result)
}

func TestIsRouteAcceptedByGateway_CrossNamespaceExplicit(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gw", Namespace: "gw-ns"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{
					Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: new(gatewayv1.NamespacesFromAll)},
					},
				},
			},
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gateway, gatewayClass).Build()
	validator := routebinding.NewValidator(fakeClient)

	gwNs := gatewayv1.Namespace("gw-ns")
	route := HTTPRouteWrapper{&gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "route-ns"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gw", Namespace: &gwNs},
				},
			},
		},
	}}

	result := IsRouteAcceptedByGateway(context.Background(), fakeClient, validator, "test-controller", route)
	assert.True(t, result)
}

func TestConfigMapper_IsSecretReferencedByConfig_GetConfigError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cf-credentials", Namespace: "default"},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
		},
	}

	fakeClient := setupMapperFakeClient(secret, gatewayClass)
	mapper := &ConfigMapper{
		Client:         fakeClient,
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
	}

	result := mapper.isSecretReferencedByConfig(ctx, secret)
	assert.False(t, result)
}

func TestManagedClassNames(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	class1 := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class-a"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}
	class2 := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class-b"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}
	otherClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "other-class"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "other-controller"},
	}

	fakeClient := setupMapperFakeClient(class1, class2, otherClass)

	names, err := managedClassNames(ctx, fakeClient, "test-controller")
	require.NoError(t, err)

	assert.True(t, names["class-a"])
	assert.True(t, names["class-b"])
	assert.False(t, names["other-class"])
}

func TestManagedClassNames_Empty(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	fakeClient := setupMapperFakeClient()

	names, err := managedClassNames(ctx, fakeClient, "test-controller")
	require.NoError(t, err)

	assert.Empty(t, names)
}

func TestListGatewayClassesForController(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	class1 := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class-a"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "example.com/my-controller"},
	}
	class2 := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "class-b"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "example.com/my-controller"},
	}
	otherClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "other"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "example.com/other"},
	}

	fakeClient := setupMapperFakeClient(class1, class2, otherClass)

	// Matching controllerName returns both classes.
	matched, err := listGatewayClassesForController(ctx, fakeClient, "example.com/my-controller")
	require.NoError(t, err)
	assert.Len(t, matched, 2)

	// Non-matching controllerName returns empty.
	none, err := listGatewayClassesForController(ctx, fakeClient, "example.com/nonexistent")
	require.NoError(t, err)
	assert.Empty(t, none)

	// No classes at all returns empty.
	emptyClient := setupMapperFakeClient()
	empty, err := listGatewayClassesForController(ctx, emptyClient, "example.com/my-controller")
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestIsGatewayManagedByController_GatewayClassNotFound verifies that a missing
// GatewayClass causes the function to return false (not panic).
func TestIsGatewayManagedByController_GatewayClassNotFound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "non-existent-class"},
	}

	fakeClient := setupMapperFakeClient(gateway)

	assert.False(t, isGatewayManagedByController(ctx, fakeClient, gateway, "any-controller"))
}

// TestIsGatewayManagedByController_ClassNameDiffersFromControllerName verifies
// that gateway management is determined by controllerName, not by class name.
// This is the key indirection per Gateway API spec.
func TestIsGatewayManagedByController_ClassNameDiffersFromControllerName(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "my-custom-tunnel-class"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "example.com/tunnel-controller"},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "my-custom-tunnel-class",
		},
	}

	fakeClient := setupMapperFakeClient(gatewayClass, gateway)

	// Matches by controllerName — should return true
	assert.True(t, isGatewayManagedByController(ctx, fakeClient, gateway, "example.com/tunnel-controller"))

	// Does NOT match by class name — should return false
	assert.False(t, isGatewayManagedByController(ctx, fakeClient, gateway, "my-custom-tunnel-class"))
}

// TestFindRoutesForGateway_MultipleClassesSameController verifies that routes
// are found when multiple GatewayClasses reference the same controllerName.
func TestFindRoutesForGateway_MultipleClassesSameController(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	classA := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "tunnel-prod"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "example.com/tunnel"},
	}
	classB := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "tunnel-staging"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "example.com/tunnel"},
	}

	gwProd := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-prod", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "tunnel-prod"},
	}

	fakeClient := setupMapperFakeClient(classA, classB, gwProd)

	routes := []Route{
		HTTPRouteWrapper{&gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "route-prod", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{
					ParentRefs: []gatewayv1.ParentReference{{Name: "gw-prod"}},
				},
			},
		}},
	}

	result := FindRoutesForGateway(ctx, fakeClient, gwProd, "example.com/tunnel", routes)
	require.Len(t, result, 1)
	assert.Equal(t, "default/route-prod", result[0].String())
}
