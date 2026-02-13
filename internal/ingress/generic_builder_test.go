package ingress_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
)

// TestGenericBuilder_HTTPRoute tests that GenericBuilder works with HTTPRoute.
func TestGenericBuilder_HTTPRoute(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGenericBuilder[gatewayv1.HTTPRoute](
		"cluster.local",
		nil, nil, nil, nil,
		ingress.HTTPRouteAdapter{},
	)

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("my-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	result := builder.Build(context.Background(), routes)

	require.Len(t, result.Rules, 2)
	assert.Equal(t, "app.example.com", result.Rules[0].Hostname.Value)
	assert.Equal(t, "http://my-service.default.svc.cluster.local:8080", result.Rules[0].Service.Value)
	assert.Equal(t, ingress.CatchAllService, result.Rules[1].Service.Value)
}

// TestGenericBuilder_GRPCRoute tests that GenericBuilder works with GRPCRoute.
func TestGenericBuilder_GRPCRoute(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGenericBuilder[gatewayv1.GRPCRoute](
		"cluster.local",
		nil, nil, nil, nil,
		ingress.GRPCRouteAdapter{},
	)

	routes := []gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-grpc-route",
				Namespace: "default",
			},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							newGRPCBackendRef("grpc-service", nil, int32Ptr(9090)),
						},
					},
				},
			},
		},
	}

	result := builder.Build(context.Background(), routes)

	// GRPCBuilder doesn't add catch-all, so only 1 rule
	require.Len(t, result.Rules, 1)
	assert.Equal(t, "grpc.example.com", result.Rules[0].Hostname.Value)
	assert.Equal(t, "http://grpc-service.default.svc.cluster.local:9090", result.Rules[0].Service.Value)
}

// TestGenericBuilder_WildcardOrdering tests that wildcard hostnames are sorted last.
func TestGenericBuilder_WildcardOrdering(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGenericBuilder[gatewayv1.HTTPRoute](
		"cluster.local",
		nil, nil, nil, nil,
		ingress.HTTPRouteAdapter{},
	)

	routes := []gatewayv1.HTTPRoute{
		// Wildcard route (empty hostnames = "*")
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "wildcard-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("wildcard-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
		// Specific hostname route
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "specific-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRef("specific-service", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	result := builder.Build(context.Background(), routes)

	// Order: specific hostname, wildcard (no hostname), catch-all
	require.Len(t, result.Rules, 3)
	assert.Equal(t, "app.example.com", result.Rules[0].Hostname.Value)
	// Wildcard hostname should NOT set Hostname field - omitting means "match all" in Cloudflare
	assert.Equal(t, "", result.Rules[1].Hostname.Value)
	assert.Equal(t, ingress.CatchAllService, result.Rules[2].Service.Value)
}

// TestGenericBuilder_MixedRoutes tests building with multiple routes.
func TestGenericBuilder_MixedRoutes(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGenericBuilder[gatewayv1.HTTPRoute](
		"cluster.local",
		nil, nil, nil, nil,
		ingress.HTTPRouteAdapter{},
	)

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route-z", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"z.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{BackendRefs: []gatewayv1.HTTPBackendRef{newHTTPBackendRef("z-svc", nil, int32Ptr(80))}},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route-a", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"a.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{BackendRefs: []gatewayv1.HTTPBackendRef{newHTTPBackendRef("a-svc", nil, int32Ptr(80))}},
				},
			},
		},
	}

	result := builder.Build(context.Background(), routes)

	require.Len(t, result.Rules, 3)
	assert.Equal(t, "a.example.com", result.Rules[0].Hostname.Value)
	assert.Equal(t, "z.example.com", result.Rules[1].Hostname.Value)
}

// TestGenericBuilder_EmptyRoutes tests building with no routes.
func TestGenericBuilder_EmptyRoutes(t *testing.T) {
	t.Parallel()

	builder := ingress.NewGenericBuilder[gatewayv1.HTTPRoute](
		"cluster.local",
		nil, nil, nil, nil,
		ingress.HTTPRouteAdapter{},
	)

	result := builder.Build(context.Background(), []gatewayv1.HTTPRoute{})

	require.Len(t, result.Rules, 1)
	assert.Equal(t, ingress.CatchAllService, result.Rules[0].Service.Value)
}
