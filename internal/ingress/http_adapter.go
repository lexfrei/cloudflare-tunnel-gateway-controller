package ingress

import (
	"fmt"

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
	if len(route.Spec.Hostnames) == 0 {
		return []gatewayv1.Hostname{"*"}
	}

	return route.Spec.Hostnames
}

// AddCatchAll returns true because HTTP routes should have a catch-all rule.
func (HTTPRouteAdapter) AddCatchAll() bool {
	return true
}

// ProjectRules translates HTTPRoute rules into the kind-neutral projection,
// logging the HTTP-specific match features the tunnel ingress cannot express
// (headers, query params, methods, regex paths).
//
//nolint:revive // projectedRule is intentionally unexported as internal implementation detail
func (a HTTPRouteAdapter) ProjectRules(route *gatewayv1.HTTPRoute, resolver *backendResolver) []projectedRule {
	rules := make([]projectedRule, 0, len(route.Spec.Rules))

	for _, rule := range route.Spec.Rules {
		projected := projectedRule{
			ignoredFilters: len(rule.Filters),
			backendRefs:    httpBackendRefs(rule.BackendRefs),
		}

		for _, match := range rule.Matches {
			a.logUnsupportedFeatures(resolver, route.Namespace, route.Name, match)

			path, priority := a.extractPath(resolver, route.Namespace, route.Name, match.Path)
			projected.matches = append(projected.matches, projectedMatch{path: path, priority: priority})
		}

		rules = append(rules, projected)
	}

	return rules
}

// httpBackendRefs projects HTTPBackendRefs to the embedded plain BackendRefs
// (the per-backend filters are not expressible in tunnel ingress rules).
func httpBackendRefs(refs []gatewayv1.HTTPBackendRef) []gatewayv1.BackendRef {
	out := make([]gatewayv1.BackendRef, len(refs))
	for i := range refs {
		out[i] = refs[i].BackendRef
	}

	return out
}

func (HTTPRouteAdapter) logUnsupportedFeatures(resolver *backendResolver, namespace, name string, match gatewayv1.HTTPRouteMatch) {
	routeKey := fmt.Sprintf("%s/%s", namespace, name)

	if len(match.Headers) > 0 {
		resolver.logger.Info("route configuration partially applied",
			"route", routeKey,
			"reason", "header matching not supported by Cloudflare Tunnel",
			"ignored_headers", len(match.Headers),
		)
	}

	if len(match.QueryParams) > 0 {
		resolver.logger.Info("route configuration partially applied",
			"route", routeKey,
			"reason", "query parameter matching not supported by Cloudflare Tunnel",
			"ignored_params", len(match.QueryParams),
		)
	}

	if match.Method != nil {
		resolver.logger.Info("route configuration partially applied",
			"route", routeKey,
			"reason", "method matching not supported by Cloudflare Tunnel",
			"ignored_method", string(*match.Method),
		)
	}
}

func (HTTPRouteAdapter) extractPath(resolver *backendResolver, namespace, routeName string, pathMatch *gatewayv1.HTTPPathMatch) (string, int) {
	if pathMatch == nil {
		return "", 0
	}

	pathType := gatewayv1.PathMatchPathPrefix
	if pathMatch.Type != nil {
		pathType = *pathMatch.Type
	}

	path := "/"
	if pathMatch.Value != nil {
		path = *pathMatch.Value
	}

	switch pathType {
	case gatewayv1.PathMatchExact:
		return path, 1
	case gatewayv1.PathMatchRegularExpression:
		resolver.logger.Info("route configuration partially applied",
			"route", fmt.Sprintf("%s/%s", namespace, routeName),
			"reason", "RegularExpression path type treated as PathPrefix",
			"path", path,
		)

		return path, 0
	case gatewayv1.PathMatchPathPrefix:
		return path, 0
	}

	return path, 0
}
