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
		assert.Equal(t, routeReasonTunnelShared, diag.Reason)
		assert.Contains(t, diag.Message, collisionTunnel)
		names[diag.Name] = true
	}

	assert.True(t, names["a-route"] && names["b-route"], "both tenants' routes are flagged")
	assert.False(t, names["shared-r"], "the shared partition's route is not part of an infra+infra collision")
}

// TestTunnelSharedDiagnostics_SharedPlusInfraIsBenign pins the risky-vs-benign
// distinction: a dedicated Gateway sharing the CLASS tunnel (shared+infra) is
// the documented migration path, not a collision, so it surfaces nothing.
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

	assert.Empty(t, collisions, "shared+infra on one tunnel is the migration path, not a collision")
	assert.Empty(t, tunnelSharedDiagnostics(collisions, partitions))
}
