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
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

func TestGRPCRouteReconciler_Reconcile_NotFound(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

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
	require.NoError(t, gatewayv1.Install(scheme))

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
	require.NoError(t, gatewayv1.Install(scheme))
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

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

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
								Kind: new(gatewayv1.Kind("Service")),
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
									From: new(gatewayv1.NamespacesFromAll),
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
								Namespace: new(gatewayv1.Namespace("other-namespace")),
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
							Hostname: new(gatewayv1.Hostname("*.example.com")),
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
							Hostname: new(gatewayv1.Hostname("*.example.com")),
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
									From: new(gatewayv1.NamespacesFromSame),
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
								Namespace: new(gatewayv1.Namespace("gateway-ns")),
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
									From: new(gatewayv1.NamespacesFromAll),
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
								Namespace: new(gatewayv1.Namespace("gateway-ns")),
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
							Hostname: new(gatewayv1.Hostname("*.example.com")),
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
							Hostname: new(gatewayv1.Hostname("*.example.com")),
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
			require.NoError(t, gatewayv1.Install(scheme))
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
	require.NoError(t, gatewayv1.Install(scheme))

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
	require.NoError(t, gatewayv1.Install(scheme))
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
	require.NoError(t, gatewayv1.Install(scheme))

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
	require.NoError(t, gatewayv1.Install(scheme))

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
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

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
	require.NoError(t, gatewayv1.Install(scheme))
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

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

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
	require.NoError(t, gatewayv1.Install(scheme))
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

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

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
	require.NoError(t, gatewayv1.Install(scheme))
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

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

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
	require.NoError(t, gatewayv1.Install(scheme))

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

func TestGRPCRouteReconciler_FindRoutesForReferenceGrant(t *testing.T) {
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
				{
					Name:     "grpc",
					Port:     443,
					Protocol: gatewayv1.HTTPSProtocolType,
				},
			},
		},
	}

	backendNs := gatewayv1.Namespace("backend-ns")
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
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name:      "backend-svc",
									Namespace: &backendNs,
								},
							},
						},
					},
				},
			},
		},
	}

	// A route for a different gateway (should not be included)
	otherRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-route",
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
		WithObjects(gateway, route, otherRoute).
		Build()

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		bindingValidator: routebinding.NewValidator(fakeClient),
	}

	// Test with wrong type - should return nil
	wrongObj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "not-a-grant", Namespace: "default"},
	}
	requests := r.findRoutesForReferenceGrant(context.Background(), wrongObj)
	assert.Nil(t, requests)
}

func TestGRPCRouteReconciler_UpdateRouteStatus_WithSyncError(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
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
				{Name: "grpc", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
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

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
	}

	// Test with sync error
	syncErr := assert.AnError
	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: true, Reason: gatewayv1.RouteReasonAccepted, Message: "Accepted"},
		},
	}
	err := r.updateRouteStatus(context.Background(), route, bindingInfo, nil, syncErr)
	require.NoError(t, err)

	var updatedRoute gatewayv1.GRPCRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "test-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)

	require.Len(t, updatedRoute.Status.Parents, 1)

	var acceptedCond *metav1.Condition
	for i := range updatedRoute.Status.Parents[0].Conditions {
		if updatedRoute.Status.Parents[0].Conditions[i].Type == string(gatewayv1.RouteConditionAccepted) {
			acceptedCond = &updatedRoute.Status.Parents[0].Conditions[i]

			break
		}
	}

	require.NotNil(t, acceptedCond)
	assert.Equal(t, metav1.ConditionFalse, acceptedCond.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonPending), acceptedCond.Reason)
}

func TestGRPCRouteReconciler_UpdateRouteStatus_WithFailedBackendRefs(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
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
				{Name: "grpc", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
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

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
	}

	// Test with failed backend refs
	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: true, Reason: gatewayv1.RouteReasonAccepted, Message: "Accepted"},
		},
	}

	failedRefs := []ingress.BackendRefError{
		{
			RouteNamespace: "default",
			RouteName:      "test-route",
			BackendNS:      "other-ns",
			BackendName:    "backend-svc",
		},
	}

	err := r.updateRouteStatus(context.Background(), route, bindingInfo, failedRefs, nil)
	require.NoError(t, err)

	var updatedRoute gatewayv1.GRPCRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "test-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)

	require.Len(t, updatedRoute.Status.Parents, 1)

	// Check ResolvedRefs condition
	var resolvedRefsCond *metav1.Condition
	for i := range updatedRoute.Status.Parents[0].Conditions {
		if updatedRoute.Status.Parents[0].Conditions[i].Type == string(gatewayv1.RouteConditionResolvedRefs) {
			resolvedRefsCond = &updatedRoute.Status.Parents[0].Conditions[i]

			break
		}
	}

	require.NotNil(t, resolvedRefsCond)
	assert.Equal(t, metav1.ConditionFalse, resolvedRefsCond.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonRefNotPermitted), resolvedRefsCond.Reason)
	assert.Contains(t, resolvedRefsCond.Message, "other-ns/backend-svc")
}

func TestGRPCRouteReconciler_UpdateRouteStatus_NonGatewayParentRef(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	serviceKind := gatewayv1.Kind("Service")
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route",
			Namespace: "default",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name: "some-service",
						Kind: &serviceKind,
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(route).
		WithStatusSubresource(route).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
	}

	bindingInfo := routeBindingInfo{bindingResults: map[int]routebinding.BindingResult{}}
	err := r.updateRouteStatus(context.Background(), route, bindingInfo, nil, nil)
	require.NoError(t, err)

	var updatedRoute gatewayv1.GRPCRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "test-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)

	// Non-gateway parent refs should be skipped
	assert.Empty(t, updatedRoute.Status.Parents)
}

func TestGRPCRouteReconciler_FindRoutesForReferenceGrant_CrossNs(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, gatewayv1beta1.Install(scheme))

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{Name: "grpc", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
			},
		},
	}

	backendNs := gatewayv1.Namespace("backend-ns")
	crossNsRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cross-ns-grpc-route",
			Namespace: "default",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name:      "remote-svc",
									Namespace: &backendNs,
								},
							},
						},
					},
				},
			},
		},
	}

	localRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "local-grpc-route",
			Namespace: "default",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "local-svc",
								},
							},
						},
					},
				},
			},
		},
	}

	refGrant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-backend",
			Namespace: "backend-ns",
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
				{Group: "", Kind: "Service"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gateway, crossNsRoute, localRoute, refGrant).
		Build()

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		bindingValidator: routebinding.NewValidator(fakeClient),
	}

	requests := r.findRoutesForReferenceGrant(context.Background(), refGrant)

	// Only the cross-namespace route should be returned
	require.Len(t, requests, 1)
	assert.Equal(t, "cross-ns-grpc-route", requests[0].Name)
}

func TestGRPCRouteReconciler_UpdateRouteStatus_MultipleParents(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	gw1 := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gateway-1",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{Name: "grpc", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
			},
		},
	}

	gw2 := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gateway-2",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{Name: "grpc", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
			},
		},
	}

	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "multi-parent-route",
			Namespace: "default",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "gateway-1"},
					{Name: "gateway-2"},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gw1, gw2, route).
		WithStatusSubresource(route).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
	}

	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: true, Reason: gatewayv1.RouteReasonAccepted, Message: "Accepted"},
			1: {Accepted: false, Reason: gatewayv1.RouteReasonNotAllowedByListeners, Message: "Not allowed"},
		},
	}

	err := r.updateRouteStatus(context.Background(), route, bindingInfo, nil, nil)
	require.NoError(t, err)

	var updatedRoute gatewayv1.GRPCRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "multi-parent-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)

	// Both parents should have status entries
	require.Len(t, updatedRoute.Status.Parents, 2)

	// First parent: accepted
	var cond0 *metav1.Condition
	for i := range updatedRoute.Status.Parents[0].Conditions {
		if updatedRoute.Status.Parents[0].Conditions[i].Type == string(gatewayv1.RouteConditionAccepted) {
			cond0 = &updatedRoute.Status.Parents[0].Conditions[i]

			break
		}
	}
	require.NotNil(t, cond0)
	assert.Equal(t, metav1.ConditionTrue, cond0.Status)

	// Second parent: not accepted
	var cond1 *metav1.Condition
	for i := range updatedRoute.Status.Parents[1].Conditions {
		if updatedRoute.Status.Parents[1].Conditions[i].Type == string(gatewayv1.RouteConditionAccepted) {
			cond1 = &updatedRoute.Status.Parents[1].Conditions[i]

			break
		}
	}
	require.NotNil(t, cond1)
	assert.Equal(t, metav1.ConditionFalse, cond1.Status)
}

func TestGRPCRouteReconciler_UpdateRouteStatus_ExplicitNamespace(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gw",
			Namespace: "gw-ns",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{Name: "grpc", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
			},
		},
	}

	gwNs := gatewayv1.Namespace("gw-ns")
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cross-ns-route",
			Namespace: "route-ns",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name:      "gw",
						Namespace: &gwNs,
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gateway, route).
		WithStatusSubresource(route).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
	}

	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: true, Reason: gatewayv1.RouteReasonAccepted, Message: "Accepted"},
		},
	}

	err := r.updateRouteStatus(context.Background(), route, bindingInfo, nil, nil)
	require.NoError(t, err)

	var updatedRoute gatewayv1.GRPCRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "cross-ns-route", Namespace: "route-ns"}, &updatedRoute)
	require.NoError(t, err)

	require.Len(t, updatedRoute.Status.Parents, 1)
	assert.Equal(t, gatewayv1.Namespace("gw-ns"), *updatedRoute.Status.Parents[0].ParentRef.Namespace)
}

func TestGRPCRouteReconciler_UpdateRouteStatus_MultipleFailedBackendRefs(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
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
				{Name: "grpc", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
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

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
	}

	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: true, Reason: gatewayv1.RouteReasonAccepted, Message: "Accepted"},
		},
	}

	failedRefs := []ingress.BackendRefError{
		{RouteNamespace: "default", RouteName: "test-route", BackendNS: "ns1", BackendName: "svc1"},
		{RouteNamespace: "default", RouteName: "test-route", BackendNS: "ns2", BackendName: "svc2"},
	}

	err := r.updateRouteStatus(context.Background(), route, bindingInfo, failedRefs, nil)
	require.NoError(t, err)

	var updatedRoute gatewayv1.GRPCRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "test-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)

	require.Len(t, updatedRoute.Status.Parents, 1)

	var resolvedRefsCond *metav1.Condition
	for i := range updatedRoute.Status.Parents[0].Conditions {
		if updatedRoute.Status.Parents[0].Conditions[i].Type == string(gatewayv1.RouteConditionResolvedRefs) {
			resolvedRefsCond = &updatedRoute.Status.Parents[0].Conditions[i]

			break
		}
	}

	require.NotNil(t, resolvedRefsCond)
	assert.Equal(t, metav1.ConditionFalse, resolvedRefsCond.Status)
	assert.Contains(t, resolvedRefsCond.Message, "ns1/svc1")
	assert.Contains(t, resolvedRefsCond.Message, "ns2/svc2")
}

func TestGRPCRouteReconciler_SyncAndUpdateStatus_WithRoutesAndConfigFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	allNamespaces := gatewayv1.NamespacesFromAll

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "nonexistent-config",
			},
		},
	}

	gateway := &gatewayv1.Gateway{
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
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{
							From: &allNamespaces,
						},
					},
				},
			},
		},
	}

	grpcRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-grpc-route",
			Namespace: "default",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
			Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, gatewayv1beta1.Install(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, grpcRoute).
		WithStatusSubresource(grpcRoute).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
		bindingValidator: routebinding.NewValidator(fakeClient),
	}
	r.startupComplete.Store(true)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-grpc-route",
			Namespace: "default",
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)

	// Verify route status was updated
	var updatedRoute gatewayv1.GRPCRoute
	err = fakeClient.Get(ctx, client.ObjectKey{Name: "test-grpc-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)
	require.NotEmpty(t, updatedRoute.Status.Parents)
}

func TestGRPCRouteReconciler_SyncAndUpdateStatus_MultipleRoutes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	allNamespaces := gatewayv1.NamespacesFromAll

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "missing-config",
			},
		},
	}

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-gateway",
			Namespace: "default",
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
							From: &allNamespaces,
						},
					},
				},
			},
		},
	}

	route1 := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "grpc-route-one",
			Namespace: "default",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "my-gateway"},
				},
			},
			Hostnames: []gatewayv1.Hostname{"grpc1.example.com"},
		},
	}

	route2 := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "grpc-route-two",
			Namespace: "default",
		},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "my-gateway"},
				},
			},
			Hostnames: []gatewayv1.Hostname{"grpc2.example.com"},
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, gatewayv1beta1.Install(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route1, route2).
		WithStatusSubresource(route1, route2).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
		bindingValidator: routebinding.NewValidator(fakeClient),
	}
	r.startupComplete.Store(true)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "grpc-route-one",
			Namespace: "default",
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)

	var updated1 gatewayv1.GRPCRoute
	err = fakeClient.Get(ctx, client.ObjectKey{Name: "grpc-route-one", Namespace: "default"}, &updated1)
	require.NoError(t, err)
	require.NotEmpty(t, updated1.Status.Parents)

	var updated2 gatewayv1.GRPCRoute
	err = fakeClient.Get(ctx, client.ObjectKey{Name: "grpc-route-two", Namespace: "default"}, &updated2)
	require.NoError(t, err)
	require.NotEmpty(t, updated2.Status.Parents)
}

func TestGRPCRouteReconciler_Reconcile_RouteNotForOurGateway(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	allNamespaces := gatewayv1.NamespacesFromAll

	otherGateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "other-class",
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{
							From: &allNamespaces,
						},
					},
				},
			},
		},
	}

	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-grpc-route",
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

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, gatewayv1beta1.Install(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(otherGateway, route).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &GRPCRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		GatewayClassName: "cloudflare-tunnel",
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
		bindingValidator: routebinding.NewValidator(fakeClient),
	}
	r.startupComplete.Store(true)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "other-grpc-route",
			Namespace: "default",
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}
