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

	mkRoute := func(ns, name string) gatewayv1.HTTPRoute {
		return gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	}

	// Two partitions on the SAME tunnel ("shared"): each carries one shared
	// route plus one of its own.
	partitions := []routePartition{
		{Key: sharedPartitionKey, HTTPRoutes: []gatewayv1.HTTPRoute{mkRoute("a", "common"), mkRoute("a", "only-shared")}},
		{Key: "ns/gw", HTTPRoutes: []gatewayv1.HTTPRoute{mkRoute("a", "common"), mkRoute("b", "only-gw")}},
	}

	// Proxy side.
	unioned := unionPartitionRoutes(partitions, "tunnel-1")
	proxySet := httpRouteKeys(unioned[0].HTTPRoutes)

	// Cloudflare side: both partitions share tunnel-1, so they form one group.
	group := tunnelGroup{partitions: []*routePartition{&partitions[0], &partitions[1]}}
	cfRoutes, _ := groupRoutes(&group)
	cfSet := httpRouteKeys(cfRoutes)

	assert.Equal(t, cfSet, proxySet,
		"the Cloudflare write and the proxy push must union same-tunnel routes identically")
	assert.Equal(t, []string{"a/common", "a/only-shared", "b/only-gw"}, cfSet,
		"the union is deduplicated by namespace/name")
}

func httpRouteKeys(routes []gatewayv1.HTTPRoute) []string {
	keys := make([]string, 0, len(routes))
	for i := range routes {
		keys = append(keys, routes[i].Namespace+"/"+routes[i].Name)
	}

	sort.Strings(keys)

	return keys
}
