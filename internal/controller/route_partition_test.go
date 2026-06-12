package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

func partRoute(name string) gatewayv1.HTTPRoute {
	return gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
}

func partGRPCRoute(name string) gatewayv1.GRPCRoute {
	return gatewayv1.GRPCRoute{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
}

func bindingOn(gateways ...string) routeBindingInfo {
	accepted := make(map[string]bool, len(gateways))
	for _, gateway := range gateways {
		accepted[gateway] = true
	}

	return routeBindingInfo{acceptedGateways: accepted}
}

func testInfraGateways(keys ...string) *infraGateways {
	out := &infraGateways{resolved: make(map[string]*infraGateway, len(keys))}
	for _, key := range keys {
		out.resolved[key] = &infraGateway{perGateway: &config.PerGatewayConfig{}}
	}

	return out
}

// TestPartitionRoutes_CoreAssignment pins the isolation guarantee of the
// whole feature: a route lands in EXACTLY the partitions of the Gateways it
// is accepted on — an infra Gateway's partition, the shared partition, or
// both for multi-parent routes. The shared partition always exists (the
// chart-deployed plane must converge to empty config when no routes remain).
func TestPartitionRoutes_CoreAssignment(t *testing.T) {
	t.Parallel()

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{
			partRoute("shared-only"),
			partRoute("infra-only"),
			partRoute("both"),
		},
		bindings: map[string]routeBindingInfo{
			"default/shared-only": bindingOn("default/shared-gw"),
			"default/infra-only":  bindingOn("default/infra-gw"),
			"default/both":        bindingOn("default/shared-gw", "default/infra-gw"),
		},
	}
	grpcResult := &grpcRouteResult{
		accepted: []gatewayv1.GRPCRoute{partGRPCRoute("grpc-infra")},
		bindings: map[string]routeBindingInfo{
			"default/grpc-infra": bindingOn("default/infra-gw"),
		},
	}

	partitions := partitionRoutes(httpResult, grpcResult, testInfraGateways("default/infra-gw"))

	require.Len(t, partitions, 2)
	byKey := map[string]routePartition{}

	for _, partition := range partitions {
		byKey[partition.Key] = partition
	}

	shared := byKey[sharedPartitionKey]
	assert.ElementsMatch(t, []string{"shared-only", "both"}, routeNames(shared.HTTPRoutes))
	assert.Empty(t, shared.GRPCRoutes)

	infra := byKey["default/infra-gw"]
	assert.ElementsMatch(t, []string{"infra-only", "both"}, routeNames(infra.HTTPRoutes))
	assert.ElementsMatch(t, []string{"grpc-infra"}, grpcRouteNames(infra.GRPCRoutes))
}

func routeNames(routes []gatewayv1.HTTPRoute) []string {
	names := make([]string, 0, len(routes))
	for i := range routes {
		names = append(names, routes[i].Name)
	}

	return names
}

func grpcRouteNames(routes []gatewayv1.GRPCRoute) []string {
	names := make([]string, 0, len(routes))
	for i := range routes {
		names = append(names, routes[i].Name)
	}

	return names
}

// TestPartitionRoutes_NoInfraGatewaysIsSharedOnly pins back-compat: with no
// opted-in Gateways everything lands in exactly one shared partition.
func TestPartitionRoutes_NoInfraGatewaysIsSharedOnly(t *testing.T) {
	t.Parallel()

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{partRoute("a"), partRoute("b")},
		bindings: map[string]routeBindingInfo{
			"default/a": bindingOn("default/gw"),
			"default/b": bindingOn("default/gw"),
		},
	}

	partitions := partitionRoutes(httpResult, &grpcRouteResult{}, nil)

	require.Len(t, partitions, 1)
	assert.Equal(t, sharedPartitionKey, partitions[0].Key)
	assert.Len(t, partitions[0].HTTPRoutes, 2)
}

// TestPartitionRoutes_DeterministicOrder pins stable output ordering (shared
// first, then infra partitions sorted by key) so config versions and sync
// logs stay reproducible.
func TestPartitionRoutes_DeterministicOrder(t *testing.T) {
	t.Parallel()

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{partRoute("r")},
		bindings: map[string]routeBindingInfo{
			"default/r": bindingOn("default/zz-gw", "default/aa-gw"),
		},
	}

	partitions := partitionRoutes(httpResult, &grpcRouteResult{}, testInfraGateways("default/zz-gw", "default/aa-gw"))

	require.Len(t, partitions, 3)
	assert.Equal(t, sharedPartitionKey, partitions[0].Key)
	assert.Equal(t, "default/aa-gw", partitions[1].Key)
	assert.Equal(t, "default/zz-gw", partitions[2].Key)
}

// TestPartitionRoutes_BrokenInfraGatewayFailsClosed pins the fail-closed
// contract for an opted-in Gateway whose GatewayConfig did NOT resolve
// (deleted Secret, garbled token, dangling ref): its routes belong to NO
// partition — they must neither leak into the shared tunnel/proxy nor be
// served by a half-configured dedicated plane. The Gateway's own status
// (InvalidParameters) is the operator signal.
func TestPartitionRoutes_BrokenInfraGatewayFailsClosed(t *testing.T) {
	t.Parallel()

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{
			partRoute("broken-only"),
			partRoute("broken-and-shared"),
		},
		bindings: map[string]routeBindingInfo{
			"default/broken-only":       bindingOn("default/broken-gw"),
			"default/broken-and-shared": bindingOn("default/broken-gw", "default/shared-gw"),
		},
	}

	infra := testInfraGateways()
	infra.broken = map[string]bool{"default/broken-gw": true}

	partitions := partitionRoutes(httpResult, &grpcRouteResult{}, infra)

	for _, partition := range partitions {
		assert.NotContains(t, routeNames(partition.HTTPRoutes), "broken-only",
			"a route accepted ONLY on a broken opted-in Gateway must be served nowhere (partition %q)", partition.Key)
	}

	require.Len(t, partitions, 1, "a broken gateway contributes no partition")
	assert.ElementsMatch(t, []string{"broken-and-shared"}, routeNames(partitions[0].HTTPRoutes),
		"the multi-parent route keeps serving via its healthy shared parent only")
}

// TestPartitionRoutes_InfraOnlyRouteNeverLeaksToShared is the negative-space
// pin of the isolation guarantee: a route accepted ONLY on a dedicated
// Gateway must never appear in the shared partition (= the shared tunnel and
// the shared proxy never serve it).
func TestPartitionRoutes_InfraOnlyRouteNeverLeaksToShared(t *testing.T) {
	t.Parallel()

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{partRoute("tenant-route")},
		bindings: map[string]routeBindingInfo{
			"default/tenant-route": bindingOn("default/infra-gw"),
		},
	}

	partitions := partitionRoutes(httpResult, &grpcRouteResult{}, testInfraGateways("default/infra-gw"))

	for _, partition := range partitions {
		if partition.Key == sharedPartitionKey {
			assert.Empty(t, partition.HTTPRoutes, "tenant route leaked into the shared data plane")
		}
	}
}
