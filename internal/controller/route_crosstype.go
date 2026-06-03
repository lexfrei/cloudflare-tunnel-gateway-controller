package controller

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// routeReasonConflicted is the Accepted=False reason stamped on the route that
// loses a cross-route-type conflict. The Gateway API spec mandates only that the
// rejected route's Accepted condition be set to False (grpcroute_types.go:136);
// it defines no standard RouteConditionReason for this case, so we use the
// CamelCase "Conflicted" — mirroring PolicyReasonConflicted used for the
// analogous GEP-713 BackendTLSPolicy precedence loss.
const routeReasonConflicted gatewayv1.RouteConditionReason = "Conflicted"

const crossTypeConflictMessage = "Conflicts with a Route of the other type (HTTPRoute/GRPCRoute) on a shared " +
	"Gateway with intersecting hostnames; the oldest Route by creationTimestamp is accepted"

// crossTypeRoute is a lightweight descriptor of an accepted route used for
// cross-route-type conflict resolution, decoupled from the concrete HTTP/GRPC
// type so the pairwise comparison can be written once.
type crossTypeRoute struct {
	key       string
	created   metav1.Time
	gateways  map[string]bool
	hostnames []gatewayv1.Hostname
}

// resolveCrossTypeConflicts implements the Gateway API cross-route-type conflict
// rule (grpcroute_types.go:129-138): when an HTTPRoute and a GRPCRoute attach to
// the same Gateway and their hostnames intersect, the implementation MUST accept
// exactly one of them — the oldest by creationTimestamp, then the first
// alphabetically by {namespace}/{name} — and set Accepted=False on the loser.
//
// Scope note: a loser is rejected on every parent it was accepted on (whole-route
// rejection), not just the conflicting Gateway. The single flattened tunnel
// ingress has no per-listener serving boundary, so a route accepted anywhere
// would otherwise still serve the conflicting hostname; whole-route rejection is
// the faithful behaviour for this data plane. The multi-Gateway partial case is
// not exercised by the conformance suite.
func resolveCrossTypeConflicts(httpResult *httpRouteResult, grpcResult *grpcRouteResult) {
	httpRoutes := httpCrossTypeRoutes(httpResult)
	grpcRoutes := grpcCrossTypeRoutes(grpcResult)

	httpLosers := make(map[string]bool)
	grpcLosers := make(map[string]bool)

	// Conflict resolution is pairwise per the spec ("accept exactly one of these
	// two routes"). A route that loses any pairing is rejected; in a transitive
	// 3-way conflict the globally-oldest survives and the rest are rejected
	// without re-pairing, which the spec neither mandates nor forbids.
	for _, httpRoute := range httpRoutes {
		for _, grpcRoute := range grpcRoutes {
			if !gatewaysIntersect(httpRoute.gateways, grpcRoute.gateways) {
				continue
			}

			if !hostnamesIntersect(httpRoute.hostnames, grpcRoute.hostnames) {
				continue
			}

			if routeWins(httpRoute, grpcRoute) {
				grpcLosers[grpcRoute.key] = true
			} else {
				httpLosers[httpRoute.key] = true
			}
		}
	}

	httpResult.accepted = rejectCrossTypeLosers(httpResult.accepted, httpResult.bindings, httpLosers,
		func(r gatewayv1.HTTPRoute) (string, *[]gatewayv1.HTTPRoute) {
			return r.Namespace + "/" + r.Name, &httpResult.rejected
		})
	grpcResult.accepted = rejectCrossTypeLosers(grpcResult.accepted, grpcResult.bindings, grpcLosers,
		func(r gatewayv1.GRPCRoute) (string, *[]gatewayv1.GRPCRoute) {
			return r.Namespace + "/" + r.Name, &grpcResult.rejected
		})
}

func httpCrossTypeRoutes(result *httpRouteResult) []crossTypeRoute {
	out := make([]crossTypeRoute, 0, len(result.accepted))
	for i := range result.accepted {
		route := &result.accepted[i]
		key := route.Namespace + "/" + route.Name
		out = append(out, crossTypeRoute{
			key:       key,
			created:   route.CreationTimestamp,
			gateways:  result.bindings[key].acceptedGateways,
			hostnames: route.Spec.Hostnames,
		})
	}

	return out
}

func grpcCrossTypeRoutes(result *grpcRouteResult) []crossTypeRoute {
	out := make([]crossTypeRoute, 0, len(result.accepted))
	for i := range result.accepted {
		route := &result.accepted[i]
		key := route.Namespace + "/" + route.Name
		out = append(out, crossTypeRoute{
			key:       key,
			created:   route.CreationTimestamp,
			gateways:  result.bindings[key].acceptedGateways,
			hostnames: route.Spec.Hostnames,
		})
	}

	return out
}

// routeWins reports whether left should be accepted over right: the oldest by
// creationTimestamp wins, and on a tie the lexicographically smaller
// {namespace}/{name} key wins (the spec's alphabetical tiebreak).
func routeWins(left, right crossTypeRoute) bool {
	if !left.created.Equal(&right.created) {
		return left.created.Before(&right.created)
	}

	return left.key < right.key
}

// rejectCrossTypeLosers moves every accepted route whose key is in losers into
// the rejected slice (via appendRejected) and flips its accepted bindings to
// Accepted=False/Conflicted. Returns the trimmed accepted slice.
func rejectCrossTypeLosers[T any](
	accepted []T,
	bindings map[string]routeBindingInfo,
	losers map[string]bool,
	keyAndRejected func(T) (string, *[]T),
) []T {
	if len(losers) == 0 {
		return accepted
	}

	kept := make([]T, 0, len(accepted))

	for _, route := range accepted {
		key, rejected := keyAndRejected(route)
		if losers[key] {
			*rejected = append(*rejected, route)

			markBindingConflicted(bindings, key)
		} else {
			kept = append(kept, route)
		}
	}

	return kept
}

// markBindingConflicted flips every accepted parent binding of the keyed route
// to Accepted=False with Reason=Conflicted, so the route status writer surfaces
// the rejection on the route's RouteParentStatus.
func markBindingConflicted(bindings map[string]routeBindingInfo, key string) {
	info, ok := bindings[key]
	if !ok {
		return
	}

	for idx, result := range info.bindingResults {
		if !result.Accepted {
			continue
		}

		result.Accepted = false
		result.Reason = routeReasonConflicted
		result.Message = crossTypeConflictMessage
		info.bindingResults[idx] = result
	}
}

// gatewaysIntersect reports whether two routes share at least one accepted
// managed Gateway.
func gatewaysIntersect(left, right map[string]bool) bool {
	// Iterate the smaller set for a cheap membership test.
	if len(right) < len(left) {
		left, right = right, left
	}

	for gw := range left {
		if right[gw] {
			return true
		}
	}

	return false
}

// hostnamesIntersect reports whether two hostname lists overlap. An empty list
// means "all hostnames" (the route inherits the listener's hostnames), so it
// intersects with anything.
func hostnamesIntersect(left, right []gatewayv1.Hostname) bool {
	if len(left) == 0 || len(right) == 0 {
		return true
	}

	for _, lh := range left {
		for _, rh := range right {
			if hostnameMatches(string(lh), string(rh)) {
				return true
			}
		}
	}

	return false
}

// hostnameMatches reports whether two Gateway API hostnames can match a common
// concrete hostname, accounting for the leading-label `*.` wildcard form.
func hostnameMatches(left, right string) bool {
	leftWild := strings.HasPrefix(left, "*.")
	rightWild := strings.HasPrefix(right, "*.")

	switch {
	case !leftWild && !rightWild:
		return left == right
	case leftWild && !rightWild:
		return wildcardCovers(left, right)
	case !leftWild && rightWild:
		return wildcardCovers(right, left)
	default:
		// Two wildcards overlap when one's domain suffix contains the other's.
		return strings.HasSuffix(left[1:], right[1:]) || strings.HasSuffix(right[1:], left[1:])
	}
}

// wildcardCovers reports whether the `*.`-prefixed wildcard matches the concrete
// host: the host must end with the wildcard's `.suffix` and have at least one
// label in front of it.
func wildcardCovers(wildcard, host string) bool {
	suffix := wildcard[1:] // ".example.com"

	return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
}
