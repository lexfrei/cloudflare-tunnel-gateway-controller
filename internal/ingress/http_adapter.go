//nolint:dupl // parallel to GRPCRouteAdapter on purpose: each method is a one-line accessor on a different route type
package ingress

import (
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// HTTPRouteAdapter adapts HTTPRoute for use with GenericBuilder.
type HTTPRouteAdapter struct{}

// RouteKind returns "http" for metrics labeling.
func (HTTPRouteAdapter) RouteKind() string {
	return "http"
}

// GatewayKind returns the Gateway API kind for ReferenceGrant checks and
// failed-ref reporting.
func (HTTPRouteAdapter) GatewayKind() string {
	return "HTTPRoute"
}

// GetMeta returns the namespace and name of the route.
func (HTTPRouteAdapter) GetMeta(route *gatewayv1.HTTPRoute) (string, string) {
	return route.Namespace, route.Name
}

// GetHostnames returns hostnames from the route, defaulting to ["*"] if empty.
func (HTTPRouteAdapter) GetHostnames(route *gatewayv1.HTTPRoute) []gatewayv1.Hostname {
	return hostnamesOrWildcard(route.Spec.Hostnames)
}

// AddCatchAll returns true because HTTP routes should have a catch-all rule.
func (HTTPRouteAdapter) AddCatchAll() bool {
	return true
}

// RuleBackendRefs returns each rule's backendRefs.
func (HTTPRouteAdapter) RuleBackendRefs(route *gatewayv1.HTTPRoute) [][]gatewayv1.BackendRef {
	return embeddedBackendRefs(route.Spec.Rules,
		func(rule *gatewayv1.HTTPRouteRule) []gatewayv1.HTTPBackendRef { return rule.BackendRefs },
		func(ref *gatewayv1.HTTPBackendRef) gatewayv1.BackendRef { return ref.BackendRef },
	)
}
