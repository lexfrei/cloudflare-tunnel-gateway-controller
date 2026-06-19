package controller

import (
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
