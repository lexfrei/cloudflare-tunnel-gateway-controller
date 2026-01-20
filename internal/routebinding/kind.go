package routebinding

import (
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Route kind constants for Gateway API route types.
const (
	KindHTTPRoute = gatewayv1.Kind("HTTPRoute")
	KindGRPCRoute = gatewayv1.Kind("GRPCRoute")
	KindTLSRoute  = gatewayv1.Kind("TLSRoute")
	KindTCPRoute  = gatewayv1.Kind("TCPRoute")
	KindUDPRoute  = gatewayv1.Kind("UDPRoute")
)

// IsRouteKindAllowed checks if a route kind is allowed by the listener.
// Per Gateway API spec:
//   - If allowedRoutes.kinds is nil/empty, defaults are determined by listener protocol.
//   - HTTP/HTTPS protocols allow HTTPRoute and GRPCRoute by default.
//   - TLS protocol allows TLSRoute by default.
//   - TCP protocol allows TCPRoute by default.
//   - UDP protocol allows UDPRoute by default.
func IsRouteKindAllowed(
	allowedRoutes *gatewayv1.AllowedRoutes,
	protocol gatewayv1.ProtocolType,
	routeKind gatewayv1.Kind,
) bool {
	kinds := getAllowedKinds(allowedRoutes, protocol)

	for _, allowed := range kinds {
		if kindMatches(allowed, routeKind) {
			return true
		}
	}

	return false
}

// getAllowedKinds returns the list of allowed route kinds for a listener.
func getAllowedKinds(
	allowedRoutes *gatewayv1.AllowedRoutes,
	protocol gatewayv1.ProtocolType,
) []gatewayv1.RouteGroupKind {
	if allowedRoutes != nil && len(allowedRoutes.Kinds) > 0 {
		return allowedRoutes.Kinds
	}

	return getDefaultKindsForProtocol(protocol)
}

// getDefaultKindsForProtocol returns default allowed route kinds for a protocol.
func getDefaultKindsForProtocol(protocol gatewayv1.ProtocolType) []gatewayv1.RouteGroupKind {
	group := gatewayv1.Group(gatewayv1.GroupName)

	switch protocol {
	case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType:
		return []gatewayv1.RouteGroupKind{
			{Group: &group, Kind: KindHTTPRoute},
			{Group: &group, Kind: KindGRPCRoute},
		}

	case gatewayv1.TLSProtocolType:
		return []gatewayv1.RouteGroupKind{
			{Group: &group, Kind: KindTLSRoute},
		}

	case gatewayv1.TCPProtocolType:
		return []gatewayv1.RouteGroupKind{
			{Group: &group, Kind: KindTCPRoute},
		}

	case gatewayv1.UDPProtocolType:
		return []gatewayv1.RouteGroupKind{
			{Group: &group, Kind: KindUDPRoute},
		}

	default:
		return []gatewayv1.RouteGroupKind{
			{Group: &group, Kind: KindHTTPRoute},
			{Group: &group, Kind: KindGRPCRoute},
		}
	}
}

// kindMatches checks if the allowed kind matches the route kind.
func kindMatches(allowed gatewayv1.RouteGroupKind, routeKind gatewayv1.Kind) bool {
	if allowed.Kind != routeKind {
		return false
	}

	allowedGroup := gatewayv1.Group(gatewayv1.GroupName)
	if allowed.Group != nil && *allowed.Group != "" {
		allowedGroup = *allowed.Group
	}

	return allowedGroup == gatewayv1.Group(gatewayv1.GroupName)
}

// FilterSupportedKinds returns only the route kinds that this controller supports
// (HTTPRoute and GRPCRoute), along with validation status flags.
// Returns:
//   - slice of RouteGroupKinds that this controller supports
//   - hasSupported: true if at least one supported kind exists
//   - hasInvalid: true if any explicitly specified kinds were rejected (unsupported)
//
// Per Gateway API spec, if allowedRoutes.kinds explicitly lists unsupported kinds,
// the listener should report ResolvedRefs=False with reason InvalidRouteKinds.
func FilterSupportedKinds(
	allowedRoutes *gatewayv1.AllowedRoutes,
	protocol gatewayv1.ProtocolType,
) ([]gatewayv1.RouteGroupKind, bool, bool) {
	// Check if kinds are explicitly specified (not defaulted)
	explicitlySpecified := allowedRoutes != nil && len(allowedRoutes.Kinds) > 0

	kinds := getAllowedKinds(allowedRoutes, protocol)

	var supported []gatewayv1.RouteGroupKind

	hasInvalid := false
	gatewayGroup := gatewayv1.Group(gatewayv1.GroupName)

	for _, kind := range kinds {
		// Get the group, defaulting to gateway.networking.k8s.io
		group := gatewayGroup
		if kind.Group != nil && *kind.Group != "" {
			group = *kind.Group
		}

		// Only HTTPRoute and GRPCRoute are supported by this controller
		if group == gatewayGroup && (kind.Kind == KindHTTPRoute || kind.Kind == KindGRPCRoute) {
			supported = append(supported, kind)
		} else if explicitlySpecified {
			// Mark as invalid only if kinds were explicitly specified
			// Default kinds don't count as "invalid" even if protocol defaults include unsupported types
			hasInvalid = true
		}
	}

	return supported, len(supported) > 0, hasInvalid
}
