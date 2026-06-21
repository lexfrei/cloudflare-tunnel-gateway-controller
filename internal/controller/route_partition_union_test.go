package controller

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// gatewayv1HTTPRoutes builds single-element route slices for the union test.
func gatewayv1HTTPRoutes(name string) []gatewayv1.HTTPRoute {
	return []gatewayv1.HTTPRoute{{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}}
}

// TestUnionPartitionRoutes pins the same-tunnel union rule: Cloudflare load-
// balances each tunnel's requests across ALL its connectors, so every
// partition sharing a tunnel must receive the UNION of that tunnel's routes —
// otherwise a request landing on the "wrong" data plane's connector 404s
// nondeterministically. Partitions on distinct tunnels keep disjoint configs
// (that distinctness IS the isolation).
func TestUnionPartitionRoutes(t *testing.T) {
	t.Parallel()

	sharedTunnel := "shared-tunnel"
	tenantTunnel := "tenant-tunnel"

	partitions := []routePartition{
		{
			Key:        sharedPartitionKey,
			HTTPRoutes: gatewayv1HTTPRoutes("shared-r"),
		},
		{
			Key:        "default/same-tunnel-gw",
			PerGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: sharedTunnel}},
			HTTPRoutes: gatewayv1HTTPRoutes("same-tunnel-r"),
		},
		{
			Key:        "default/tenant-gw",
			PerGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: tenantTunnel}},
			HTTPRoutes: gatewayv1HTTPRoutes("tenant-r"),
		},
	}

	unioned := unionPartitionRoutes(partitions, sharedTunnel)
	require.Len(t, unioned, 3)

	byKey := map[string]routePartition{}
	for _, partition := range unioned {
		byKey[partition.Key] = partition
	}

	assert.ElementsMatch(t, []string{"shared-r", "same-tunnel-r"}, routeNames(byKey[sharedPartitionKey].HTTPRoutes),
		"shared partition shares its tunnel with same-tunnel-gw → union")
	assert.ElementsMatch(t, []string{"shared-r", "same-tunnel-r"}, routeNames(byKey["default/same-tunnel-gw"].HTTPRoutes))
	assert.ElementsMatch(t, []string{"tenant-r"}, routeNames(byKey["default/tenant-gw"].HTTPRoutes),
		"a distinct tunnel keeps its disjoint config — the isolation property")
}

// TestUnionPartitionRoutes_SameTunnelSiblingsShareReadOnlyBacking pins the
// zero-copy contract unionPartitionRoutes documents: same-tunnel partitions
// share ONE union backing slice rather than each getting its own copy. The
// slices are READ-ONLY downstream — an in-place sort/filter on one would
// corrupt the sibling. Run under -race: concurrent reads of the aliased slices
// mirror the real push (the edge load-balances a tunnel's requests across all
// its connectors, so each same-tunnel partition's config is pushed
// concurrently), so a future change that mutates a partition's slice in place
// on that path would data-race here. Switching to per-partition copies would
// break the identity check below, forcing a conscious decision.
func TestUnionPartitionRoutes_SameTunnelSiblingsShareReadOnlyBacking(t *testing.T) {
	t.Parallel()

	sharedTunnel := "shared-tunnel"

	partitions := []routePartition{
		{
			Key:        sharedPartitionKey,
			HTTPRoutes: gatewayv1HTTPRoutes("shared-r"),
		},
		{
			Key:        "default/same-tunnel-gw",
			PerGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: sharedTunnel}},
			HTTPRoutes: gatewayv1HTTPRoutes("same-tunnel-r"),
		},
	}

	unioned := unionPartitionRoutes(partitions, sharedTunnel)
	require.Len(t, unioned, 2)

	first, second := unioned[0].HTTPRoutes, unioned[1].HTTPRoutes
	require.NotEmpty(t, first)
	require.Len(t, second, len(first))

	// Identical element addresses prove both partitions point at one backing
	// slice — the deliberate aliasing.
	assert.Same(t, &first[0], &second[0], "same-tunnel partitions must share the union backing slice")

	var wg sync.WaitGroup

	for _, routes := range [][]gatewayv1.HTTPRoute{first, second} {
		wg.Add(1)

		go func(rs []gatewayv1.HTTPRoute) {
			defer wg.Done()

			for i := range rs {
				_ = rs[i].Name
			}
		}(routes)
	}

	wg.Wait()
}
