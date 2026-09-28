package controller

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// TestBuildParentStatus_DiagnosticsScopedToParentPartition pins that a
// partition's diagnostics land only on the parents in that partition. The
// route has one parent on a dedicated Gateway whose tunnel is shared and one
// parent on the shared plane whose push keeps failing: each parent entry
// carries its own partition's condition and not the other's.
func TestBuildParentStatus_DiagnosticsScopedToParentPartition(t *testing.T) {
	t.Parallel()

	binding := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{0: {Accepted: true}, 1: {Accepted: true}},
		parentGateways: map[int]string{0: "team-a/gw", 1: "sys/shared-gw"},
		parentPartitions: map[int]string{
			0: "team-a/gw",
			1: sharedPartitionKey,
		},
	}

	diagnostics := []proxy.RouteDiagnostic{
		{
			Namespace: "team-a", Name: "a-route", Partition: "team-a/gw",
			Target: proxy.DiagnosticTunnelShared, Reason: reasonSharedAcrossNamespaces, Message: "shares a tunnel",
		},
		{
			Namespace: "team-a", Name: "a-route", Partition: sharedPartitionKey,
			Target: proxy.DiagnosticProxyConfigPush, Reason: routeReasonProxyConfigPushFailed, Message: "push failed",
		},
		{
			Namespace: "team-a", Name: "a-route", Partition: "team-a/gw",
			Target: proxy.DiagnosticShadowed, Reason: routeReasonShadowed, Message: "shadowed on the dedicated plane",
		},
	}

	statusFor := func(refIdx int) gatewayv1.RouteParentStatus {
		return buildParentStatus(gatewayv1.ParentReference{Name: "gw"}, "team-a", "example.com/controller",
			1, metav1.Now(), binding, refIdx, nil, nil, nil, diagnostics, 1)
	}

	dedicated := statusFor(0)
	assert.NotNil(t, findCondition(dedicated.Conditions, routeConditionTunnelShared))
	assert.NotNil(t, findCondition(dedicated.Conditions, routeConditionShadowed))
	assert.Nil(t, findCondition(dedicated.Conditions, routeConditionProxyConfigPushed),
		"the shared plane's push failure must not appear on the dedicated parent")

	shared := statusFor(1)
	assert.NotNil(t, findCondition(shared.Conditions, routeConditionProxyConfigPushed))
	assert.Nil(t, findCondition(shared.Conditions, routeConditionTunnelShared),
		"the dedicated Gateway's shared tunnel must not appear on the shared-plane parent")
	assert.Nil(t, findCondition(shared.Conditions, routeConditionShadowed),
		"shadowing on the dedicated plane must not appear on the shared-plane parent")
}

// TestBuildParentStatus_SpecDiagnosticsReachEveryParent pins the other half:
// a diagnostic about the route's own spec holds for every parent, whichever
// partition reported it. A parent served from no partition, such as one on a
// broken dedicated Gateway, still carries it.
func TestBuildParentStatus_SpecDiagnosticsReachEveryParent(t *testing.T) {
	t.Parallel()

	binding := routeBindingInfo{
		bindingResults:   map[int]routebinding.BindingResult{0: {Accepted: true}, 1: {Accepted: true}},
		parentGateways:   map[int]string{0: "sys/shared-gw", 1: "team-a/broken"},
		parentPartitions: map[int]string{0: sharedPartitionKey},
	}

	diagnostics := []proxy.RouteDiagnostic{
		{
			Namespace: "team-a", Name: "a-route", Partition: sharedPartitionKey,
			Target: proxy.DiagnosticResolvedRefs, Reason: string(gatewayv1.RouteReasonUnsupportedProtocol),
			Message: "unsupported appProtocol",
		},
		{
			Namespace: "team-a", Name: "a-route", Partition: sharedPartitionKey, RuleIndex: 0,
			Target: proxy.DiagnosticAccepted, Reason: string(gatewayv1.RouteReasonUnsupportedValue),
			Message: "unsupported filter",
		},
	}

	for refIdx := range 2 {
		status := buildParentStatus(gatewayv1.ParentReference{Name: "gw"}, "team-a", "example.com/controller",
			1, metav1.Now(), binding, refIdx, nil, nil, nil, diagnostics, 2)

		resolved := findCondition(status.Conditions, string(gatewayv1.RouteConditionResolvedRefs))
		require.NotNil(t, resolved)
		assert.Equal(t, metav1.ConditionFalse, resolved.Status, "parent %d", refIdx)

		partial := findCondition(status.Conditions, string(gatewayv1.RouteConditionPartiallyInvalid))
		assert.NotNil(t, partial, "parent %d", refIdx)
	}
}

// TestAssignParentPartitions pins which partition each parent is served from:
// its own for a resolved dedicated Gateway, the shared one for any other
// Gateway, and none for a dedicated Gateway whose plane did not resolve.
func TestAssignParentPartitions(t *testing.T) {
	t.Parallel()

	bindings := map[string]routeBindingInfo{
		"team-a/r": {parentGateways: map[int]string{
			0: "team-a/dedicated",
			1: "sys/shared-gw",
			2: "team-a/broken",
		}},
	}

	infra := &infraGateways{
		resolved: map[string]*infraGateway{"team-a/dedicated": {}},
		broken:   map[string]bool{"team-a/broken": true},
	}

	assignParentPartitions(bindings, infra)

	assert.Equal(t, map[int]string{0: "team-a/dedicated", 1: sharedPartitionKey},
		bindings["team-a/r"].parentPartitions)
}

// TestTunnelSharedDiagnostics_StampPartition pins that a collision diagnostic
// names the partition it belongs to, which is what scopes it to that parent.
func TestTunnelSharedDiagnostics_StampPartition(t *testing.T) {
	t.Parallel()

	perGateway := &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: collisionTunnel}}
	partitions := []routePartition{
		{Key: "team-a/gw", PerGateway: perGateway, HTTPRoutes: []gatewayv1.HTTPRoute{*pushFallbackRoute("a-route", "a.example.com")}},
		{Key: "team-b/gw", PerGateway: perGateway, HTTPRoutes: []gatewayv1.HTTPRoute{*pushFallbackRoute("b-route", "b.example.com")}},
	}

	collisions := sharedInfraTunnelCollisions(buildTunnelGroups(&config.ResolvedConfig{TunnelID: "class-tunnel"}, partitions))
	diags := tunnelSharedDiagnostics(collisions, partitions)
	require.Len(t, diags, 2)

	for _, diag := range diags {
		assert.Equal(t, map[string]string{"a-route": "team-a/gw", "b-route": "team-b/gw"}[diag.Name], diag.Partition)
	}
}

// TestPushPartitionConfigs_StampsConverterDiagnostics pins that diagnostics
// produced while building a partition's config name that partition.
func TestPushPartitionConfigs_StampsConverterDiagnostics(t *testing.T) {
	t.Parallel()

	replica := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(replica.Close)

	ourHost := gatewayv1.Hostname("ours.example.com")
	cli := failingGetClient(t, &gatewayv1.Gateway{}, gatewayUnderClass("ours", "our-class", &ourHost))

	route := httpRouteTo()
	route.Spec.ParentRefs = parentRefsToGateways("ours")

	params := syncUpdateParams{
		routeSyncer:    &RouteSyncer{ClusterDomain: "cluster.local", Metrics: cfmetrics.NewNoopCollector()},
		proxySyncer:    NewProxySyncer("cluster.local", "", "", cli, slog.Default()),
		proxyEndpoints: []string{replica.URL + "/config"},
		pushProxy:      true,
	}

	syncResult := &SyncResult{
		Partitions: []routePartition{{Key: sharedPartitionKey, HTTPRoutes: []gatewayv1.HTTPRoute{*route}}},
	}

	diags, _ := pushPartitionConfigs(context.Background(), slog.Default(), &params, syncResult)
	require.NotEmpty(t, diags, "the unreadable parent is reported by the converter pass")

	for _, diag := range diags {
		assert.Equal(t, sharedPartitionKey, diag.Partition)
	}
}
