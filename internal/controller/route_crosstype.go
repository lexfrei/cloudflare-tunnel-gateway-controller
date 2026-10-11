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

const crossTypeConflictMessage = "Conflicts with a Route of the other type (HTTPRoute/GRPCRoute) attached to the " +
	"same listener with intersecting hostnames; the oldest Route by creationTimestamp is accepted"

// crossTypeRoute is a lightweight descriptor of an accepted route used for
// cross-route-type conflict resolution, decoupled from the concrete HTTP/GRPC
// type so the pairwise comparison can be written once.
type crossTypeRoute struct {
	key     string
	created metav1.Time
	// listeners maps each listener the route attaches through to the key of
	// the Gateway it belongs to.
	listeners map[string]string
	hostnames []gatewayv1.Hostname
}

// resolveCrossTypeConflicts implements the Gateway API cross-route-type conflict
// rule (GRPCRouteSpec.Hostnames): when an HTTPRoute and a GRPCRoute attach to
// the same listener and their hostnames intersect, the implementation MUST
// accept exactly one of them — the oldest by creationTimestamp, then the first
// alphabetically by {namespace}/{name} — and set Accepted=False on the loser.
//
// The loser is rejected on every parent that binds it to the Gateway carrying
// the contested listener, and keeps its other Gateways. Narrower is not
// possible: the proxy config scopes a route to the Gateways it is accepted on,
// so a sibling parent through an uncontested listener of the same Gateway
// would serve it on the contested listener too.
func resolveCrossTypeConflicts(httpResult *httpRouteResult, grpcResult *grpcRouteResult) {
	httpRoutes := crossTypeRoutes(httpResult.accepted, httpResult.bindings, httpCrossTypeMeta)
	grpcRoutes := crossTypeRoutes(grpcResult.accepted, grpcResult.bindings, grpcCrossTypeMeta)

	httpLosers := make(map[string]map[string]bool)
	grpcLosers := make(map[string]map[string]bool)

	// Conflict resolution is pairwise per the spec ("accept exactly one of these
	// two routes"). A route that loses any pairing is rejected there; in a
	// transitive 3-way conflict the globally-oldest survives and the rest are
	// rejected without re-pairing, which the spec neither mandates nor forbids.
	for _, httpRoute := range httpRoutes {
		for _, grpcRoute := range grpcRoutes {
			gateways := sharedListenerGateways(httpRoute.listeners, grpcRoute.listeners)
			if len(gateways) == 0 || !hostnamesIntersect(httpRoute.hostnames, grpcRoute.hostnames) {
				continue
			}

			if routeWins(httpRoute, grpcRoute) {
				addLoserGateways(grpcLosers, grpcRoute.key, gateways)
			} else {
				addLoserGateways(httpLosers, httpRoute.key, gateways)
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

func httpCrossTypeMeta(route *gatewayv1.HTTPRoute) (metav1.ObjectMeta, []gatewayv1.Hostname) {
	return route.ObjectMeta, route.Spec.Hostnames
}

func grpcCrossTypeMeta(route *gatewayv1.GRPCRoute) (metav1.ObjectMeta, []gatewayv1.Hostname) {
	return route.ObjectMeta, route.Spec.Hostnames
}

func crossTypeRoutes[T any](
	accepted []T,
	bindings map[string]routeBindingInfo,
	meta func(*T) (metav1.ObjectMeta, []gatewayv1.Hostname),
) []crossTypeRoute {
	out := make([]crossTypeRoute, 0, len(accepted))

	for i := range accepted {
		objectMeta, hostnames := meta(&accepted[i])
		key := objectMeta.Namespace + "/" + objectMeta.Name
		binding := bindings[key]

		listeners := make(map[string]string)

		for idx, keys := range binding.parentListeners {
			if !binding.bindingResults[idx].Accepted {
				continue
			}

			for _, listener := range keys {
				listeners[listener] = binding.parentGateways[idx]
			}
		}

		out = append(out, crossTypeRoute{
			key:       key,
			created:   objectMeta.CreationTimestamp,
			listeners: listeners,
			hostnames: hostnames,
		})
	}

	return out
}

// sharedListenerGateways returns the Gateways of the listeners both routes
// attach through.
func sharedListenerGateways(left, right map[string]string) map[string]bool {
	var out map[string]bool

	for listener, gateway := range left {
		if _, ok := right[listener]; !ok {
			continue
		}

		if out == nil {
			out = make(map[string]bool)
		}

		out[gateway] = true
	}

	return out
}

func addLoserGateways(losers map[string]map[string]bool, key string, gateways map[string]bool) {
	if losers[key] == nil {
		losers[key] = make(map[string]bool, len(gateways))
	}

	for gateway := range gateways {
		losers[key][gateway] = true
	}
}

// parentListenerKey identifies one listener: a Gateway listener or a
// ListenerSet entry, by its parent object and section name.
func parentListenerKey(kind gatewayv1.Kind, namespace, name string, section gatewayv1.SectionName) string {
	return string(kind) + "/" + namespace + "/" + name + "/" + string(section)
}

// recordParentListeners records the listeners an accepted parentRef attaches
// the route through.
func recordParentListeners(
	info *routeBindingInfo,
	refIdx int,
	ref gatewayv1.ParentReference,
	routeNamespace string,
	matched []gatewayv1.SectionName,
) {
	kind := gatewayv1.Kind(kindGateway)
	if ref.Kind != nil && *ref.Kind != "" {
		kind = *ref.Kind
	}

	namespace := routeNamespace
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}

	keys := make([]string, 0, len(matched))
	for _, section := range matched {
		keys = append(keys, parentListenerKey(kind, namespace, string(ref.Name), section))
	}

	if info.parentListeners == nil {
		info.parentListeners = make(map[int][]string)
	}

	info.parentListeners[refIdx] = keys
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

// rejectCrossTypeLosers flips each loser's parents on the Gateways it lost on
// to Accepted=False/Conflicted, and moves a route left with no accepted parent
// into the rejected slice (via keyAndRejected). Returns the trimmed accepted
// slice.
func rejectCrossTypeLosers[T any](
	accepted []T,
	bindings map[string]routeBindingInfo,
	losers map[string]map[string]bool,
	keyAndRejected func(T) (string, *[]T),
) []T {
	if len(losers) == 0 {
		return accepted
	}

	kept := make([]T, 0, len(accepted))

	for _, route := range accepted {
		key, rejected := keyAndRejected(route)
		if gateways, lost := losers[key]; lost && !markBindingConflicted(bindings, key, gateways) {
			*rejected = append(*rejected, route)
		} else {
			kept = append(kept, route)
		}
	}

	return kept
}

// markBindingConflicted flips every accepted parent binding of the keyed route
// on one of gateways to Accepted=False with Reason=Conflicted, and drops those
// Gateways from the route's accepted set. Reports whether the route is still
// accepted on another parent.
func markBindingConflicted(bindings map[string]routeBindingInfo, key string, gateways map[string]bool) bool {
	info, ok := bindings[key]
	if !ok {
		return false
	}

	for idx, result := range info.bindingResults {
		if !result.Accepted || !gateways[info.parentGateways[idx]] {
			continue
		}

		result.Accepted = false
		result.Incomplete = false
		result.Reason = routeReasonConflicted
		result.Message = crossTypeConflictMessage
		info.bindingResults[idx] = result
	}

	for gateway := range gateways {
		delete(info.acceptedGateways, gateway)
	}

	return info.hasAcceptedParent()
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
