package routebinding

import (
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// HostnamesIntersect checks if listener and route hostnames have an intersection.
// Per Gateway API spec:
//   - If listener has no hostname (nil or empty), it accepts all routes.
//   - If route has no hostnames (nil or empty), it matches any listener.
//   - Otherwise, at least one hostname must match.
func HostnamesIntersect(listenerHostname *gatewayv1.Hostname, routeHostnames []gatewayv1.Hostname) bool {
	if listenerHostname == nil || *listenerHostname == "" {
		return true
	}

	if len(routeHostnames) == 0 {
		return true
	}

	for _, routeHost := range routeHostnames {
		if hostnameMatches(string(*listenerHostname), string(routeHost)) {
			return true
		}
	}

	return false
}

// hostnameMatches checks if a listener hostname matches a route hostname.
// Supports wildcard prefixes like *.example.com per Gateway API spec.
// DNS names are case-insensitive, so comparison is done in lowercase.
func hostnameMatches(listenerHost, routeHost string) bool {
	listenerHost = strings.ToLower(listenerHost)
	routeHost = strings.ToLower(routeHost)

	if listenerHost == routeHost {
		return true
	}

	listenerIsWildcard := strings.HasPrefix(listenerHost, "*.")
	routeIsWildcard := strings.HasPrefix(routeHost, "*.")

	if listenerIsWildcard && routeIsWildcard {
		listenerSuffix := listenerHost[1:]
		routeSuffix := routeHost[1:]

		return listenerSuffix == routeSuffix
	}

	if listenerIsWildcard {
		return matchesWildcard(listenerHost, routeHost)
	}

	if routeIsWildcard {
		return matchesWildcard(routeHost, listenerHost)
	}

	return false
}

// matchesWildcard checks if specificHost matches wildcardHost pattern.
// wildcardHost must start with "*." (e.g., "*.example.com").
//
// Per Gateway API spec interpretation (permissive mode): *.example.com matches both
// single-level subdomains (foo.example.com) and multi-level subdomains
// (bar.foo.example.com). This is consistent with Envoy Gateway, Istio, and Kong.
//
// *.example.com does NOT match example.com itself (apex domain).
func matchesWildcard(wildcardHost, specificHost string) bool {
	suffix := wildcardHost[1:]

	if !strings.HasSuffix(specificHost, suffix) {
		return false
	}

	if specificHost == suffix[1:] {
		return false
	}

	return true
}
