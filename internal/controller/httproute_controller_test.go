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

func TestHTTPRouteReconciler_Reconcile_NotFound(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}
	// Mark startup complete so reconcile can proceed
	r.startupComplete.Store(true)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "non-existent-route",
			Namespace: "default",
		},
	}

	// Note: When route is not found, syncAllRoutes is called which tries to get config
	// This will fail because no GatewayClass exists, and returns requeue after delay
	result, err := r.Reconcile(context.Background(), req)

	// Since we don't have a GatewayClass, the sync will fail and request requeue
	assert.NoError(t, err)
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)
}

func TestHTTPRouteReconciler_Reconcile_WaitForStartup(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
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

func TestHTTPRouteReconciler_Reconcile_WrongGatewayClass(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	kindGatewayVal := gatewayv1.Kind("Gateway")

	// Create a gateway with different class
	gateway := &gatewayv1.Gateway{
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
	}

	// Create HTTPRoute referencing the gateway
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
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
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}
	r.startupComplete.Store(true)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-route",
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)

	// Should return empty result since route is not for our gateway
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestHTTPRouteReconciler_IsRouteForOurGateway(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		controllerName string
		gateway        *gatewayv1.Gateway
		route          *gatewayv1.HTTPRoute
		expected       bool
	}{
		{
			name:           "route_for_our_gateway",
			controllerName: "test-controller",
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
			route: &gatewayv1.HTTPRoute{
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
			},
			expected: true,
		},
		{
			name:           "route_for_different_gateway_class",
			controllerName: "test-controller",
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
			route: &gatewayv1.HTTPRoute{
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
			},
			expected: false,
		},
		{
			name:           "route_with_non_gateway_parent",
			controllerName: "test-controller",
			gateway:        nil,
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.HTTPRouteSpec{
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
			name:           "route_with_explicit_namespace",
			controllerName: "test-controller",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "other-namespace",
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
									From: new(gatewayv1.NamespacesFromAll),
								},
							},
						},
					},
				},
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.HTTPRouteSpec{
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
			name:           "route_referencing_nonexistent_gateway",
			controllerName: "test-controller",
			gateway:        nil,
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.HTTPRouteSpec{
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
			name:           "route_hostname_no_intersection",
			controllerName: "test-controller",
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
			route: &gatewayv1.HTTPRoute{
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
			},
			expected: false,
		},
		{
			name:           "route_hostname_wildcard_match",
			controllerName: "test-controller",
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
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.HTTPRouteSpec{
					Hostnames: []gatewayv1.Hostname{"app.example.com"},
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
			name:           "route_namespace_not_allowed_same",
			controllerName: "test-controller",
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
			route: &gatewayv1.HTTPRoute{
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
			},
			expected: false,
		},
		{
			name:           "route_namespace_allowed_all",
			controllerName: "test-controller",
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
									From: new(gatewayv1.NamespacesFromAll),
								},
							},
						},
					},
				},
			},
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "any-namespace",
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
			},
			expected: true,
		},
		{
			name:           "route_no_hostnames_matches_any_listener",
			controllerName: "test-controller",
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
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.HTTPRouteSpec{
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
			name:           "multi_level_subdomain_matches_wildcard",
			controllerName: "test-controller",
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
			route: &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: gatewayv1.HTTPRouteSpec{
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
				gcName := string(tt.gateway.Spec.GatewayClassName)
				// GatewayClass "cloudflare-tunnel" is managed by our controller;
				// any other class gets a different controllerName.
				gcControllerName := gatewayv1.GatewayController(tt.controllerName)
				if gcName != "cloudflare-tunnel" {
					gcControllerName = "other-controller"
				}
				builder = builder.WithObjects(&gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{Name: gcName},
					Spec:       gatewayv1.GatewayClassSpec{ControllerName: gcControllerName},
				})
			}
			if tt.route != nil {
				builder = builder.WithObjects(tt.route)
			}
			fakeClient := builder.Build()

			r := &HTTPRouteReconciler{
				Client:           fakeClient,
				Scheme:           scheme,
				ControllerName:   tt.controllerName,
				bindingValidator: routebinding.NewValidator(fakeClient),
			}

			result := r.isRouteForOurGateway(context.Background(), tt.route)

			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestHTTPRouteReconciler_FindRoutesForGateway(t *testing.T) {
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
					Name:     "http",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
				},
			},
		},
	}

	route1 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-1",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
		},
	}

	route2 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-2",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "other-gateway"},
				},
			},
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route1, route2).
		Build()

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	requests := r.findRoutesForGateway(context.Background(), gateway)

	require.Len(t, requests, 1)
	assert.Equal(t, "route-1", requests[0].Name)
	assert.Equal(t, "default", requests[0].Namespace)
}

func TestHTTPRouteReconciler_FindRoutesForGateway_WrongType(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	// Pass wrong type
	wrongType := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
	}

	requests := r.findRoutesForGateway(context.Background(), wrongType)

	assert.Nil(t, requests)
}

func TestHTTPRouteReconciler_FindRoutesForGateway_WrongClass(t *testing.T) {
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
					Name:     "http",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gateway).
		Build()

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	requests := r.findRoutesForGateway(context.Background(), gateway)

	assert.Nil(t, requests)
}

func TestHTTPRouteReconciler_GetAllRelevantRoutes(t *testing.T) {
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
					Name:     "http",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
				},
			},
		},
	}

	route1 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-1",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
		},
	}

	route2 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-2",
			Namespace: "other-namespace",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "other-gateway"},
				},
			},
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route1, route2).
		Build()

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	requests := r.getAllRelevantRoutes(context.Background())

	require.Len(t, requests, 1)
	assert.Equal(t, "route-1", requests[0].Name)
}

func TestHTTPRouteReconciler_Start(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}

	// Verify startupComplete is false before Start
	assert.False(t, r.startupComplete.Load())

	// Start will try to sync and fail (no GatewayClass), but should still complete
	err := r.Start(context.Background())

	assert.NoError(t, err)
	// Verify startupComplete is true after Start
	assert.True(t, r.startupComplete.Load())
}

func TestHTTPRouteReconciler_Constants(t *testing.T) {
	t.Parallel()

	// Verify important constants
	assert.Equal(t, "Gateway", kindGateway)
	assert.Equal(t, 1000, maxIngressRules)
}

func TestHTTPRouteReconciler_UpdateRouteStatus_Integration(t *testing.T) {
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
					Name:     "http",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
				},
			},
		},
	}

	route := &gatewayv1.HTTPRoute{
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
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route).
		WithStatusSubresource(route).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
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

	// Verify status was updated
	var updatedRoute gatewayv1.HTTPRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "test-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)

	require.Len(t, updatedRoute.Status.Parents, 1)
	assert.Equal(t, gatewayv1.GatewayController("test-controller"), updatedRoute.Status.Parents[0].ControllerName)
	require.Len(t, updatedRoute.Status.Parents[0].Conditions, 2)

	// Find Accepted condition
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

func TestHTTPRouteReconciler_UpdateRouteStatus_NotAccepted(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
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
				},
			},
		},
	}

	route := &gatewayv1.HTTPRoute{
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
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route).
		WithStatusSubresource(route).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
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

	// Verify status was updated
	var updatedRoute gatewayv1.HTTPRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "test-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)

	require.Len(t, updatedRoute.Status.Parents, 1)

	// Find Accepted condition
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

func TestHTTPRouteReconciler_MapperIntegration(t *testing.T) {
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
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: "gateway.cloudflare-tunnel.io",
				Kind:  "GatewayClassConfig",
				Name:  "test-config",
			},
		},
	}

	gatewayClassConfig := &v1alpha1.GatewayClassConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-config",
		},
		Spec: v1alpha1.GatewayClassConfigSpec{
			TunnelID: "test-tunnel-id",
			CloudflareCredentialsSecretRef: v1alpha1.SecretReference{
				Name:      "cf-credentials",
				Namespace: "default",
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
				},
			},
		},
	}

	route := &gatewayv1.HTTPRoute{
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
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gatewayClassConfig, gateway, route).
		Build()

	resolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", resolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}

	mapper := &ConfigMapper{
		Client:         fakeClient,
		ControllerName: "test-controller",
		ConfigResolver: resolver,
	}

	// Test that config mapper returns relevant routes
	requests := mapper.MapConfigToRequests(r.getAllRelevantRoutes)(context.Background(), gatewayClassConfig)

	require.Len(t, requests, 1)
	assert.Equal(t, "test-route", requests[0].Name)
	assert.Equal(t, "default", requests[0].Namespace)
}

func TestHTTPRouteReconciler_SyncAndUpdateStatus_NoConfig(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	// Create gateway class without config
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cloudflare-tunnel",
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "cloudflare-tunnel-controller",
			// No parametersRef
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}

	result, err := r.syncAndUpdateStatus(context.Background())

	// Should requeue due to config resolution error
	assert.NoError(t, err)
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)
}

func TestHTTPRouteReconciler_GetAllRelevantRoutes_Empty(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
	}

	requests := r.getAllRelevantRoutes(context.Background())

	assert.Empty(t, requests)
}

func TestHTTPRouteReconciler_UpdateRouteStatus_WithSyncError(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}

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

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test-route",
			Namespace:  "default",
			Generation: 2,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route).
		WithStatusSubresource(route).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}

	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: true, Reason: gatewayv1.RouteReasonAccepted},
		},
	}

	syncErr := assert.AnError
	err := r.updateRouteStatus(context.Background(), route, bindingInfo, nil, syncErr)
	require.NoError(t, err)

	var updatedRoute gatewayv1.HTTPRoute
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
	assert.Equal(t, string(gatewayv1.RouteReasonPending), acceptedCondition.Reason)
}

func TestHTTPRouteReconciler_UpdateRouteStatus_WithFailedBackendRefs(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}

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

	route := &gatewayv1.HTTPRoute{
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
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route).
		WithStatusSubresource(route).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}

	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: true, Reason: gatewayv1.RouteReasonAccepted},
		},
	}

	failedRefs := []ingress.BackendRefError{
		{
			RouteNamespace: "default",
			RouteName:      "test-route",
			BackendName:    "backend-svc",
			BackendNS:      "other-ns",
			Reason:         "RefNotPermitted",
			Message:        "cross-namespace reference not allowed",
		},
	}

	err := r.updateRouteStatus(context.Background(), route, bindingInfo, failedRefs, nil)
	require.NoError(t, err)

	var updatedRoute gatewayv1.HTTPRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "test-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)

	require.Len(t, updatedRoute.Status.Parents, 1)

	// Find ResolvedRefs condition
	var resolvedRefsCondition *metav1.Condition
	for i := range updatedRoute.Status.Parents[0].Conditions {
		if updatedRoute.Status.Parents[0].Conditions[i].Type == string(gatewayv1.RouteConditionResolvedRefs) {
			resolvedRefsCondition = &updatedRoute.Status.Parents[0].Conditions[i]

			break
		}
	}

	require.NotNil(t, resolvedRefsCondition)
	assert.Equal(t, metav1.ConditionFalse, resolvedRefsCondition.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonRefNotPermitted), resolvedRefsCondition.Reason)
	assert.Contains(t, resolvedRefsCondition.Message, "other-ns/backend-svc")
}

func TestHTTPRouteReconciler_UpdateRouteStatus_MultipleParents(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}

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

	otherGateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-gateway",
			Namespace: "default",
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "other-class",
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "multi-parent-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
					{Name: "other-gateway"},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, otherGateway, route).
		WithStatusSubresource(route).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}

	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: true, Reason: gatewayv1.RouteReasonAccepted},
		},
	}

	err := r.updateRouteStatus(context.Background(), route, bindingInfo, nil, nil)
	require.NoError(t, err)

	var updatedRoute gatewayv1.HTTPRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "multi-parent-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)

	// Only one parent status should be set (the one matching our GatewayClassName)
	require.Len(t, updatedRoute.Status.Parents, 1)
	assert.Equal(t, gatewayv1.ObjectName("test-gateway"), updatedRoute.Status.Parents[0].ParentRef.Name)
}

func TestHTTPRouteReconciler_UpdateRouteStatus_NonGatewayParentRef(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	svcKind := gatewayv1.Kind("Service")

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "svc-parent-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name: "some-service",
						Kind: &svcKind,
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
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}

	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{},
	}

	err := r.updateRouteStatus(context.Background(), route, bindingInfo, nil, nil)
	require.NoError(t, err)

	var updatedRoute gatewayv1.HTTPRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "svc-parent-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)

	// No parent statuses should be set for non-Gateway parent refs
	assert.Empty(t, updatedRoute.Status.Parents)
}

func TestHTTPRouteReconciler_FindRoutesForReferenceGrant_CrossNs(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, gatewayv1beta1.Install(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}

	backendNs := gatewayv1.Namespace("backend-ns")

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

	// Route with cross-namespace backend ref
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cross-ns-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name:      "svc",
									Namespace: &backendNs,
								},
							},
						},
					},
				},
			},
		},
	}

	// Route without cross-namespace ref
	localRoute := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "local-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "test-gateway"},
				},
			},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
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
					Kind:      "HTTPRoute",
					Namespace: "default",
				},
			},
			To: []gatewayv1beta1.ReferenceGrantTo{
				{
					Group: "",
					Kind:  "Service",
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route, localRoute, refGrant).
		Build()

	r := &HTTPRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ControllerName:   "test-controller",
		bindingValidator: routebinding.NewValidator(fakeClient),
	}

	requests := r.findRoutesForReferenceGrant(context.Background(), refGrant)

	// Only the cross-namespace route should be returned
	require.Len(t, requests, 1)
	assert.Equal(t, "cross-ns-route", requests[0].Name)
}

func TestHTTPRouteReconciler_FindRoutesForReferenceGrant_WrongType(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, gatewayv1beta1.Install(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &HTTPRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ControllerName:   "test-controller",
		bindingValidator: routebinding.NewValidator(fakeClient),
	}

	// Pass wrong type
	wrongObj := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "not-a-grant", Namespace: "default"},
	}

	requests := r.findRoutesForReferenceGrant(context.Background(), wrongObj)
	assert.Nil(t, requests)
}

func TestHTTPRouteReconciler_UpdateRouteStatus_ParentRefWithExplicitNamespace(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
	}

	gateway := &gatewayv1.Gateway{
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
							From: new(gatewayv1.NamespacesFromAll),
						},
					},
				},
			},
		},
	}

	gwNs := gatewayv1.Namespace("gateway-ns")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cross-ns-route",
			Namespace: "route-ns",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Name:      "test-gateway",
						Namespace: &gwNs,
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route).
		WithStatusSubresource(route).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:         fakeClient,
		Scheme:         scheme,
		ControllerName: "test-controller",
		RouteSyncer:    routeSyncer,
	}

	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: true, Reason: gatewayv1.RouteReasonAccepted},
		},
	}

	err := r.updateRouteStatus(context.Background(), route, bindingInfo, nil, nil)
	require.NoError(t, err)

	var updatedRoute gatewayv1.HTTPRoute
	err = fakeClient.Get(context.Background(), client.ObjectKey{Name: "cross-ns-route", Namespace: "route-ns"}, &updatedRoute)
	require.NoError(t, err)

	require.Len(t, updatedRoute.Status.Parents, 1)
	assert.Equal(t, gatewayv1.ObjectName("test-gateway"), updatedRoute.Status.Parents[0].ParentRef.Name)
	require.NotNil(t, updatedRoute.Status.Parents[0].ParentRef.Namespace)
	assert.Equal(t, gatewayv1.Namespace("gateway-ns"), *updatedRoute.Status.Parents[0].ParentRef.Namespace)
}

func TestHTTPRouteReconciler_SyncAndUpdateStatus_WithRoutesAndConfigFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	allNamespaces := gatewayv1.NamespacesFromAll

	// GatewayClass with parametersRef pointing to nonexistent config
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

	httpRoute := &gatewayv1.HTTPRoute{
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
			Hostnames: []gatewayv1.Hostname{"example.com"},
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, gatewayv1beta1.Install(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, httpRoute).
		WithStatusSubresource(httpRoute).
		Build()

	configResolver := config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector())
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
		bindingValidator: routebinding.NewValidator(fakeClient),
	}
	r.startupComplete.Store(true)

	// Reconcile the existing route - triggers syncAndUpdateStatus
	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-route",
			Namespace: "default",
		},
	})

	// Config resolution fails, so we get requeue with delay but no error
	assert.NoError(t, err)
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)

	// Verify route status was updated with Pending condition (sync error)
	var updatedRoute gatewayv1.HTTPRoute
	err = fakeClient.Get(ctx, client.ObjectKey{Name: "test-route", Namespace: "default"}, &updatedRoute)
	require.NoError(t, err)

	// Route should have status parents set (at least one parent)
	require.NotEmpty(t, updatedRoute.Status.Parents)
}

func TestHTTPRouteReconciler_SyncAndUpdateStatus_MultipleRoutes(t *testing.T) {
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

	route1 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-one",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "my-gateway"},
				},
			},
			Hostnames: []gatewayv1.Hostname{"one.example.com"},
		},
	}

	route2 := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "route-two",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Name: "my-gateway"},
				},
			},
			Hostnames: []gatewayv1.Hostname{"two.example.com"},
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
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
		bindingValidator: routebinding.NewValidator(fakeClient),
	}
	r.startupComplete.Store(true)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "route-one",
			Namespace: "default",
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, apiErrorRequeueDelay, result.RequeueAfter)

	// Both routes should have had their status updated
	var updated1 gatewayv1.HTTPRoute
	err = fakeClient.Get(ctx, client.ObjectKey{Name: "route-one", Namespace: "default"}, &updated1)
	require.NoError(t, err)
	require.NotEmpty(t, updated1.Status.Parents)

	var updated2 gatewayv1.HTTPRoute
	err = fakeClient.Get(ctx, client.ObjectKey{Name: "route-two", Namespace: "default"}, &updated2)
	require.NoError(t, err)
	require.NotEmpty(t, updated2.Status.Parents)
}

func TestHTTPRouteReconciler_Reconcile_RouteNotForOurGateway(t *testing.T) {
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

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-route",
			Namespace: "default",
		},
		Spec: gatewayv1.HTTPRouteSpec{
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
	routeSyncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "test-controller", configResolver, cfmetrics.NewNoopCollector(), nil)

	r := &HTTPRouteReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		ControllerName:   "test-controller",
		RouteSyncer:      routeSyncer,
		bindingValidator: routebinding.NewValidator(fakeClient),
	}
	r.startupComplete.Store(true)

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "other-route",
			Namespace: "default",
		},
	})

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}
