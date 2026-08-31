package tunnelownership

import (
	"slices"
	"time"
)

// Shared fixture values for the vector table (hoisted for goconst).
const (
	vectorSharedTunnel = "11111111-1111-1111-1111-111111111111"
	vectorOwnedTunnel  = "22222222-2222-2222-2222-222222222222"
	vectorOtherTunnel  = "33333333-3333-3333-3333-333333333333"

	vectorTeamA = "team-a"
	vectorTeamB = "team-b"

	vectorGatewayA = "team-a/gw"
	vectorGatewayB = "team-b/gw"
)

// vectorEpoch anchors the relative creation times below. Only the ORDER
// matters to the rule; the absolute instant is arbitrary.
var vectorEpoch = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC) //nolint:gochecknoglobals // shared read-only fixture

// Vector is one shared semantic case for the tunnel-ownership rule — the
// contract three enforcement layers must never disagree about:
//
//   - the route partitioner, which decides whose config is built and pushed,
//   - the Gateway reconciler, which decides whose Accepted condition says so,
//     and
//   - the infra reconciler, which decides whose data plane is rendered.
//
// A divergence between them is the failure to prevent: a Gateway told it is
// Accepted while its routes are silently unprogrammed, or a refused Gateway
// whose proxy still connects to the contested tunnel. What actually prevents it
// is that all three call Arbitrate over one claim set from collectTunnelClaims;
// this table pins the decision they share, not each layer's use of it.
type Vector struct {
	Name string
	// SharedTunnel is the class tunnel every non-opted-in Gateway uses.
	SharedTunnel string
	Claims       []Claim
	// WantRejected lists the claim keys that must not be programmed.
	WantRejected []string
}

// Vectors returns the shared semantic contract for tunnel ownership. Extend
// the matching half below; ownership_test.go re-runs the whole table, so a new
// case is enforced against the decision function without further wiring.
func Vectors() []Vector {
	return slices.Concat(sharingVectors(), incumbencyVectors())
}

// sharingVectors covers who may serve a tunnel at all: distinct tunnels, the
// class tunnel, and a tenant sharing with itself.
func sharingVectors() []Vector {
	return []Vector{
		{
			Name:         "distinct tunnels coexist",
			SharedTunnel: vectorSharedTunnel,
			Claims: []Claim{
				claim(vectorGatewayA, vectorTeamA, vectorOwnedTunnel, 0),
				claim(vectorGatewayB, vectorTeamB, vectorOtherTunnel, 1),
			},
		},
		{
			// The attack: a second namespace names a tunnel that is already
			// serving someone else. The incumbent keeps it.
			Name:         "newcomer claiming another namespace's tunnel is rejected",
			SharedTunnel: vectorSharedTunnel,
			Claims: []Claim{
				claim(vectorGatewayA, vectorTeamA, vectorOwnedTunnel, 0),
				claim(vectorGatewayB, vectorTeamB, vectorOwnedTunnel, 1),
			},
			WantRejected: []string{vectorGatewayB},
		},
		{
			// Order of arrival in the list must not matter — only age does.
			Name:         "incumbent wins regardless of list order",
			SharedTunnel: vectorSharedTunnel,
			Claims: []Claim{
				claim(vectorGatewayB, vectorTeamB, vectorOwnedTunnel, 1),
				claim(vectorGatewayA, vectorTeamA, vectorOwnedTunnel, 0),
			},
			WantRejected: []string{vectorGatewayB},
		},
		{
			// Equal timestamps happen: k8s stamps at second granularity.
			// UID breaks the tie so both layers reach the same verdict.
			Name:         "equal creation times fall back to UID",
			SharedTunnel: vectorSharedTunnel,
			Claims: []Claim{
				{Key: vectorGatewayA, Namespace: vectorTeamA, TunnelID: vectorOwnedTunnel, CreatedAt: vectorEpoch, UID: "bbb"},
				{Key: vectorGatewayB, Namespace: vectorTeamB, TunnelID: vectorOwnedTunnel, CreatedAt: vectorEpoch, UID: "aaa"},
			},
			WantRejected: []string{vectorGatewayA},
		},
		{
			// One tenant pointing two of its own Gateways at one tunnel is
			// sharing with itself. No cross-tenant boundary is crossed, so
			// the rule stays out of it.
			Name:         "same namespace may share its own tunnel",
			SharedTunnel: vectorSharedTunnel,
			Claims: []Claim{
				claim("team-a/gw-1", vectorTeamA, vectorOwnedTunnel, 0),
				claim("team-a/gw-2", vectorTeamA, vectorOwnedTunnel, 1),
			},
		},
		{
			// The class tunnel is the operator's, not a tenant's: no
			// creation-time race can take it, however old the claimant.
			Name:         "the class tunnel always wins",
			SharedTunnel: vectorSharedTunnel,
			Claims: []Claim{
				claim(vectorGatewayA, vectorTeamA, vectorSharedTunnel, -1),
			},
			WantRejected: []string{vectorGatewayA},
		},
	}
}

// incumbencyVectors covers who wins a contested tunnel: possession first, then
// age, then UID.
func incumbencyVectors() []Vector {
	return []Vector{
		{
			// The eviction hazard: an attacker whose Gateway predates the
			// victim's retargets its token at the victim's tunnel. Age alone
			// would hand it the win and evict the legitimate holder, turning
			// this rule into a weapon. Possession decides instead.
			Name:         "retargeting at a held tunnel loses regardless of age",
			SharedTunnel: vectorSharedTunnel,
			Claims: []Claim{
				// Older, but advertising a DIFFERENT tunnel: it is retargeting.
				contender(vectorGatewayA, vectorTeamA, vectorOtherTunnel, 0),
				// Younger, but already serving the contested tunnel.
				contender(vectorGatewayB, vectorTeamB, vectorOwnedTunnel, 1),
			},
			WantRejected: []string{vectorGatewayA},
		},
		{
			// With nobody holding it yet, two first-time claims fall back to
			// age — the only signal available.
			Name:         "first-time claims still fall back to age",
			SharedTunnel: vectorSharedTunnel,
			Claims: []Claim{
				contender(vectorGatewayA, vectorTeamA, "", 0),
				contender(vectorGatewayB, vectorTeamB, "", 1),
			},
			WantRejected: []string{vectorGatewayB},
		},
		{
			Name:         "a lone claim is never rejected",
			SharedTunnel: vectorSharedTunnel,
			Claims: []Claim{
				claim(vectorGatewayA, vectorTeamA, vectorOwnedTunnel, 0),
			},
		},
	}
}

// contender builds a claim on the contested tunnel that also states which
// tunnel the Gateway currently advertises in its status — empty when it
// advertises none yet, which is what makes it a first-time claim rather than
// a holder.
func contender(key, namespace, advertised string, ageRank int) Claim {
	out := claim(key, namespace, vectorOwnedTunnel, ageRank)
	out.Advertised = advertised

	return out
}

// claim builds a vector claim whose age is ordered by ageRank: lower ranks are
// older, so rank 0 is the oldest.
func claim(key, namespace, tunnelID string, ageRank int) Claim {
	return Claim{
		Key:       key,
		Namespace: namespace,
		TunnelID:  tunnelID,
		CreatedAt: vectorEpoch.Add(time.Duration(ageRank) * time.Hour),
		UID:       key,
	}
}
