//nolint:dupl // parallel to HTTPRouteAdapter on purpose: each method is a one-line accessor on a different route type
package ingress

import (
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// GRPCRouteAdapter adapts GRPCRoute for use with GenericBuilder.
type GRPCRouteAdapter struct{}

// RouteKind returns "grpc" for metrics labeling.
func (GRPCRouteAdapter) RouteKind() string {
	return "grpc"
}

// GatewayKind returns the Gateway API kind for ReferenceGrant checks and
// failed-ref reporting.
func (GRPCRouteAdapter) GatewayKind() string {
	return "GRPCRoute"
}

// GetMeta returns the namespace and name of the route.
func (GRPCRouteAdapter) GetMeta(route *gatewayv1.GRPCRoute) (string, string) {
	return route.Namespace, route.Name
}

// GetHostnames returns hostnames from the route, defaulting to ["*"] if empty.
func (GRPCRouteAdapter) GetHostnames(route *gatewayv1.GRPCRoute) []gatewayv1.Hostname {
	return hostnamesOrWildcard(route.Spec.Hostnames)
}

// AddCatchAll returns false because GRPC routes don't add catch-all.
// The catch-all should be added once after merging all route types.
func (GRPCRouteAdapter) AddCatchAll() bool {
	return false
}

// RuleBackendRefs returns each rule's backendRefs.
func (GRPCRouteAdapter) RuleBackendRefs(route *gatewayv1.GRPCRoute) [][]gatewayv1.BackendRef {
	return embeddedBackendRefs(route.Spec.Rules,
		func(rule *gatewayv1.GRPCRouteRule) []gatewayv1.GRPCBackendRef { return rule.BackendRefs },
		func(ref *gatewayv1.GRPCBackendRef) gatewayv1.BackendRef { return ref.BackendRef },
	)
}
