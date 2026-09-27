package tunnelownership_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
)

// TestArbitrate_Vectors runs the shared semantic contract against the
// arbitration function. The route partitioner and the Gateway reconciler both
// call this, so a change here is a change to both layers at once.
func TestArbitrate_Vectors(t *testing.T) {
	t.Parallel()

	for _, vector := range tunnelownership.Vectors() {
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()

			rejected := tunnelownership.Arbitrate(vector.SharedTunnel, vector.AllowSharedTunnels, vector.Claims)

			keys := make([]string, 0, len(rejected))
			for key := range rejected {
				keys = append(keys, key)
			}

			assert.ElementsMatch(t, vector.WantRejected, keys,
				"the set of rejected claims must match the shared contract")
		})
	}
}

// TestArbitrate_RejectionNamesTheIncumbent pins that a rejection carries the
// Gateway it lost to. Without it the operator sees "rejected" with no way to
// find out by whom, and the Gateway status message would have nothing to say.
func TestArbitrate_RejectionNamesTheIncumbent(t *testing.T) {
	t.Parallel()

	const tunnelID = "22222222-2222-2222-2222-222222222222"

	claims := []tunnelownership.Claim{
		{Key: "team-a/gw", Namespace: "team-a", TunnelID: tunnelID, UID: "a", Proof: tunnelownership.ProofVerified},
		{Key: "team-b/gw", Namespace: "team-b", TunnelID: tunnelID, UID: "b", Proof: tunnelownership.ProofVerified},
	}
	claims[1].CreatedAt = claims[0].CreatedAt.Add(1)

	rejected := tunnelownership.Arbitrate("shared", false, claims)

	reason, ok := rejected["team-b/gw"]
	assert.True(t, ok, "the newcomer must be rejected")
	assert.Equal(t, "team-a/gw", reason.HeldBy,
		"the rejection must name the Gateway already serving the tunnel")
	assert.Equal(t, tunnelID, reason.TunnelID,
		"the rejection must name the contested tunnel")
}

// TestArbitrate_SharedTunnelRejectionIsAttributedToTheClass pins the shared
// case: no Gateway holds the class tunnel, so the rejection must say that
// rather than naming a peer that does not exist.
func TestArbitrate_SharedTunnelRejectionIsAttributedToTheClass(t *testing.T) {
	t.Parallel()

	const shared = "11111111-1111-1111-1111-111111111111"

	rejected := tunnelownership.Arbitrate(shared, false, []tunnelownership.Claim{
		{Key: "team-a/gw", Namespace: "team-a", TunnelID: shared, UID: "a", Proof: tunnelownership.ProofVerified},
	})

	reason, ok := rejected["team-a/gw"]
	assert.True(t, ok, "a Gateway claiming the class tunnel must be rejected")
	assert.Empty(t, reason.HeldBy, "no Gateway holds the class tunnel")
	assert.True(t, reason.IsClassTunnel, "the rejection must identify the class tunnel case")
}

// TestArbitrate_UnprovenRejectionNamesNoHolder pins that a claim refused for
// lack of proof says so, and names nobody: it lost to Cloudflare's answer, not
// to a neighbour, so there is no holder to report.
func TestArbitrate_UnprovenRejectionNamesNoHolder(t *testing.T) {
	t.Parallel()

	const tunnelID = "22222222-2222-2222-2222-222222222222"

	rejected := tunnelownership.Arbitrate("shared", false, []tunnelownership.Claim{
		{Key: "team-a/gw", Namespace: "team-a", TunnelID: tunnelID, UID: "a", Proof: tunnelownership.ProofVerified},
		{Key: "team-b/gw", Namespace: "team-b", TunnelID: tunnelID, UID: "b", Proof: tunnelownership.ProofRefuted},
	})

	reason, ok := rejected["team-b/gw"]
	assert.True(t, ok, "a refuted claim must be rejected")
	assert.True(t, reason.Unproven, "the rejection must identify the missing proof")
	assert.Empty(t, reason.HeldBy, "an unproven claim lost to no Gateway")
	assert.Equal(t, tunnelID, reason.TunnelID)
}
