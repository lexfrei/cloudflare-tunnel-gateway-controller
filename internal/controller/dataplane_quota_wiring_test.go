package controller

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
)

// TestGatewayWatchIsRegisteredOnBothControllers pins the WIRING of the sibling
// watch, which no behavioural test reaches: a mapper can be exercised directly,
// but a Watches block that is simply deleted leaves every test green while the
// cap silently fails open again — a displaced Gateway keeps Accepted=True and a
// rendered plane until the informer's ten-hour resync.
//
// Source text rather than a live manager, because controller-runtime exposes no
// way to read a built controller's sources back. Whitespace is stripped so the
// pin survives reformatting and breaks only on removal.
func TestGatewayWatchIsRegisteredOnBothControllers(t *testing.T) {
	t.Parallel()

	for file, mapper := range map[string]string{
		"gateway_controller.go":       "r.namespaceDataPlaneSiblings",
		"gateway_infra_reconciler.go": "r.namespaceInfraGateways",
	} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			source, err := os.ReadFile(file)
			require.NoError(t, err)

			packed := strings.Join(strings.Fields(string(source)), "")

			watch := "Watches(&gatewayv1.Gateway{},handler.EnqueueRequestsFromMapFunc(" + mapper + "),"
			assert.Contains(t, packed, watch,
				"%s must watch sibling Gateways: without it a Gateway displaced by a sibling's "+
					"opt-in is never re-reconciled", file)

			// Asserted separately so a dropped predicate does not report itself
			// as a removed watch. Losing it costs no correctness, only a wakeup
			// per status write, and the two deserve different messages.
			assert.Contains(t, packed, watch+"builder.WithPredicates(predicate.GenerationChangedPredicate{}),",
				"%s must gate the sibling watch on generation, or this controller's own status "+
					"writes wake every opted-in Gateway in the namespace", file)
		})
	}
}

// TestQuotaTieBreakingMatchesTunnelArbitration pins the one thing the two rules
// share: oldest first, ties broken by UID. That comparator is written out twice,
// in two packages, and nothing else binds them, so drift would quietly falsify
// the promise the docs and the CRD godoc both make.
//
// Tie-breaking is ALL that is pinned, and the claims below carry no Advertised
// tunnel for that reason. Tunnel arbitration puts possession first — a Gateway
// already advertising a tunnel keeps it against an older claimant — while the
// cap has no possession term at all, so an older Gateway opting in later does
// displace a newer holder. The two therefore agree on ties and deliberately
// diverge everywhere possession applies; whether the cap SHOULD gain a
// possession term is #758.
func TestQuotaTieBreakingMatchesTunnelArbitration(t *testing.T) {
	t.Parallel()

	const tunnelID = "550e8400-e29b-41d4-a716-446655440000"

	epoch := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

	type contender struct {
		key     string
		ageRank int
		uid     string
	}

	vectors := map[string][]contender{
		"distinct ages": {{"one", 2, "u9"}, {"two", 0, "u5"}, {"three", 1, "u1"}},
		"equal ages break on UID": {
			{"one", 0, "u9"}, {"two", 0, "u1"}, {"three", 0, "u5"},
		},
		"equal ages, UID ordering is lexical not numeric": {
			{"one", 0, "u10"}, {"two", 0, "u9"}, {"three", 0, "u2"},
		},
	}

	for name, contenders := range vectors {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ownershipClaims := make([]tunnelownership.Claim, 0, len(contenders))
			quotaClaims := make([]dataPlaneClaim, 0, len(contenders))

			for _, c := range contenders {
				createdAt := epoch.Add(time.Duration(c.ageRank) * time.Hour)

				// Arbitrate only rejects ACROSS namespaces, and the cap only
				// counts WITHIN one, so the same vector has to be spelled both
				// ways. The ordering under test is the same either way.
				ownershipClaims = append(ownershipClaims, tunnelownership.Claim{
					Key: c.key, Namespace: c.key, TunnelID: tunnelID, CreatedAt: createdAt, UID: c.uid,
				})
				quotaClaims = append(quotaClaims, dataPlaneClaim{
					Key: c.key, Namespace: "tenant", CreatedAt: createdAt, UID: c.uid,
				})
			}

			rejected := tunnelownership.Arbitrate("", ownershipClaims)
			refused := overQuotaGateways(new(int32(1)), quotaClaims)

			var heldTunnel, heldSlot string

			for _, c := range contenders {
				if _, ok := rejected[c.key]; !ok {
					heldTunnel = c.key
				}

				if !refused[c.key] {
					heldSlot = c.key
				}
			}

			require.NotEmpty(t, heldTunnel, "arbitration must leave exactly one holder")
			assert.Equal(t, heldTunnel, heldSlot,
				"with no claim holding possession, both rules reduce to the shared comparator "+
					"and must order these claims identically")
		})
	}
}
