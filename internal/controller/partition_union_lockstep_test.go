package controller

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestPartitionUnion_CloudflareAndProxyAgree pins the lockstep contract: two
// independent union implementations — groupRoutes (the Cloudflare ingress
// write) and unionPartitionRoutes (the proxy config push) — MUST produce the
// same route set for partitions sharing one tunnel. If they drift, a route
// would be written to the edge document but missing from the proxy config (or
// vice versa), 404ing nondeterministically.
func TestPartitionUnion_CloudflareAndProxyAgree(t *testing.T) {
	t.Parallel()

	mkHTTP := func(ns, name string) gatewayv1.HTTPRoute {
		return gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}
	mkGRPC := func(ns, name string) gatewayv1.GRPCRoute {
		return gatewayv1.GRPCRoute{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}

	// Two partitions on the SAME tunnel: each carries one shared route plus one
	// of its own, for both HTTP and gRPC.
	partitions := []routePartition{
		{
			Key:        sharedPartitionKey,
			HTTPRoutes: []gatewayv1.HTTPRoute{mkHTTP("a", "common"), mkHTTP("a", "only-shared")},
			GRPCRoutes: []gatewayv1.GRPCRoute{mkGRPC("a", "g-common"), mkGRPC("a", "g-only-shared")},
		},
		{
			Key:        "ns/gw",
			HTTPRoutes: []gatewayv1.HTTPRoute{mkHTTP("a", "common"), mkHTTP("b", "only-gw")},
			GRPCRoutes: []gatewayv1.GRPCRoute{mkGRPC("a", "g-common"), mkGRPC("b", "g-only-gw")},
		},
	}

	// Proxy side.
	unioned := unionPartitionRoutes(partitions, "tunnel-1")
	proxyHTTP := httpRouteKeys(unioned[0].HTTPRoutes)
	proxyGRPC := grpcRouteKeys(unioned[0].GRPCRoutes)

	// Cloudflare side: both partitions share tunnel-1, so they form one group.
	group := tunnelGroup{partitions: []*routePartition{&partitions[0], &partitions[1]}}
	cfHTTP, cfGRPC := groupRoutes(&group)

	assert.Equal(t, httpRouteKeys(cfHTTP), proxyHTTP,
		"the Cloudflare write and the proxy push must union same-tunnel HTTP routes identically")
	assert.Equal(t, grpcRouteKeys(cfGRPC), proxyGRPC,
		"the Cloudflare write and the proxy push must union same-tunnel gRPC routes identically")
	assert.Equal(t, []string{"a/common", "a/only-shared", "b/only-gw"}, httpRouteKeys(cfHTTP),
		"the HTTP union is deduplicated by namespace/name")
	assert.Equal(t, []string{"a/g-common", "a/g-only-shared", "b/g-only-gw"}, grpcRouteKeys(cfGRPC),
		"the gRPC union is deduplicated by namespace/name")
}

// TestPartitionUnion_ThreePartitionsOneTunnel extends the lockstep contract to
// the N>=3 case: route_syncer tolerates several partitions (shared plus two
// dedicated Gateways) collapsing onto one tunnel and unions all of them. Both
// union implementations are N-correct by inspection, but a future early-exit
// in either would silently drop a route from one side — pin the 3-way union so
// that regression fails loudly.
func TestPartitionUnion_ThreePartitionsOneTunnel(t *testing.T) {
	t.Parallel()

	mkHTTP := func(ns, name string) gatewayv1.HTTPRoute {
		return gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}
	mkGRPC := func(ns, name string) gatewayv1.GRPCRoute {
		return gatewayv1.GRPCRoute{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}

	// Three partitions on the SAME tunnel: a shared one and two dedicated
	// Gateways, each carrying the common route plus one of its own.
	partitions := []routePartition{
		{
			Key:        sharedPartitionKey,
			HTTPRoutes: []gatewayv1.HTTPRoute{mkHTTP("a", "common"), mkHTTP("a", "only-shared")},
			GRPCRoutes: []gatewayv1.GRPCRoute{mkGRPC("a", "g-common"), mkGRPC("a", "g-only-shared")},
		},
		{
			Key:        "ns/gw-1",
			HTTPRoutes: []gatewayv1.HTTPRoute{mkHTTP("a", "common"), mkHTTP("b", "only-gw1")},
			GRPCRoutes: []gatewayv1.GRPCRoute{mkGRPC("a", "g-common"), mkGRPC("b", "g-only-gw1")},
		},
		{
			Key:        "ns/gw-2",
			HTTPRoutes: []gatewayv1.HTTPRoute{mkHTTP("a", "common"), mkHTTP("c", "only-gw2")},
			GRPCRoutes: []gatewayv1.GRPCRoute{mkGRPC("a", "g-common"), mkGRPC("c", "g-only-gw2")},
		},
	}

	unioned := unionPartitionRoutes(partitions, "tunnel-1")
	proxyHTTP := httpRouteKeys(unioned[0].HTTPRoutes)
	proxyGRPC := grpcRouteKeys(unioned[0].GRPCRoutes)

	group := tunnelGroup{partitions: []*routePartition{&partitions[0], &partitions[1], &partitions[2]}}
	cfHTTP, cfGRPC := groupRoutes(&group)

	assert.Equal(t, httpRouteKeys(cfHTTP), proxyHTTP,
		"the Cloudflare write and the proxy push must union three same-tunnel HTTP partitions identically")
	assert.Equal(t, grpcRouteKeys(cfGRPC), proxyGRPC,
		"the Cloudflare write and the proxy push must union three same-tunnel gRPC partitions identically")
	assert.Equal(t, []string{"a/common", "a/only-shared", "b/only-gw1", "c/only-gw2"}, httpRouteKeys(cfHTTP),
		"the 3-way HTTP union is deduplicated by namespace/name")
	assert.Equal(t, []string{"a/g-common", "a/g-only-shared", "b/g-only-gw1", "c/g-only-gw2"}, grpcRouteKeys(cfGRPC),
		"the 3-way gRPC union is deduplicated by namespace/name")
}

func grpcRouteKeys(routes []gatewayv1.GRPCRoute) []string {
	keys := make([]string, 0, len(routes))
	for i := range routes {
		keys = append(keys, routes[i].Namespace+"/"+routes[i].Name)
	}

	sort.Strings(keys)

	return keys
}

func httpRouteKeys(routes []gatewayv1.HTTPRoute) []string {
	keys := make([]string, 0, len(routes))
	for i := range routes {
		keys = append(keys, routes[i].Namespace+"/"+routes[i].Name)
	}

	sort.Strings(keys)

	return keys
}
