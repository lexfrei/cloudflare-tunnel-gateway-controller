package ingress

import (
	"context"
	"fmt"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// HTTPRouteAdapter adapts HTTPRoute for use with GenericBuilder.
type HTTPRouteAdapter struct{}

// RouteKind returns "http" for metrics labeling.
func (HTTPRouteAdapter) RouteKind() string {
	return "http"
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

// ExtractEntries extracts route entries from an HTTPRoute.
//
//nolint:revive // routeEntry is intentionally unexported as internal implementation detail
func (a HTTPRouteAdapter) ExtractEntries(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	resolver *backendResolver,
) ([]routeEntry, []BackendRefError) {
	var entries []routeEntry

	var failedRefs []BackendRefError

	hostnames := a.GetHostnames(route)

	for _, hostname := range hostnames {
		for _, rule := range route.Spec.Rules {
			a.logFilters(resolver, route.Namespace, route.Name, rule.Filters)

			service, backendErr := a.resolveBackendRef(ctx, resolver, route.Namespace, route.Name, rule.BackendRefs)
			if service == "" {
				if backendErr != nil {
					failedRefs = append(failedRefs, *backendErr)
				}

				continue
			}

			if len(rule.Matches) == 0 {
				entries = append(entries, routeEntry{
					hostname: string(hostname),
					path:     "",
					service:  service,
					priority: 0,
				})

				continue
			}

			for _, match := range rule.Matches {
				a.logUnsupportedFeatures(resolver, route.Namespace, route.Name, match)

				path, priority := a.extractPath(resolver, route.Namespace, route.Name, match.Path)
				entries = append(entries, routeEntry{
					hostname: string(hostname),
					path:     path,
					service:  service,
					priority: priority,
				})
			}
		}
	}

	return entries, failedRefs
}

func (HTTPRouteAdapter) logFilters(resolver *backendResolver, namespace, name string, filters []gatewayv1.HTTPRouteFilter) {
	if len(filters) > 0 {
		resolver.logger.Info("route configuration partially applied",
			"route", fmt.Sprintf("%s/%s", namespace, name),
			"reason", "filters not supported by Cloudflare Tunnel",
			"ignored_filters", len(filters),
		)
	}
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

//nolint:dupl // Similar to GRPCRouteAdapter.resolveBackendRef but operates on different types
func (a HTTPRouteAdapter) resolveBackendRef(
	ctx context.Context,
	resolver *backendResolver,
	namespace, routeName string,
	refs []gatewayv1.HTTPBackendRef,
) (string, *BackendRefError) {
	if len(refs) == 0 {
		return "", nil
	}

	a.logMultipleBackends(resolver, namespace, routeName, len(refs))
	a.logBackendWeights(resolver, namespace, routeName, refs)

	selectedIdx := SelectHighestWeightIndex(wrapHTTPBackendRefs(refs))
	if selectedIdx == -1 {
		return "", nil
	}

	ref := refs[selectedIdx].BackendRef

	if ref.Group != nil && *ref.Group != "" && *ref.Group != backendGroupCoreAlias {
		return "", nil
	}

	if ref.Kind != nil && *ref.Kind != backendKindService {
		return "", nil
	}

	svcNamespace := namespace
	if ref.Namespace != nil {
		svcNamespace = string(*ref.Namespace)
	}

	port := DefaultHTTPPort
	if ref.Port != nil {
		port = int(*ref.Port)
	}

	url, backendErr := resolveServiceURL(ctx, &serviceResolveParams{
		client:        resolver.client,
		validator:     resolver.validator,
		logger:        resolver.logger,
		clusterDomain: resolver.clusterDomain,
		routeKind:     "HTTPRoute",
		routeNS:       namespace,
		routeName:     routeName,
		svcName:       string(ref.Name),
		svcNS:         svcNamespace,
		port:          port,
	})

	if resolver.metrics != nil {
		if backendErr != nil {
			resolver.metrics.RecordBackendRefValidation(ctx, "http", "failed", backendErr.Reason)
		} else {
			resolver.metrics.RecordBackendRefValidation(ctx, "http", "success", "")
		}
	}

	return url, backendErr
}

func (HTTPRouteAdapter) logMultipleBackends(resolver *backendResolver, namespace, routeName string, totalBackends int) {
	if totalBackends > 1 {
		resolver.logger.Info("route configuration partially applied",
			"route", fmt.Sprintf("%s/%s", namespace, routeName),
			"reason", "multiple backendRefs specified, using only highest weight",
			"total_backends", totalBackends,
			"ignored_backends", totalBackends-1,
		)
	}
}

func (HTTPRouteAdapter) logBackendWeights(resolver *backendResolver, namespace, routeName string, refs []gatewayv1.HTTPBackendRef) {
	for i, backendRef := range refs {
		if backendRef.Weight != nil && *backendRef.Weight != 1 {
			resolver.logger.Info("route configuration partially applied",
				"route", fmt.Sprintf("%s/%s", namespace, routeName),
				"reason", "backendRef weight ignored, traffic splitting not supported",
				"backend", string(backendRef.Name),
				"backend_index", i,
				"weight", *backendRef.Weight,
			)
		}
	}
}
