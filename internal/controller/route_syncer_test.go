package controller

import (
	"context"
	"testing"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/metrics"
)

// httpListener creates a standard HTTP listener for testing.
func httpListener() []gatewayv1.Listener {
	allNamespaces := gatewayv1.NamespacesFromAll

	return []gatewayv1.Listener{
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
	}
}

func TestRouteSyncer_GetRelevantHTTPRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		routes        []gatewayv1.HTTPRoute
		gateways      []gatewayv1.Gateway
		expectedCount int
	}{
		{
			name:          "no routes",
			routes:        nil,
			gateways:      nil,
			expectedCount: 0,
		},
		{
			name: "route for our gateway",
			routes: []gatewayv1.HTTPRoute{
				{
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
			},
			gateways: []gatewayv1.Gateway{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-gateway",
						Namespace: "default",
					},
					Spec: gatewayv1.GatewaySpec{
						GatewayClassName: "cloudflare-tunnel",
						Listeners:        httpListener(),
					},
				},
			},
			expectedCount: 1,
		},
		{
			name: "route for different gateway class",
			routes: []gatewayv1.HTTPRoute{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-route",
						Namespace: "default",
					},
					Spec: gatewayv1.HTTPRouteSpec{
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{
								{Name: "other-gateway"},
							},
						},
					},
				},
			},
			gateways: []gatewayv1.Gateway{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "other-gateway",
						Namespace: "default",
					},
					Spec: gatewayv1.GatewaySpec{
						GatewayClassName: "other-class",
						Listeners:        httpListener(),
					},
				},
			},
			expectedCount: 0,
		},
		{
			name: "route with explicit namespace",
			routes: []gatewayv1.HTTPRoute{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-route",
						Namespace: "route-ns",
					},
					Spec: gatewayv1.HTTPRouteSpec{
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
			},
			gateways: []gatewayv1.Gateway{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-gateway",
						Namespace: "gateway-ns",
					},
					Spec: gatewayv1.GatewaySpec{
						GatewayClassName: "cloudflare-tunnel",
						Listeners:        httpListener(),
					},
				},
			},
			expectedCount: 1,
		},
		{
			name: "route with non-gateway kind",
			routes: []gatewayv1.HTTPRoute{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-route",
						Namespace: "default",
					},
					Spec: gatewayv1.HTTPRouteSpec{
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{
								{
									Name: "test-service",
									Kind: ptr(gatewayv1.Kind("Service")),
								},
							},
						},
					},
				},
			},
			gateways:      nil,
			expectedCount: 0,
		},
		{
			name: "mixed routes - some for our gateway",
			routes: []gatewayv1.HTTPRoute{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "our-route",
						Namespace: "default",
					},
					Spec: gatewayv1.HTTPRouteSpec{
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{
								{Name: "our-gateway"},
							},
						},
					},
				},
				{
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
				},
			},
			gateways: []gatewayv1.Gateway{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "our-gateway",
						Namespace: "default",
					},
					Spec: gatewayv1.GatewaySpec{
						GatewayClassName: "cloudflare-tunnel",
						Listeners:        httpListener(),
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "other-gateway",
						Namespace: "default",
					},
					Spec: gatewayv1.GatewaySpec{
						GatewayClassName: "other-class",
						Listeners:        httpListener(),
					},
				},
			},
			expectedCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			require.NoError(t, gatewayv1.AddToScheme(scheme))
			require.NoError(t, v1alpha1.AddToScheme(scheme))

			builder := fake.NewClientBuilder().WithScheme(scheme)
			for i := range tt.routes {
				builder = builder.WithObjects(&tt.routes[i])
			}
			for i := range tt.gateways {
				builder = builder.WithObjects(&tt.gateways[i])
			}
			fakeClient := builder.Build()

			syncer := NewRouteSyncer(
				fakeClient,
				scheme,
				"cluster.local",
				"cloudflare-tunnel",
				config.NewResolver(fakeClient, "default", metrics.NewNoopCollector()),
				metrics.NewNoopCollector(),
				nil,
			)

			routes, _, err := syncer.getRelevantHTTPRoutes(context.Background())

			require.NoError(t, err)
			assert.Len(t, routes, tt.expectedCount)
		})
	}
}

func TestRouteSyncer_GetRelevantGRPCRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		routes        []gatewayv1.GRPCRoute
		gateways      []gatewayv1.Gateway
		expectedCount int
	}{
		{
			name:          "no routes",
			routes:        nil,
			gateways:      nil,
			expectedCount: 0,
		},
		{
			name: "route for our gateway",
			routes: []gatewayv1.GRPCRoute{
				{
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
			},
			gateways: []gatewayv1.Gateway{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-gateway",
						Namespace: "default",
					},
					Spec: gatewayv1.GatewaySpec{
						GatewayClassName: "cloudflare-tunnel",
						Listeners:        httpListener(),
					},
				},
			},
			expectedCount: 1,
		},
		{
			name: "route for different gateway class",
			routes: []gatewayv1.GRPCRoute{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-route",
						Namespace: "default",
					},
					Spec: gatewayv1.GRPCRouteSpec{
						CommonRouteSpec: gatewayv1.CommonRouteSpec{
							ParentRefs: []gatewayv1.ParentReference{
								{Name: "other-gateway"},
							},
						},
					},
				},
			},
			gateways: []gatewayv1.Gateway{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "other-gateway",
						Namespace: "default",
					},
					Spec: gatewayv1.GatewaySpec{
						GatewayClassName: "other-class",
						Listeners:        httpListener(),
					},
				},
			},
			expectedCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			require.NoError(t, gatewayv1.AddToScheme(scheme))
			require.NoError(t, v1alpha1.AddToScheme(scheme))

			builder := fake.NewClientBuilder().WithScheme(scheme)
			for i := range tt.routes {
				builder = builder.WithObjects(&tt.routes[i])
			}
			for i := range tt.gateways {
				builder = builder.WithObjects(&tt.gateways[i])
			}
			fakeClient := builder.Build()

			syncer := NewRouteSyncer(
				fakeClient,
				scheme,
				"cluster.local",
				"cloudflare-tunnel",
				config.NewResolver(fakeClient, "default", metrics.NewNoopCollector()),
				metrics.NewNoopCollector(),
				nil,
			)

			routes, _, err := syncer.getRelevantGRPCRoutes(context.Background())

			require.NoError(t, err)
			assert.Len(t, routes, tt.expectedCount)
		})
	}
}

func TestMergeAndSortRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		httpRules     []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress
		grpcRules     []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress
		expectedCount int
	}{
		{
			name:          "empty rules",
			httpRules:     nil,
			grpcRules:     nil,
			expectedCount: 0,
		},
		{
			name: "only http rules",
			httpRules: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{
					Hostname: cloudflare.String("example.com"),
					Service:  cloudflare.String("http://svc:80"),
				},
			},
			grpcRules:     nil,
			expectedCount: 1,
		},
		{
			name:      "only grpc rules",
			httpRules: nil,
			grpcRules: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{
					Hostname: cloudflare.String("grpc.example.com"),
					Service:  cloudflare.String("http://grpc-svc:50051"),
				},
			},
			expectedCount: 1,
		},
		{
			name: "both http and grpc rules",
			httpRules: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{
					Hostname: cloudflare.String("example.com"),
					Service:  cloudflare.String("http://svc:80"),
				},
			},
			grpcRules: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{
					Hostname: cloudflare.String("grpc.example.com"),
					Service:  cloudflare.String("http://grpc-svc:50051"),
				},
			},
			expectedCount: 2,
		},
		{
			name: "filters out catch-all from http",
			httpRules: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{
					Hostname: cloudflare.String("example.com"),
					Service:  cloudflare.String("http://svc:80"),
				},
				{
					Service: cloudflare.String("http_status:404"),
				},
			},
			grpcRules:     nil,
			expectedCount: 1,
		},
		{
			name:      "filters out catch-all from grpc",
			httpRules: nil,
			grpcRules: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{
					Hostname: cloudflare.String("grpc.example.com"),
					Service:  cloudflare.String("http://grpc-svc:50051"),
				},
				{
					Service: cloudflare.String("http_status:404"),
				},
			},
			expectedCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := mergeAndSortRules(tt.httpRules, tt.grpcRules)

			assert.Len(t, result, tt.expectedCount)
		})
	}
}

func TestFilterOutCatchAll(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		rules         []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress
		expectedCount int
	}{
		{
			name:          "empty rules",
			rules:         nil,
			expectedCount: 0,
		},
		{
			name: "no catch-all",
			rules: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{
					Hostname: cloudflare.String("example.com"),
					Service:  cloudflare.String("http://svc:80"),
				},
				{
					Hostname: cloudflare.String("api.example.com"),
					Service:  cloudflare.String("http://api:80"),
				},
			},
			expectedCount: 2,
		},
		{
			name: "with catch-all at end",
			rules: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{
					Hostname: cloudflare.String("example.com"),
					Service:  cloudflare.String("http://svc:80"),
				},
				{
					Service: cloudflare.String("http_status:404"),
				},
			},
			expectedCount: 1,
		},
		{
			name: "only catch-all",
			rules: []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
				{
					Service: cloudflare.String("http_status:404"),
				},
			},
			expectedCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := filterOutCatchAll(tt.rules)

			assert.Len(t, result, tt.expectedCount)

			for _, rule := range result {
				assert.NotNil(t, rule.Hostname, "filtered rules should have hostname")
			}
		})
	}
}

func TestNewRouteSyncer(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	resolver := config.NewResolver(fakeClient, "default", metrics.NewNoopCollector())

	syncer := NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel", resolver, metrics.NewNoopCollector(), nil)

	require.NotNil(t, syncer)
	assert.Equal(t, "cluster.local", syncer.ClusterDomain)
	assert.Equal(t, "cloudflare-tunnel", syncer.GatewayClassName)
	assert.NotNil(t, syncer.httpBuilder)
	assert.NotNil(t, syncer.grpcBuilder)
	assert.NotNil(t, syncer.Metrics)
}
