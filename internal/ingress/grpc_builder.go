package ingress

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// GRPCBuilder converts Gateway API GRPCRoute resources to Cloudflare Tunnel
// ingress configuration rules.
type GRPCBuilder struct {
	// ClusterDomain is the Kubernetes cluster domain suffix for service DNS.
	// Typically "cluster.local".
	ClusterDomain string
}

// NewGRPCBuilder creates a new GRPCBuilder with the specified cluster domain.
func NewGRPCBuilder(clusterDomain string) *GRPCBuilder {
	return &GRPCBuilder{
		ClusterDomain: clusterDomain,
	}
}

// Build converts a list of GRPCRoute resources to Cloudflare Tunnel ingress rules.
//
// Rules are sorted by:
//  1. Hostname (alphabetically)
//  2. Priority (exact matches before prefix matches)
//  3. Path length (longer paths first for specificity)
//
// Unlike HTTPRoute builder, this does NOT append a catch-all rule.
// The catch-all rule should be added once after merging all route types.
func (b *GRPCBuilder) Build(routes []gatewayv1.GRPCRoute) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	var entries []routeEntry

	for i := range routes {
		routeEntries := b.buildRouteEntries(&routes[i])
		entries = append(entries, routeEntries...)
	}

	sort.Slice(entries, func(idx, jdx int) bool {
		if entries[idx].hostname != entries[jdx].hostname {
			return entries[idx].hostname < entries[jdx].hostname
		}

		if entries[idx].priority != entries[jdx].priority {
			return entries[idx].priority > entries[jdx].priority
		}

		return len(entries[idx].path) > len(entries[jdx].path)
	})

	rules := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(entries))

	for _, entry := range entries {
		rule := zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Hostname: cloudflare.F(entry.hostname),
			Service:  cloudflare.F(entry.service),
		}

		if entry.path != "" && entry.path != "/" {
			pathWithWildcard := entry.path
			if entry.priority == 0 {
				pathWithWildcard = entry.path + "*"
			}

			rule.Path = cloudflare.F(pathWithWildcard)
		}

		rules = append(rules, rule)
	}

	return rules
}

// logUnsupportedGRPCHeaders logs info messages for unsupported GRPCRouteMatch header features.
func logUnsupportedGRPCHeaders(namespace, name string, headers []gatewayv1.GRPCHeaderMatch) {
	if len(headers) > 0 {
		slog.Info("route configuration partially applied",
			"route", fmt.Sprintf("%s/%s", namespace, name),
			"reason", "header matching not supported by Cloudflare Tunnel",
			"ignored_headers", len(headers),
		)
	}
}

func (b *GRPCBuilder) buildRouteEntries(route *gatewayv1.GRPCRoute) []routeEntry {
	var entries []routeEntry

	hostnames := route.Spec.Hostnames
	if len(hostnames) == 0 {
		hostnames = []gatewayv1.Hostname{"*"}
	}

	for _, hostname := range hostnames {
		for _, rule := range route.Spec.Rules {
			// Warn if filters are specified (not supported)
			if len(rule.Filters) > 0 {
				slog.Info("route configuration partially applied",
					"route", fmt.Sprintf("%s/%s", route.Namespace, route.Name),
					"reason", "filters not supported by Cloudflare Tunnel",
					"ignored_filters", len(rule.Filters),
				)
			}

			service := b.resolveBackendRef(route.Namespace, route.Name, rule.BackendRefs)
			if service == "" {
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
				logUnsupportedGRPCHeaders(route.Namespace, route.Name, match.Headers)

				path, priority := b.extractGRPCPath(match.Method)
				entries = append(entries, routeEntry{
					hostname: string(hostname),
					path:     path,
					service:  service,
					priority: priority,
				})
			}
		}
	}

	return entries
}

// extractGRPCPath converts a GRPCMethodMatch to an HTTP path.
// gRPC requests use HTTP/2 POST to paths like /package.Service/Method.
//
// Returns:
//   - path: the HTTP path (e.g., "/mypackage.MyService/GetUser")
//   - priority: 1 for exact match (service+method), 0 for prefix match (service only or none)
func (b *GRPCBuilder) extractGRPCPath(methodMatch *gatewayv1.GRPCMethodMatch) (path string, priority int) {
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

// logGRPCBackendWeights logs info messages for GRPC backends with non-default weights.
func logGRPCBackendWeights(namespace, routeName string, refs []gatewayv1.GRPCBackendRef) {
	for i, backendRef := range refs {
		if backendRef.Weight != nil && *backendRef.Weight != 1 {
			slog.Info("route configuration partially applied",
				"route", fmt.Sprintf("%s/%s", namespace, routeName),
				"reason", "backendRef weight ignored, traffic splitting not supported",
				"backend", string(backendRef.Name),
				"backend_index", i,
				"weight", *backendRef.Weight,
			)
		}
	}
}

//nolint:dupl // similar structure for different route types is intentional
func (b *GRPCBuilder) resolveBackendRef(namespace, routeName string, refs []gatewayv1.GRPCBackendRef) string {
	if len(refs) == 0 {
		return ""
	}

	logMultipleBackends(namespace, routeName, len(refs))
	logGRPCBackendWeights(namespace, routeName, refs)

	selectedIdx := SelectHighestWeightIndex(wrapGRPCBackendRefs(refs))
	if selectedIdx == -1 {
		return "" // All backends disabled (weight=0)
	}

	ref := refs[selectedIdx].BackendRef

	if ref.Group != nil && *ref.Group != "" && *ref.Group != backendGroupCore {
		return ""
	}

	if ref.Kind != nil && *ref.Kind != backendKindService {
		return ""
	}

	svcNamespace := namespace
	if ref.Namespace != nil {
		svcNamespace = string(*ref.Namespace)
	}

	port := DefaultHTTPPort
	if ref.Port != nil {
		port = int(*ref.Port)
	}

	scheme := schemeHTTP
	if port == DefaultHTTPSPort {
		scheme = schemeHTTPS
	}

	return fmt.Sprintf("%s://%s.%s.svc.%s:%d",
		scheme,
		string(ref.Name),
		svcNamespace,
		b.ClusterDomain,
		port,
	)
}
