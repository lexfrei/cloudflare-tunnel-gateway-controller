package ingress

import (
	"context"
	"fmt"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// GRPCRouteAdapter adapts GRPCRoute for use with GenericBuilder.
type GRPCRouteAdapter struct{}

// RouteKind returns "grpc" for metrics labeling.
func (GRPCRouteAdapter) RouteKind() string {
	return "grpc"
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

// ExtractEntries extracts route entries from a GRPCRoute.
//
//nolint:revive // routeEntry is intentionally unexported as internal implementation detail
func (a GRPCRouteAdapter) ExtractEntries(
	ctx context.Context,
	route *gatewayv1.GRPCRoute,
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
				a.logUnsupportedHeaders(resolver, route.Namespace, route.Name, match.Headers)

				path, priority := a.extractGRPCPath(match.Method)
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

func (GRPCRouteAdapter) logFilters(resolver *backendResolver, namespace, name string, filters []gatewayv1.GRPCRouteFilter) {
	if len(filters) > 0 {
		resolver.logger.Info("route configuration partially applied",
			"route", fmt.Sprintf("%s/%s", namespace, name),
			"reason", "filters not supported by Cloudflare Tunnel",
			"ignored_filters", len(filters),
		)
	}
}

func (GRPCRouteAdapter) logUnsupportedHeaders(resolver *backendResolver, namespace, name string, headers []gatewayv1.GRPCHeaderMatch) {
	if len(headers) > 0 {
		resolver.logger.Info("route configuration partially applied",
			"route", fmt.Sprintf("%s/%s", namespace, name),
			"reason", "header matching not supported by Cloudflare Tunnel",
			"ignored_headers", len(headers),
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

//nolint:dupl // Similar to HTTPRouteAdapter.resolveBackendRef but operates on different types
func (a GRPCRouteAdapter) resolveBackendRef(
	ctx context.Context,
	resolver *backendResolver,
	namespace, routeName string,
	refs []gatewayv1.GRPCBackendRef,
) (string, *BackendRefError) {
	if len(refs) == 0 {
		return "", nil
	}

	a.logMultipleBackends(resolver, namespace, routeName, len(refs))
	a.logBackendWeights(resolver, namespace, routeName, refs)

	selectedIdx := SelectHighestWeightIndex(wrapGRPCBackendRefs(refs))
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
		routeKind:     "GRPCRoute",
		routeNS:       namespace,
		routeName:     routeName,
		svcName:       string(ref.Name),
		svcNS:         svcNamespace,
		port:          port,
	})

	if resolver.metrics != nil {
		if backendErr != nil {
			resolver.metrics.RecordBackendRefValidation(ctx, "grpc", "failed", backendErr.Reason)
		} else {
			resolver.metrics.RecordBackendRefValidation(ctx, "grpc", "success", "")
		}
	}

	return url, backendErr
}

func (GRPCRouteAdapter) logMultipleBackends(resolver *backendResolver, namespace, routeName string, totalBackends int) {
	if totalBackends > 1 {
		resolver.logger.Info("route configuration partially applied",
			"route", fmt.Sprintf("%s/%s", namespace, routeName),
			"reason", "multiple backendRefs specified, using only highest weight",
			"total_backends", totalBackends,
			"ignored_backends", totalBackends-1,
		)
	}
}

func (GRPCRouteAdapter) logBackendWeights(resolver *backendResolver, namespace, routeName string, refs []gatewayv1.GRPCBackendRef) {
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
