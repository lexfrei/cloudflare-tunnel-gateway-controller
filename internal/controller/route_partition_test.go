package controller

import (
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
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

// TestApplyTunnelOwnership_RejectsCrossNamespaceClaim pins the config half of
// the tunnel-ownership rule: a Gateway whose token names a tunnel another
// namespace already serves gets no partition, so its plane is never sent the
// incumbent's routes and its own routes never reach the incumbent's tunnel.
//
// The status half lives in the Gateway reconciler and runs the same arbitration
// over the same claim set, which is what keeps the two from disagreeing about
// who won.
func TestApplyTunnelOwnership_RejectsCrossNamespaceClaim(t *testing.T) {
	t.Parallel()

	const contested = "22222222-2222-2222-2222-222222222222"

	infra := &infraGateways{
		resolved: map[string]*infraGateway{
			"team-a/gw": {
				gateway:    gatewayWithAge("team-a", "gw", 0),
				perGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: contested}},
			},
			"team-b/gw": {
				gateway:    gatewayWithAge("team-b", "gw", 1),
				perGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: contested}},
			},
		},
		broken: map[string]bool{},
		// Pre-marked transient: the newcomer's config resolved, but a prior
		// blip left the mark. The refusal must clear it.
		transient: map[string]bool{"team-b/gw": true},
	}

	applyTunnelOwnership(infra, "11111111-1111-1111-1111-111111111111", false, claimsFrom(infra))

	assert.Contains(t, infra.rejected, "team-b/gw",
		"the newcomer must be rejected, not merged into the incumbent's tunnel")
	assert.NotContains(t, infra.rejected, "team-a/gw",
		"the incumbent keeps the tunnel it claimed first")
	assert.NotContains(t, infra.resolved, "team-b/gw",
		"a rejected Gateway must contribute no partition at all")
	assert.NotContains(t, infra.transientKeys(), "team-b/gw",
		"a refusal is a decision, not a blip: keeping the transient mark would retain "+
			"the push cache for a plane that will keep being refused")

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{partRoute("victim"), partRoute("attacker")},
		bindings: map[string]routeBindingInfo{
			"default/victim":   bindingOn("team-a/gw"),
			"default/attacker": bindingOn("team-b/gw"),
		},
	}

	partitions := partitionRoutes(httpResult, &grpcRouteResult{}, infra)

	for _, partition := range partitions {
		assert.NotContains(t, routeNames(partition.HTTPRoutes), "attacker",
			"the rejected Gateway's route must be served nowhere (partition %q)", partition.Key)

		if partition.Key == "team-a/gw" {
			assert.ElementsMatch(t, []string{"victim"}, routeNames(partition.HTTPRoutes),
				"the incumbent keeps serving its own routes and gains none of the newcomer's")
		}
	}
}

// TestApplyTunnelOwnership_ClassTunnelClaimIsRejected pins the shape that
// matters most: the class tunnel serves every non-opted-in Gateway, so a
// dedicated plane claiming it would receive the union of ALL shared routes.
func TestApplyTunnelOwnership_ClassTunnelClaimIsRejected(t *testing.T) {
	t.Parallel()

	const classTunnel = "11111111-1111-1111-1111-111111111111"

	infra := &infraGateways{
		resolved: map[string]*infraGateway{
			"team-a/gw": {
				gateway:    gatewayWithAge("team-a", "gw", 0),
				perGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: classTunnel}},
			},
		},
		broken:    map[string]bool{},
		transient: map[string]bool{},
	}

	applyTunnelOwnership(infra, classTunnel, false, claimsFrom(infra))

	rejection, ok := infra.rejected["team-a/gw"]
	assert.True(t, ok, "claiming the class tunnel must be rejected")
	assert.True(t, rejection.IsClassTunnel,
		"the rejection must identify the class-tunnel case so the status message can say so")
	assert.NotContains(t, infra.resolved, "team-a/gw")
}

// gatewayWithAge builds a Gateway whose creation timestamp is ordered by
// ageRank: lower is older, so rank 0 is the incumbent.
func gatewayWithAge(namespace, name string, ageRank int) gatewayv1.Gateway {
	return gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         namespace,
			Name:              name,
			UID:               types.UID(namespace + "/" + name),
			CreationTimestamp: metav1.NewTime(time.Date(2026, time.January, 1, ageRank, 0, 0, 0, time.UTC)),
		},
	}
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

// TestGatewaySyncError_RejectedClaimIsNamedOnTheRoute pins the tenant-facing
// half of a refused tunnel claim. Routes of a refused Gateway already fail
// closed by inheriting the broken set, but the generic broken-data-plane
// message sends the tenant looking for an outage that is not there. The route
// must say the tunnel claim was refused, so the tenant can read their own
// status and act instead of filing a ticket.
func TestGatewaySyncError_RejectedClaimIsNamedOnTheRoute(t *testing.T) {
	t.Parallel()

	const contested = "22222222-2222-2222-2222-222222222222"

	infra := &infraGateways{
		resolved:  map[string]*infraGateway{},
		broken:    map[string]bool{"team-b/gw": true},
		transient: map[string]bool{},
		rejected: map[string]tunnelownership.Rejection{
			"team-b/gw": {TunnelID: contested, HeldBy: "team-a/gw"},
		},
	}

	err := gatewaySyncError("team-b/gw", map[string]error{}, infra)

	require.Error(t, err, "a refused Gateway's routes must still fail closed")
	assert.Contains(t, err.Error(), contested,
		"the route error must name the tunnel the Gateway's own token claimed")
	assert.NotContains(t, err.Error(), "broken",
		"the generic broken-data-plane wording would send the tenant hunting an outage")
}

// TestTenantVisibleRejectionHidesTheIncumbent pins the audience split for a
// refused tunnel claim. The refused tenant reads their own Gateway and route
// status, so naming the Gateway that holds the tunnel would hand them the
// namespace and name of a neighbour they may know nothing about — the same
// class of cross-namespace disclosure this rule exists to stop, just smaller.
//
// The tenant is told what they need to act on (the tunnel is not theirs); the
// operator gets both sides, in the controller log only.
//
// The same messages must not blame the connector token either. A claim can come
// from the tunnel already advertised in status instead — which is what a Gateway
// migrating off the shared plane carries while its token Secret is still
// unreadable — and naming the token there describes a read that never happened.
func TestTenantVisibleRejectionHidesTheIncumbent(t *testing.T) {
	t.Parallel()

	const (
		contested = "22222222-2222-2222-2222-222222222222"
		incumbent = "victim-team/production-gw"
	)

	rejection := tunnelownership.Rejection{TunnelID: contested, HeldBy: incumbent}

	infra := &infraGateways{
		resolved:  map[string]*infraGateway{},
		broken:    map[string]bool{"attacker/gw": true},
		transient: map[string]bool{},
		rejected:  map[string]tunnelownership.Rejection{"attacker/gw": rejection},
	}

	routeErr := gatewaySyncError("attacker/gw", map[string]error{}, infra)
	require.Error(t, routeErr)

	for _, surface := range []string{routeErr.Error(), tunnelRejectionMessage(rejection)} {
		assert.NotContains(t, surface, incumbent,
			"a tenant-visible message must not name the Gateway holding the tunnel")
		assert.NotContains(t, surface, "victim-team",
			"a tenant-visible message must not name another tenant's namespace")
		assert.Contains(t, surface, contested,
			"the tenant still needs to know which tunnel their own Gateway claimed")
		assert.NotContains(t, surface, "connector token",
			"a claim can come from the advertised address, so the message must not blame the token")
	}
}

// TestTunnelRejectionMessageSurvivesConditionTruncation pins the length budget
// the refusal dedup depends on.
//
// isTunnelRefusalReported asks whether the stored condition CONTAINS the
// rendered message, and the stored form is truncateMessage("Refused: " + msg).
// Let either message grow past the budget and the substring is never found
// again: every requeue then re-emits the Error log and the Warning Event, at a
// rate the refused tenant chooses. The failure is silent — the refusal still
// works, it just becomes a log flood.
func TestTunnelRejectionMessageSurvivesConditionTruncation(t *testing.T) {
	t.Parallel()

	const uuid = "22222222-2222-2222-2222-222222222222"

	for _, rejection := range []tunnelownership.Rejection{
		{TunnelID: uuid},
		{TunnelID: uuid, IsClassTunnel: true},
	} {
		stored := refusedConditionPrefix + tunnelRejectionMessage(rejection)
		assert.LessOrEqual(t, len(stored), maxConditionMessageLength,
			"a rejection message that truncates breaks the dedup that keeps the Event from repeating")
	}
}

// TestTunnelRefusalErrorCarriesBothSentinels pins that a refusal answers to
// config.ErrInvalidParameters as well as to its own sentinel.
//
// errors.Mark does not carry the reference's unwrap chain, so the two marks are
// independent. Without the second one, a refusal reaching handleResolveError
// would look like a transient API failure on an opted-in Gateway and take the
// requeue-without-status branch, leaving the tenant no condition to read.
func TestTunnelRefusalErrorCarriesBothSentinels(t *testing.T) {
	t.Parallel()

	err := tunnelRefusalError(tunnelownership.Rejection{
		TunnelID: "22222222-2222-2222-2222-222222222222",
	})

	// errors.Is here is cockroachdb's, matching the consumers. A mark is
	// invisible to the standard library's Is, so assert.ErrorIs — which uses it
	// — reports false for an error the production branches match.
	assert.True(t, errors.Is(err, errTunnelClaimRefused),
		"the status writer keys the \"Refused:\" prefix on this sentinel")
	assert.True(t, errors.Is(err, config.ErrInvalidParameters),
		"a refusal is a deterministic spec problem, and every branch keyed on that must match it")
}

// TestApplyTunnelOwnership_OperatorOptInRestoresSharing pins the escape hatch:
// with AllowSharedTunnels set on the cluster-scoped GatewayClassConfig, the
// pre-existing merge behaviour returns unchanged. The field lives there and
// nowhere tenant-writable, so a tenant cannot grant it to themselves.
func TestApplyTunnelOwnership_OperatorOptInRestoresSharing(t *testing.T) {
	t.Parallel()

	const contested = "22222222-2222-2222-2222-222222222222"

	infra := &infraGateways{
		resolved: map[string]*infraGateway{
			"team-a/gw": {
				gateway:    gatewayWithAge("team-a", "gw", 0),
				perGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: contested}},
			},
			"team-b/gw": {
				gateway:    gatewayWithAge("team-b", "gw", 1),
				perGateway: &config.PerGatewayConfig{ResolvedConfig: config.ResolvedConfig{TunnelID: contested}},
			},
		},
		broken:    map[string]bool{},
		transient: map[string]bool{},
	}

	applyTunnelOwnership(infra, "11111111-1111-1111-1111-111111111111", true, claimsFrom(infra))

	assert.Empty(t, infra.rejected, "the opt-in must refuse nothing")
	assert.Len(t, infra.resolved, 2, "both Gateways keep their partitions and merge as before")
}

// claimsFrom derives the shared claim set from a test's infra fixture, so a
// unit test exercises applyTunnelOwnership with the same shape production
// builds via collectTunnelClaims.
func claimsFrom(infra *infraGateways) []tunnelownership.Claim {
	claims := make([]tunnelownership.Claim, 0, len(infra.resolved))

	for key, entry := range infra.resolved {
		claims = append(claims, tunnelownership.Claim{
			Key:        key,
			Namespace:  entry.gateway.Namespace,
			TunnelID:   entry.perGateway.TunnelID,
			CreatedAt:  entry.gateway.CreationTimestamp.Time,
			UID:        string(entry.gateway.UID),
			Advertised: advertisedTunnelID(&entry.gateway),
		})
	}

	return claims
}

// TestApplyDataPlaneQuota pins the route-side half of the cap: a Gateway past
// its namespace's limit loses its partition, so its routes are programmed
// NOWHERE — in particular they do not fall back to the shared plane, which
// would hand a refused tenant's hostnames to the data plane serving everyone
// else.
func TestApplyDataPlaneQuota(t *testing.T) {
	t.Parallel()

	newInfra := func() *infraGateways {
		return &infraGateways{
			resolved: map[string]*infraGateway{
				"team-a/old": {
					gateway:    gatewayWithAge("team-a", "old", 0),
					perGateway: &config.PerGatewayConfig{},
				},
				"team-a/new": {
					gateway:    gatewayWithAge("team-a", "new", 1),
					perGateway: &config.PerGatewayConfig{},
				},
			},
			broken:    map[string]bool{},
			transient: map[string]bool{},
		}
	}

	t.Run("the newest loses its partition and falls back nowhere", func(t *testing.T) {
		t.Parallel()

		infra := newInfra()
		applyDataPlaneQuota(infra, new(int32(1)), quotaClaimsFrom(infra))

		assert.NotContains(t, infra.resolved, "team-a/new", "a refused Gateway contributes no partition")
		assert.Contains(t, infra.resolved, "team-a/old", "the oldest keeps its plane")

		binding := routeBindingInfo{acceptedGateways: map[string]bool{"team-a/new": true}}
		assert.Empty(t, partitionKeysFor(binding, infra),
			"a route bound only to a refused Gateway must reach no partition, shared included")
	})

	t.Run("the route says the cap was hit, not that the plane is broken", func(t *testing.T) {
		t.Parallel()

		infra := newInfra()
		applyDataPlaneQuota(infra, new(int32(1)), quotaClaimsFrom(infra))

		routeErr := gatewaySyncError("team-a/new", map[string]error{}, infra)
		require.Error(t, routeErr)
		assert.Contains(t, routeErr.Error(), "dedicated data plane",
			"the generic broken-data-plane sentence would send the tenant hunting an outage")
		assert.NotContains(t, routeErr.Error(), "old",
			"a tenant-visible message must not name the Gateway holding the slot")
	})

	t.Run("the refusal clears a stale transient mark", func(t *testing.T) {
		t.Parallel()

		infra := newInfra()
		// Pre-marked transient: a prior blip left the mark, and the Gateway's
		// config resolves fine now. Keeping the mark would put the key in
		// SyncResult.TransientBrokenKeys, which retains the push cache and
		// requeues the sync on apiErrorRequeueDelay -- turning a permanent
		// capacity refusal into a retry loop for a plane that is never rendered.
		infra.transient["team-a/new"] = true

		applyDataPlaneQuota(infra, new(int32(1)), quotaClaimsFrom(infra))

		assert.NotContains(t, infra.transientKeys(), "team-a/new",
			"a cap is a decision, not a blip")
	})

	t.Run("a Gateway broken for its own reason keeps that reason", func(t *testing.T) {
		t.Parallel()

		// Over the cap AND unresolvable. Its Gateway condition says
		// InvalidParameters, because the status path fails on the config long
		// before it reaches the cap. Reporting "quota" on its routes would point
		// the tenant at a condition that never mentions the cap, and freeing a
		// slot would change nothing.
		infra := newInfra()
		delete(infra.resolved, "team-a/new")
		infra.broken["team-a/new"] = true

		applyDataPlaneQuota(infra, new(int32(1)), quotaClaimsFrom(infra, dataPlaneClaim{
			Key:       "team-a/new",
			Namespace: "team-a",
			CreatedAt: gatewayWithAge("team-a", "new", 1).CreationTimestamp.Time,
			UID:       "team-a/new",
		}))

		require.ErrorIs(t, gatewaySyncError("team-a/new", map[string]error{}, infra), errBrokenDataPlane)
	})

	t.Run("no cap refuses nothing", func(t *testing.T) {
		t.Parallel()

		infra := newInfra()
		applyDataPlaneQuota(infra, nil, quotaClaimsFrom(infra))

		assert.Len(t, infra.resolved, 2)
		assert.Empty(t, infra.broken)
	})
}

// quotaClaimsFrom derives claims the way production's collectDataPlaneClaims
// does: over EVERY opted-in Gateway, resolved or not, because counting only the
// resolved ones would let a tenant free a slot by breaking one of their own
// tokens. Broken Gateways carry no object in infraGateways, so the caller passes
// theirs.
func quotaClaimsFrom(infra *infraGateways, broken ...dataPlaneClaim) []dataPlaneClaim {
	claims := make([]dataPlaneClaim, 0, len(infra.resolved)+len(broken))

	for key, entry := range infra.resolved {
		claims = append(claims, dataPlaneClaim{
			Key:       key,
			Namespace: entry.gateway.Namespace,
			CreatedAt: entry.gateway.CreationTimestamp.Time,
			UID:       string(entry.gateway.UID),
		})
	}

	return append(claims, broken...)
}
