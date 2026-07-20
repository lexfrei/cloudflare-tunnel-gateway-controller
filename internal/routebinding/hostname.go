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

// HostnameIntersection returns the effective served hostname for a single
// (listener hostname, route hostname) pair per Gateway API intersection
// semantics, and ok=false when the two do not intersect.
//
// The MORE SPECIFIC of the two is returned: an exact route host under a
// wildcard listener yields the exact route host; a wildcard route host over an
// exact listener host yields the listener's exact host; overlapping wildcards
// yield the narrower (longer-suffix) wildcard; two equal hostnames yield that
// hostname. A listener with no hostname (nil or empty) accepts every hostname,
// so the route hostname passes through unchanged. Wildcard matching follows
// hostnameMatches: "*.example.com" matches any subdomain but never the apex.
func HostnameIntersection(listenerHostname *gatewayv1.Hostname, routeHostname gatewayv1.Hostname) (gatewayv1.Hostname, bool) {
	if listenerHostname == nil || *listenerHostname == "" {
		return routeHostname, true
	}

	if !hostnameMatches(string(*listenerHostname), string(routeHostname)) {
		return "", false
	}

	// hostnameMatches is true; return the more specific side. A wildcard route
	// host is the broader side unless it is nested deeper than a wildcard
	// listener, so compare by length when both are wildcards (the longer
	// suffix is the narrower set); otherwise an exact route host is the most
	// specific, and a wildcard route host loses to the listener's hostname.
	if strings.HasPrefix(string(routeHostname), "*.") {
		if strings.HasPrefix(string(*listenerHostname), "*.") && len(routeHostname) > len(*listenerHostname) {
			return routeHostname, true
		}

		return *listenerHostname, true
	}

	return routeHostname, true
}

// EffectiveListenerHostnames returns the hostnames a route effectively serves
// through a single listener: the intersection of the route's hostnames with the
// listener's hostname. A route with no hostnames of its own inherits the
// listener's hostname; when both are unset the result is empty (the listener is
// a catch-all and the route serves every hostname). Route hostname order is
// preserved and non-intersecting entries are dropped.
func EffectiveListenerHostnames(listenerHostname *gatewayv1.Hostname, routeHostnames []gatewayv1.Hostname) []gatewayv1.Hostname {
	if len(routeHostnames) == 0 {
		if listenerHostname != nil && *listenerHostname != "" {
			return []gatewayv1.Hostname{*listenerHostname}
		}

		return nil
	}

	var out []gatewayv1.Hostname

	for _, routeHost := range routeHostnames {
		if effective, ok := HostnameIntersection(listenerHostname, routeHost); ok {
			out = append(out, effective)
		}
	}

	return out
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

		// Two wildcards intersect when they overlap, i.e. one suffix is nested
		// under the other ("*.sub.example.com" is a strict subset of
		// "*.example.com"); the intersection is the narrower of the two.
		return strings.HasSuffix(listenerSuffix, routeSuffix) || strings.HasSuffix(routeSuffix, listenerSuffix)
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
