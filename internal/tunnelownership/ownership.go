// Package tunnelownership decides which Gateway may serve a Cloudflare tunnel
// when more than one claims it.
//
// A per-Gateway data plane takes its tunnel identity from a connector token in
// the Gateway's own namespace, and that token proves nothing: it is a base64
// JSON blob whose tunnel UUID any tenant can write. Tunnel UUIDs are not
// secret either — the controller publishes them in Gateway status as
// <id>.cfargotunnel.com so external-dns can consume them. Without arbitration,
// naming someone else's tunnel is enough to join their partition, which merges
// both parties' routes into one document and pushes the union to both parties'
// proxies.
//
// The rule: a tunnel belongs to the Gateway already serving it, and the class
// tunnel belongs to the operator. Age breaks ties only between claims of equal
// standing, because deciding by age alone would let a tenant whose Gateway
// predates the victim's retarget its token and evict the rightful holder.
// Claims from other namespaces are rejected outright rather than merged, so a
// rejected Gateway's routes are never programmed and its plane never receives
// a neighbour's config.
//
// Three layers enforce this and must never disagree: the route partitioner
// (whose config is built), the Gateway reconciler (whose status says so), and
// the infra reconciler (whose data plane is rendered). All three call
// Arbitrate over one shared claim set, and Vectors is their semantic
// contract.
package tunnelownership

import (
	"cmp"
	"slices"
	"time"
)

// Claim is one Gateway's assertion that it serves a tunnel.
type Claim struct {
	// Key identifies the Gateway as "namespace/name".
	Key       string
	Namespace string
	// TunnelID is parsed from the Gateway's connector token, and is therefore
	// an assertion rather than a fact.
	TunnelID string
	// CreatedAt is the Gateway's creation timestamp: the tie-breaker that
	// makes the incumbent win.
	CreatedAt time.Time
	// UID breaks ties between claims created within the same second, which
	// Kubernetes timestamp granularity makes ordinary.
	UID string

	// Advertised is the tunnel this Gateway currently publishes in its status
	// addresses, empty when it publishes none yet. It is what distinguishes
	// holding a tunnel from merely asking for it: a Gateway that already
	// serves a tunnel keeps it against any newcomer, however old the newcomer
	// is. Without this, an attacker whose Gateway predates the victim's could
	// retarget its token and evict the legitimate holder by age alone.
	Advertised string
}

// Rejection explains why a claim was refused, in the terms the Gateway's
// status message needs.
type Rejection struct {
	// TunnelID is the contested tunnel.
	TunnelID string
	// HeldBy names the Gateway that holds it, empty when the holder is the
	// GatewayClass rather than a Gateway.
	HeldBy string
	// IsClassTunnel reports that the claim collided with the class tunnel.
	IsClassTunnel bool
}

// Arbitrate returns the claims that must not be programmed, keyed by claim
// key. A claim is rejected when it names the class tunnel, or when a Gateway
// in a different namespace claimed the same tunnel earlier.
//
// Claims from one namespace never reject each other: a tenant pointing two of
// its own Gateways at one tunnel is sharing with itself, and no boundary is
// crossed. The input order does not affect the outcome.
func Arbitrate(sharedTunnelID string, claims []Claim) map[string]Rejection {
	rejected := make(map[string]Rejection)

	byTunnel := make(map[string][]Claim)

	for _, claim := range claims {
		if claim.TunnelID == "" {
			continue
		}

		if claim.TunnelID == sharedTunnelID {
			rejected[claim.Key] = Rejection{TunnelID: claim.TunnelID, IsClassTunnel: true}

			continue
		}

		byTunnel[claim.TunnelID] = append(byTunnel[claim.TunnelID], claim)
	}

	for tunnelID, contenders := range byTunnel {
		holder := incumbent(tunnelID, contenders)

		for _, claim := range contenders {
			if claim.Namespace == holder.Namespace {
				continue
			}

			rejected[claim.Key] = Rejection{TunnelID: tunnelID, HeldBy: holder.Key}
		}
	}

	return rejected
}

// incumbent returns the claim that holds the tunnel.
//
// Possession decides first: a Gateway already advertising this tunnel is
// serving it, and a retargeted token must not take it away. Age (then UID, for
// the equal timestamps Kubernetes second-granularity makes ordinary) decides
// only among claims with equal standing — first-time claims, or the several
// holders a previously-permitted sharing arrangement can leave behind.
func incumbent(tunnelID string, claims []Claim) Claim {
	holders := make([]Claim, 0, len(claims))

	for _, claim := range claims {
		if claim.Advertised == tunnelID {
			holders = append(holders, claim)
		}
	}

	if len(holders) > 0 {
		return oldest(holders)
	}

	return oldest(claims)
}

// oldest returns the earliest-created claim, breaking equal timestamps by UID
// so the verdict is stable across processes and list orders.
func oldest(claims []Claim) Claim {
	return slices.MinFunc(claims, func(a, b Claim) int {
		if byAge := a.CreatedAt.Compare(b.CreatedAt); byAge != 0 {
			return byAge
		}

		return cmp.Compare(a.UID, b.UID)
	})
}
