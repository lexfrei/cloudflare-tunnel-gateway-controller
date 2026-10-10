package controller

import (
	"cmp"
	"slices"
	"strconv"
	"time"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// dataPlaneClaim is one opted-in Gateway's ask for a dedicated data plane.
//
// It carries no tunnel identity on purpose. The cap counts what a namespace
// ASKS for, and a Gateway asks by carrying spec.infrastructure.parametersRef —
// whether or not its GatewayConfig or connector token can be read right now.
// Counting only the Gateways whose configuration resolves would let a tenant
// make one token unreadable to free a slot for another Gateway, and would make
// the verdict move with an unrelated Secret's availability.
type dataPlaneClaim struct {
	// Key identifies the Gateway as "namespace/name".
	Key       string
	Namespace string
	// CreatedAt decides who keeps a plane when a namespace is over the cap:
	// oldest first, so a Gateway created now never evicts one already serving.
	// An older Gateway that opts in later does; see overQuotaGateways.
	CreatedAt time.Time
	// UID breaks ties between Gateways created within the same second, which
	// Kubernetes timestamp granularity makes ordinary.
	UID string
}

// overQuotaGateways returns the keys of the Gateways that must not be given a
// dedicated data plane, because their namespace already holds as many as the
// operator allows.
//
// A nil capacity means unlimited, so an operator who never set the cap keeps
// the previous behaviour. Ordering is total and independent of the input order,
// so every layer that asks reaches the same verdict and repeated reconciles do
// not flap between admitting and refusing the same Gateway.
//
// Ordering is by age alone, with no possession term like the one
// tunnelownership.incumbent applies, so an older Gateway opting in later
// displaces a newer one already serving. Possession guards a tunnel against
// another namespace, and every contender for a slot is in the same namespace.
// Age and UID never change, so the verdict depends only on which Gateways the
// listing holds; a possession term would make it depend on Gateway status that
// one of the layers reading it writes.
func overQuotaGateways(capacity *int32, claims []dataPlaneClaim) map[string]bool {
	if capacity == nil {
		return nil
	}

	// Admission rejects a cap below 1, so unlimited reaches here as a nil and
	// as nothing else. The clamp keeps a value that got past a hand-edited CRD
	// from indexing out of range, and leaves it refusing every plane — which is
	// what an operator who wrote 0 asked for.
	limit := max(int(*capacity), 0)

	byNamespace := make(map[string][]dataPlaneClaim)
	for _, claim := range claims {
		byNamespace[claim.Namespace] = append(byNamespace[claim.Namespace], claim)
	}

	refused := make(map[string]bool)

	for _, contenders := range byNamespace {
		if len(contenders) <= limit {
			continue
		}

		slices.SortFunc(contenders, func(a, b dataPlaneClaim) int {
			if byAge := a.CreatedAt.Compare(b.CreatedAt); byAge != 0 {
				return byAge
			}

			return cmp.Compare(a.UID, b.UID)
		})

		for _, claim := range contenders[limit:] {
			refused[claim.Key] = true
		}
	}

	return refused
}

// collectDataPlaneClaims returns one claim per Gateway that has opted into a
// dedicated data plane on a GatewayClass this controller owns.
//
// Built here, once, and shared by every layer that enforces the cap — the
// Gateway reconciler (whose status says so), the infra reconciler (whose plane
// is rendered) and the route partitioner (whose routes are programmed) — for
// the same reason tunnel arbitration shares its claim set: fed different inputs,
// one decision function returns different answers, and a layer that admits a
// Gateway the others refuse leaves it reporting Accepted with nothing serving.
//
// gateways is the same managedInfraGateways listing collectTunnelClaims is fed.
func collectDataPlaneClaims(gateways []*gatewayv1.Gateway) []dataPlaneClaim {
	claims := make([]dataPlaneClaim, 0, len(gateways))
	for _, gateway := range gateways {
		claims = append(claims, dataPlaneClaim{
			Key:       gateway.Namespace + "/" + gateway.Name,
			Namespace: gateway.Namespace,
			CreatedAt: gateway.CreationTimestamp.Time,
			UID:       string(gateway.UID),
		})
	}

	return claims
}

// dataPlaneQuotaMessage renders the refusal for the Gateway's status and Event
// — the surfaces the refused TENANT reads.
//
// It names the cap and never the Gateways holding the slots. Those are the
// tenant's own here, but the message is also the only thing a tenant sees, and
// a cluster where namespaces are the tenancy boundary should not have one
// tenant's object names reachable from another's status. Same rule as the
// tunnel refusal, which never names the holding Gateway.
//
// ownRef says whether the Gateway names its GatewayConfig itself. One on its
// class's default plane has no parametersRef to drop.
func dataPlaneQuotaMessage(capacity int32, ownRef bool) string {
	return "this namespace " + dataPlaneQuotaLimit(capacity) + "; " + dataPlaneQuotaRemedy(capacity, ownRef)
}

func dataPlaneQuotaRemedy(capacity int32, ownRef bool) string {
	const dropRef = "drop spec.infrastructure.parametersRef to serve this Gateway from the shared data plane"

	// Below one there is nothing to delete: no Gateway in the namespace may
	// hold a plane.
	switch {
	case capacity < 1 && ownRef:
		return dropRef
	case capacity < 1:
		return "its GatewayClass gives every Gateway a dedicated data plane, so none can be served here"
	case ownRef:
		return "delete one, or " + dropRef
	}

	return "delete one"
}

// dataPlaneQuotaLimit renders the cap as the phrase both tenant-facing surfaces
// quote — the Gateway's Accepted condition and the status of every route bound
// to it. Shared rather than written twice: they are the two halves of one
// explanation, and a reader who finds them disagreeing cannot tell which is
// right.
func dataPlaneQuotaLimit(capacity int32) string {
	// The subject is supplied by the caller, so a cap below one can drop the
	// "already has N" framing entirely: the namespace holds none and may hold
	// none, which is a different sentence rather than the same one with a zero
	// in it. Negatives take this branch too — overQuotaGateways clamps them to
	// the same refusal, and quoting one back at the tenant explains nothing.
	if capacity < 1 {
		return "may not have a dedicated data plane: the cluster operator allows none"
	}

	gateways := " Gateways with a dedicated data plane"
	if capacity == 1 {
		gateways = " Gateway with a dedicated data plane"
	}

	return "already has the " + strconv.Itoa(int(capacity)) + gateways +
		" that the cluster operator allows"
}
