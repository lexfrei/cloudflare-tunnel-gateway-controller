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
