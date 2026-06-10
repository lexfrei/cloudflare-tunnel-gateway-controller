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

	// Wildcard routes are skipped from Cloudflare config (handled by proxy).
	// Only specific hostname + catch-all remain.
	require.Len(t, result.Rules, 2)
	assert.Equal(t, "app.example.com", result.Rules[0].Hostname.Value)
	assert.Equal(t, ingress.CatchAllService, result.Rules[1].Service.Value)
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

// TestGenericBuilder_BackendResolutionEquivalentAcrossKinds pins #400's
// single-source guarantee: HTTPRoute and GRPCRoute backendRefs flow through
// ONE shared resolution path (projection to plain BackendRef + shared
// selection/validation), so equivalent inputs must yield equivalent ingress
// rules and identical failed-ref reporting modulo the route kind name.
func TestGenericBuilder_BackendResolutionEquivalentAcrossKinds(t *testing.T) {
	t.Parallel()

	lowWeight := int32(10)
	highWeight := int32(90)
	port := gatewayv1.PortNumber(8080)
	crossNS := gatewayv1.Namespace("other")

	httpBuilder := ingress.NewGenericBuilder[gatewayv1.HTTPRoute](
		"cluster.local", nil, nil, nil, nil, ingress.HTTPRouteAdapter{},
	)
	grpcBuilder := ingress.NewGenericBuilder[gatewayv1.GRPCRoute](
		"cluster.local", nil, nil, nil, nil, ingress.GRPCRouteAdapter{},
	)

	// Two weighted backends (highest wins) plus a cross-namespace ref that is
	// permitted (nil validator) — the same shape for both kinds.
	httpRefs := []gatewayv1.HTTPBackendRef{
		{BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-low", Port: &port},
			Weight:                 &lowWeight,
		}},
		{BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-high", Port: &port, Namespace: &crossNS},
			Weight:                 &highWeight,
		}},
	}
	grpcRefs := []gatewayv1.GRPCBackendRef{
		{BackendRef: httpRefs[0].BackendRef},
		{BackendRef: httpRefs[1].BackendRef},
	}

	httpRoutes := []gatewayv1.HTTPRoute{{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.example.com"},
			Rules:     []gatewayv1.HTTPRouteRule{{BackendRefs: httpRefs}},
		},
	}}
	grpcRoutes := []gatewayv1.GRPCRoute{{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.example.com"},
			Rules:     []gatewayv1.GRPCRouteRule{{BackendRefs: grpcRefs}},
		},
	}}

	httpResult := httpBuilder.Build(context.Background(), httpRoutes)
	grpcResult := grpcBuilder.Build(context.Background(), grpcRoutes)

	require.Empty(t, httpResult.FailedRefs)
	require.Empty(t, grpcResult.FailedRefs)

	// HTTP appends a catch-all rule; the route-derived rules before it must be
	// identical to the gRPC ones (same highest-weight backend URL, same host).
	require.NotEmpty(t, httpResult.Rules)
	require.NotEmpty(t, grpcResult.Rules)
	assert.Equal(t, grpcResult.Rules, httpResult.Rules[:len(httpResult.Rules)-1],
		"equivalent backendRefs must resolve identically through the shared path for both route kinds")
	assert.Contains(t, httpResult.Rules[0].Service.Value, "svc-high.other.svc.cluster.local",
		"the shared selection must pick the highest-weight backend")
}
