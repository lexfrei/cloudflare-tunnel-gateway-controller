package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/metrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

func TestGRPCRouteReconciler_Reconcile_NotFound(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, metrics.NewNoopCollector())

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
	}
	r.startupComplete.Store(true)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "non-existent-route",
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)
}

func TestGRPCRouteReconciler_Reconcile_WaitForStartup(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
	}
	// Leave startupComplete as false (default)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-route",
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, startupPendingRequeueDelay, result.RequeueAfter)
}

func TestGRPCRouteReconciler_Reconcile_WrongGatewayClass(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	kindGatewayVal := gatewayv1.Kind("Gateway")

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "other-class",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "grpc",
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
				},
			},
		},
	}

	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route",
			Namespace: "default",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name: "test-gateway",
						Kind: &kindGatewayVal,
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gateway, route).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, metrics.NewNoopCollector())

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
	}
	r.startupComplete.Store(true)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-route",
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestGRPCRouteReconciler_IsRouteForOurGateway(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		gatewayClassName string
		gateway          *gatewayv1.Gateway
		route            *gatewayv1.GRPCRoute
		expected         bool
	}{
		{
			name:             "route_for_our_gateway",
			gatewayClassName: "cloudflare-tunnel",
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
			route: &gatewayv1.GRPCRoute{
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
			},
			expected: true,
		},
		{
			name:             "route_for_different_gateway_class",
			gatewayClassName: "cloudflare-tunnel",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "other-class",
					Listeners: []gatewayv1.Listener{
						{
							Name:     "grpc",
							Port:     443,
							Protocol: gatewayv1.HTTPSProtocolType,
						},
					},
				},
			},
			route: &gatewayv1.GRPCRoute{
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
			},
			expected: false,
		},
		{
			name:             "route_with_non_gateway_parent",
			gatewayClassName: "cloudflare-tunnel",
			gateway:          nil,
			route: &gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.GRPCRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{
								Name: "some-service",
								Kind: ptr(gatewayv1.Kind("Service")),
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name:             "route_with_explicit_namespace",
			gatewayClassName: "cloudflare-tunnel",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "other-namespace",
				},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners: []gatewayv1.Listener{
						{
							Name:     "grpc",
							Port:     443,
							Protocol: gatewayv1.HTTPSProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: ptr(gatewayv1.NamespacesFromAll),
								},
							},
						},
					},
				},
			},
			route: &gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.GRPCRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{
								Name:      "test-gateway",
								Namespace: ptr(gatewayv1.Namespace("other-namespace")),
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name:             "route_referencing_nonexistent_gateway",
			gatewayClassName: "cloudflare-tunnel",
			gateway:          nil,
			route: &gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.GRPCRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "nonexistent-gateway"},
						},
					},
				},
			},
			expected: false,
		},
		{
			name:             "route_hostname_no_intersection",
			gatewayClassName: "cloudflare-tunnel",
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
							Hostname: ptr(gatewayv1.Hostname("*.example.com")),
						},
					},
				},
			},
			route: &gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.GRPCRouteSpec{
					Hostnames: []gatewayv1.Hostname{"other.org"},
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway"},
						},
					},
				},
			},
			expected: false,
		},
		{
			name:             "route_hostname_wildcard_match",
			gatewayClassName: "cloudflare-tunnel",
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
							Hostname: ptr(gatewayv1.Hostname("*.example.com")),
						},
					},
				},
			},
			route: &gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.GRPCRouteSpec{
					Hostnames: []gatewayv1.Hostname{"api.example.com"},
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway"},
						},
					},
				},
			},
			expected: true,
		},
		{
			name:             "route_namespace_not_allowed_same",
			gatewayClassName: "cloudflare-tunnel",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "gateway-ns",
				},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners: []gatewayv1.Listener{
						{
							Name:     "grpc",
							Port:     443,
							Protocol: gatewayv1.HTTPSProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: ptr(gatewayv1.NamespacesFromSame),
								},
							},
						},
					},
				},
			},
			route: &gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "other-ns",
				},
				Spec: gatewayv1.GRPCRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{
								Name:      "test-gateway",
								Namespace: ptr(gatewayv1.Namespace("gateway-ns")),
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			name:             "route_namespace_allowed_all",
			gatewayClassName: "cloudflare-tunnel",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "gateway-ns",
				},
				Spec: gatewayv1.GatewaySpec{
					GatewayClassName: "cloudflare-tunnel",
					Listeners: []gatewayv1.Listener{
						{
							Name:     "grpc",
							Port:     443,
							Protocol: gatewayv1.HTTPSProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: ptr(gatewayv1.NamespacesFromAll),
								},
							},
						},
					},
				},
			},
			route: &gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "any-namespace",
				},
				Spec: gatewayv1.GRPCRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{
								Name:      "test-gateway",
								Namespace: ptr(gatewayv1.Namespace("gateway-ns")),
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			name:             "route_no_hostnames_matches_any_listener",
			gatewayClassName: "cloudflare-tunnel",
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
							Hostname: ptr(gatewayv1.Hostname("*.example.com")),
						},
					},
				},
			},
			route: &gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.GRPCRouteSpec{
					Hostnames: []gatewayv1.Hostname{}, // empty hostnames
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway"},
						},
					},
				},
			},
			expected: true,
		},
		{
			name:             "multi_level_subdomain_matches_wildcard",
			gatewayClassName: "cloudflare-tunnel",
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
							Hostname: ptr(gatewayv1.Hostname("*.example.com")),
						},
					},
				},
			},
			route: &gatewayv1.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.GRPCRouteSpec{
					Hostnames: []gatewayv1.Hostname{"a.b.example.com"}, // multi-level subdomain
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{
							{Name: "test-gateway"},
						},
					},
				},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			require.NoError(t, gatewayv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))

			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tt.gateway != nil {
				builder = builder.WithObjects(tt.gateway)
			}
			if tt.route != nil {
				builder = builder.WithObjects(tt.route)
			}
			fakeClient := builder.Build()

			r := &GRPCRouteReconciler{
				Client:           fakeClient,
				Scheme:           scheme,
				GatewayClassName: tt.gatewayClassName,
				bindingValidator: routebinding.NewValidator(fakeClient),
			}

			result := r.isRouteForOurGateway(context.Background(), tt.route)

			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGRPCRouteReconciler_FindRoutesForGateway(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.AddToScheme(scheme))

	gateway := &gatewayv1.Gateway{
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
	}

	route1 := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-1",
			Namespace: "default",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
		},
	}

	route2 := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-2",
			Namespace: "default",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "other-gateway"},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gateway, route1, route2).
		Build()

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
	}

	requests := r.findRoutesForGateway(context.Background(), gateway)

	require.Len(t, requests, 1)
	assert.Equal(t, "route-1", requests[0].Name)
	assert.Equal(t, "default", requests[0].Namespace)
}

func TestGRPCRouteReconciler_FindRoutesForGateway_WrongType(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
	}

	wrongType := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
	}

	requests := r.findRoutesForGateway(context.Background(), wrongType)

	assert.Nil(t, requests)
}

func TestGRPCRouteReconciler_FindRoutesForGateway_WrongClass(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.AddToScheme(scheme))

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "other-class",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "grpc",
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gateway).
		Build()

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
	}

	requests := r.findRoutesForGateway(context.Background(), gateway)

	assert.Nil(t, requests)
}

func TestGRPCRouteReconciler_GetAllRelevantRoutes(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.AddToScheme(scheme))

	gateway := &gatewayv1.Gateway{
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
	}

	route1 := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-1",
			Namespace: "default",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
		},
	}

	route2 := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-2",
			Namespace: "other-namespace",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "other-gateway"},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gateway, route1, route2).
		Build()

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
	}

	requests := r.getAllRelevantRoutes(context.Background())

	require.Len(t, requests, 1)
	assert.Equal(t, "route-1", requests[0].Name)
}

func TestGRPCRouteReconciler_Start(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, metrics.NewNoopCollector())

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
	}

	assert.False(t, r.startupComplete.Load())

	err := r.Start(context.Background())

	assert.NoError(t, err)
	assert.True(t, r.startupComplete.Load())
}

func TestGRPCRouteReconciler_UpdateRouteStatus_Integration(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	gateway := &gatewayv1.Gateway{
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
	}

	route := &gatewayv1.GRPCRoute{
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
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gateway, route).
		WithStatusSubresource(route).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, metrics.NewNoopCollector())

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
	}

	// Test accepted status with binding info showing acceptance
	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {
				Accepted: true,
				Reason:   gatewayv1.RouteReasonAccepted,
				Message:  "Route accepted",
			},
		},
	}
	err := r.updateRouteStatus(context.Background(), route, bindingInfo, nil, nil)
	require.NoError(t, err)

	var updatedRoute gatewayv1.GRPCRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "test-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)

	require.Len(t, updatedRoute.Status.Parents, 1)
	assert.Equal(t, gatewayv1.GatewayController("test-controller"), updatedRoute.Status.Parents[0].ControllerName)
	require.Len(t, updatedRoute.Status.Parents[0].Conditions, 2)

	var acceptedCondition *metav1.Condition
	for i := range updatedRoute.Status.Parents[0].Conditions {
		if updatedRoute.Status.Parents[0].Conditions[i].Type == string(gatewayv1.RouteConditionAccepted) {
			acceptedCondition = &updatedRoute.Status.Parents[0].Conditions[i]
			break
		}
	}
	require.NotNil(t, acceptedCondition)
	assert.Equal(t, metav1.ConditionTrue, acceptedCondition.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonAccepted), acceptedCondition.Reason)
}

func TestGRPCRouteReconciler_UpdateRouteStatus_NotAccepted(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	gateway := &gatewayv1.Gateway{
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
	}

	route := &gatewayv1.GRPCRoute{
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
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gateway, route).
		WithStatusSubresource(route).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, metrics.NewNoopCollector())

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
	}

	// Test not accepted status with binding rejection
	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {
				Accepted: false,
				Reason:   gatewayv1.RouteReasonNoMatchingListenerHostname,
				Message:  "No listener hostname matches route hostnames",
			},
		},
	}
	err := r.updateRouteStatus(context.Background(), route, bindingInfo, nil, nil)
	require.NoError(t, err)

	var updatedRoute gatewayv1.GRPCRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "test-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)

	require.Len(t, updatedRoute.Status.Parents, 1)

	var acceptedCondition *metav1.Condition
	for i := range updatedRoute.Status.Parents[0].Conditions {
		if updatedRoute.Status.Parents[0].Conditions[i].Type == string(gatewayv1.RouteConditionAccepted) {
			acceptedCondition = &updatedRoute.Status.Parents[0].Conditions[i]
			break
		}
	}
	require.NotNil(t, acceptedCondition)
	assert.Equal(t, metav1.ConditionFalse, acceptedCondition.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonNoMatchingListenerHostname), acceptedCondition.Reason)
	assert.Equal(t, "No listener hostname matches route hostnames", acceptedCondition.Message)
}

func TestGRPCRouteReconciler_SyncAndUpdateStatus_NoConfig(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "cloudflare-tunnel-controller",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, metrics.NewNoopCollector())

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
	}

	result, err := r.syncAndUpdateStatus(context.Background())

	assert.NoError(t, err)
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)
}

func TestGRPCRouteReconciler_GetAllRelevantRoutes_Empty(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
	}

	requests := r.getAllRelevantRoutes(context.Background())

	assert.Empty(t, requests)
}
