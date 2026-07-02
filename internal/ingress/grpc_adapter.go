package ingress

import (
	"fmt"

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
	if len(route.Spec.Hostnames) == 0 {
		return []gatewayv1.Hostname{"*"}
	}

	return route.Spec.Hostnames
}

// AddCatchAll returns false because GRPC routes don't add catch-all.
// The catch-all should be added once after merging all route types.
func (GRPCRouteAdapter) AddCatchAll() bool {
	return false
}

// ProjectRules translates GRPCRoute rules into the kind-neutral projection,
// logging the gRPC-specific match features the tunnel ingress cannot express
// (header matches) and mapping method matches onto HTTP paths.
//
//nolint:revive // projectedRule is intentionally unexported as internal implementation detail
func (a GRPCRouteAdapter) ProjectRules(route *gatewayv1.GRPCRoute, resolver *backendResolver) []projectedRule {
	rules := make([]projectedRule, 0, len(route.Spec.Rules))

	for _, rule := range route.Spec.Rules {
		projected := projectedRule{
			ignoredFilters: len(rule.Filters),
			backendRefs:    grpcBackendRefs(rule.BackendRefs),
		}

		for _, match := range rule.Matches {
			a.logProxyOnlyHeaderMatches(resolver, route.Namespace, route.Name, match.Headers)

			path, priority := a.extractGRPCPath(match.Method)
			projected.matches = append(projected.matches, projectedMatch{path: path, priority: priority})
		}

		rules = append(rules, projected)
	}

	return rules
}

// grpcBackendRefs projects GRPCBackendRefs to the embedded plain BackendRefs
// (the per-backend filters are not expressible in tunnel ingress rules).
func grpcBackendRefs(refs []gatewayv1.GRPCBackendRef) []gatewayv1.BackendRef {
	out := make([]gatewayv1.BackendRef, len(refs))
	for i := range refs {
		out[i] = refs[i].BackendRef
	}

	return out
}

func (GRPCRouteAdapter) logProxyOnlyHeaderMatches(resolver *backendResolver, namespace, name string, headers []gatewayv1.GRPCHeaderMatch) {
	if len(headers) > 0 {
		resolver.logger.Info("cloudflare tunnel ingress document reduced",
			"route", fmt.Sprintf("%s/%s", namespace, name),
			"reason", "header matching is not expressible in tunnel ingress rules; the in-process proxy performs the match",
			"header_matches", len(headers),
		)
	}
}

// extractGRPCPath converts a GRPCMethodMatch to an HTTP path.
// gRPC requests use HTTP/2 POST to paths like /package.Service/Method.
func (GRPCRouteAdapter) extractGRPCPath(methodMatch *gatewayv1.GRPCMethodMatch) (string, int) {
	if methodMatch == nil {
		return "", 0
	}

	service := ""
	if methodMatch.Service != nil {
		service = *methodMatch.Service
	}

	method := ""
	if methodMatch.Method != nil {
		method = *methodMatch.Method
	}

	// No service and no method - match all gRPC traffic
	if service == "" && method == "" {
		return "", 0
	}

	// Service only - prefix match on /Service/
	if service != "" && method == "" {
		return "/" + service + "/", 0
	}

	// Method only (implementation-specific) - not fully supported, treat as prefix
	if service == "" && method != "" {
		return "", 0
	}

	// Both service and method - exact match
	return "/" + service + "/" + method, 1
}
