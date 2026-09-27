package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

const collisionTunnel = "550e8400-e29b-41d4-a716-446655440000"

// TestTunnelSharedDiagnostics_InfraCollisionSurfacesPerRoute pins #488: two
// dedicated (infra) Gateways in different namespaces sharing one Cloudflare
// Tunnel collapse isolation, and that now surfaces a DiagnosticTunnelShared on
// each colliding Gateway's own routes (not just an ERROR log).
func TestTunnelSharedDiagnostics_InfraCollisionSurfacesPerRoute(t *testing.T) {
	t.Parallel()

	partitions := []routePartition{
		{Key: sharedPartitionKey, HTTPRoutes: []gatewayv1.HTTPRoute{*pushFallbackRoute("shared-r", "shared.example.com")}},
		{
			Key:        "team-a/gw",
			PerGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: collisionTunnel}},
			HTTPRoutes: []gatewayv1.HTTPRoute{*pushFallbackRoute("a-route", "a.example.com")},
		},
		{
			Key:        "team-b/gw",
			PerGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: collisionTunnel}},
			HTTPRoutes: []gatewayv1.HTTPRoute{*pushFallbackRoute("b-route", "b.example.com")},
		},
	}

	groups := buildTunnelGroups(&config.ResolvedConfig{TunnelID: "shared-class-tunnel"}, partitions)
	collisions := sharedInfraTunnelCollisions(groups)
	require.Len(t, collisions, 1, "two infra Gateways on one tunnel must collide")

	diags := tunnelSharedDiagnostics(collisions, partitions)
	require.Len(t, diags, 2, "one diagnostic per colliding infra Gateway's route")

	names := map[string]bool{}

	for _, diag := range diags {
		assert.Equal(t, proxy.DiagnosticTunnelShared, diag.Target)
		assert.Equal(t, reasonSharedAcrossNamespaces, diag.Reason)
		assert.Contains(t, diag.Message, collisionTunnel)
		names[diag.Name] = true
	}

	assert.True(t, names["a-route"] && names["b-route"], "both tenants' routes are flagged")
	assert.False(t, names["shared-r"], "the shared partition's route is not part of an infra+infra collision")
}

// TestTunnelSharedDiagnostics_SharedPlusInfraIsBenign pins the risky-vs-benign
// distinction: a dedicated Gateway sharing the CLASS tunnel (shared+infra) is
// a collapse the operator opted into, not a collision, so it surfaces nothing.
func TestTunnelSharedDiagnostics_SharedPlusInfraIsBenign(t *testing.T) {
	t.Parallel()

	partitions := []routePartition{
		{Key: sharedPartitionKey, HTTPRoutes: []gatewayv1.HTTPRoute{*pushFallbackRoute("shared-r", "shared.example.com")}},
		{
			Key:        "team-a/gw",
			PerGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: collisionTunnel}},
			HTTPRoutes: []gatewayv1.HTTPRoute{*pushFallbackRoute("a-route", "a.example.com")},
		},
	}

	// The shared partition resolves to the SAME tunnel as the lone infra Gateway.
	groups := buildTunnelGroups(&config.ResolvedConfig{TunnelID: collisionTunnel}, partitions)
	collisions := sharedInfraTunnelCollisions(groups)

	assert.Empty(t, collisions, "shared+infra on one tunnel is an opted-in collapse, not a collision")
	assert.Empty(t, tunnelSharedDiagnostics(collisions, partitions))
}

// TestTunnelSharedDiagnostics_ReasonNamesTheNamespaceScope pins the reason to
// the namespaces actually involved. Two dedicated Gateways in one namespace
// still union their routes across both planes, so they are still reported,
// but not as a cross-namespace share. A Gateway is reported across namespaces
// as soon as any Gateway it shares with lives elsewhere.
func TestTunnelSharedDiagnostics_ReasonNamesTheNamespaceScope(t *testing.T) {
	t.Parallel()

	infra := func(key, route string) routePartition {
		return routePartition{
			Key:        key,
			PerGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: collisionTunnel}},
			HTTPRoutes: []gatewayv1.HTTPRoute{*pushFallbackRoute(route, route+".example.com")},
		}
	}

	reasons := func(t *testing.T, partitions []routePartition) map[string]string {
		t.Helper()

		groups := buildTunnelGroups(&config.ResolvedConfig{TunnelID: "shared-class-tunnel"}, partitions)
		diags := tunnelSharedDiagnostics(sharedInfraTunnelCollisions(groups), partitions)

		byRoute := map[string]string{}
		for _, diag := range diags {
			byRoute[diag.Name] = diag.Reason
		}

		return byRoute
	}

	t.Run("same namespace", func(t *testing.T) {
		t.Parallel()

		got := reasons(t, []routePartition{infra("team-a/gw-1", "r1"), infra("team-a/gw-2", "r2")})

		assert.Equal(t, map[string]string{
			"r1": reasonSharedWithinNamespace,
			"r2": reasonSharedWithinNamespace,
		}, got)
	})

	t.Run("one namespace plus another", func(t *testing.T) {
		t.Parallel()

		got := reasons(t, []routePartition{
			infra("team-a/gw-1", "r1"), infra("team-a/gw-2", "r2"), infra("team-b/gw", "r3"),
		})

		assert.Equal(t, map[string]string{
			"r1": reasonSharedAcrossNamespaces,
			"r2": reasonSharedAcrossNamespaces,
			"r3": reasonSharedAcrossNamespaces,
		}, got)
	})
}
