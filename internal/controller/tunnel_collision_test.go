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

// TestSharedTunnelCredentialDrops pins the credential-override-dropped warning:
// an infra Gateway sharing the class tunnel but resolving a DIFFERENT
// credential is reported; one resolving the SAME credential is not; and an
// infra+infra group (no shared partition) is left to the collision detector.
func TestSharedTunnelCredentialDrops(t *testing.T) {
	t.Parallel()

	classCreds := config.ResolvedConfig{TunnelID: "class-tunnel", APIToken: "class-token", AccountID: "acct-1"}
	shared := &routePartition{Key: sharedPartitionKey}

	infraOverride := &routePartition{
		Key:        "ns/gw-a",
		PerGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: "class-tunnel", APIToken: "tenant-token", AccountID: "acct-1"}},
	}
	infraSameCreds := &routePartition{
		Key:        "ns/gw-b",
		PerGateway: &config.PerGatewayConfig{ResolvedConfig: classCreds},
	}

	t.Run("infra override on the class tunnel is flagged", func(t *testing.T) {
		t.Parallel()

		groups := []tunnelGroup{{resolved: &classCreds, partitions: []*routePartition{shared, infraOverride}}}

		drops := sharedTunnelCredentialDrops(groups)
		require.Len(t, drops, 1)
		assert.Equal(t, "class-tunnel", drops[0].tunnelID)
		assert.Equal(t, "ns/gw-a", drops[0].gateway)
	})

	t.Run("infra inheriting the class credential is not flagged", func(t *testing.T) {
		t.Parallel()

		groups := []tunnelGroup{{resolved: &classCreds, partitions: []*routePartition{shared, infraSameCreds}}}

		assert.Empty(t, sharedTunnelCredentialDrops(groups))
	})

	t.Run("infra-only group is left to the collision detector", func(t *testing.T) {
		t.Parallel()

		groups := []tunnelGroup{{resolved: &classCreds, partitions: []*routePartition{infraOverride}}}

		assert.Empty(t, sharedTunnelCredentialDrops(groups))
	})
}
