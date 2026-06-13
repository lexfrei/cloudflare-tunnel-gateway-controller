package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// TestSharedInfraTunnelCollisions pins the cross-tenant-exposure detector: two
// DISTINCT dedicated Gateways on one tunnel are flagged (their routes union
// across tenants); a shared+infra group on one tunnel is the documented
// migration path and is NOT flagged; distinct tunnels are never flagged.
func TestSharedInfraTunnelCollisions(t *testing.T) {
	t.Parallel()

	infraA := &routePartition{Key: "ns/gw-a", PerGateway: &config.PerGatewayConfig{}}
	infraB := &routePartition{Key: "ns/gw-b", PerGateway: &config.PerGatewayConfig{}}
	shared := &routePartition{Key: sharedPartitionKey}

	t.Run("two distinct infra Gateways on one tunnel are flagged", func(t *testing.T) {
		t.Parallel()

		groups := []tunnelGroup{{
			resolved:   &config.ResolvedConfig{TunnelID: "shared-tunnel"},
			partitions: []*routePartition{infraA, infraB},
		}}

		collisions := sharedInfraTunnelCollisions(groups)
		require.Len(t, collisions, 1)
		assert.Equal(t, "shared-tunnel", collisions[0].tunnelID)
		assert.Equal(t, []string{"ns/gw-a", "ns/gw-b"}, collisions[0].gateways)
	})

	t.Run("shared plus one infra Gateway is NOT flagged (migration path)", func(t *testing.T) {
		t.Parallel()

		groups := []tunnelGroup{{
			resolved:   &config.ResolvedConfig{TunnelID: "class-tunnel"},
			partitions: []*routePartition{shared, infraA},
		}}

		assert.Empty(t, sharedInfraTunnelCollisions(groups))
	})

	t.Run("distinct tunnels are not flagged", func(t *testing.T) {
		t.Parallel()

		groups := []tunnelGroup{
			{resolved: &config.ResolvedConfig{TunnelID: "tunnel-a"}, partitions: []*routePartition{infraA}},
			{resolved: &config.ResolvedConfig{TunnelID: "tunnel-b"}, partitions: []*routePartition{infraB}},
		}

		assert.Empty(t, sharedInfraTunnelCollisions(groups))
	})
}
