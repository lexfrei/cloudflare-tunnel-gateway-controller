package ingress

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// CatchAllService is the Cloudflare Tunnel service that returns HTTP 404.
	// It is always added as the last rule in the ingress configuration.
	CatchAllService = "http_status:404"

	// DefaultHTTPPort is the default port for HTTP backend services.
	DefaultHTTPPort = 80

	// DefaultHTTPSPort is the default port for HTTPS backend services.
	DefaultHTTPSPort = 443

	// Backend reference constants.
	backendGroupCore   = "core"
	backendKindService = "Service"
	schemeHTTP         = "http"
	schemeHTTPS        = "https"
)

// Builder converts Gateway API HTTPRoute resources to Cloudflare Tunnel
// ingress configuration rules.
type Builder struct {
	// ClusterDomain is the Kubernetes cluster domain suffix for service DNS.
	// Typically "cluster.local".
	ClusterDomain string
}

// NewBuilder creates a new Builder with the specified cluster domain.
func NewBuilder(clusterDomain string) *Builder {
	return &Builder{
		ClusterDomain: clusterDomain,
	}
}

// routeEntry is an intermediate representation of an ingress rule.
// Priority 1 indicates exact path match, 0 indicates prefix match.
type routeEntry struct {
	hostname string
	path     string
	service  string
	priority int
}

// Build converts a list of HTTPRoute resources to Cloudflare Tunnel ingress rules.
//
// Rules are sorted by:
//  1. Hostname (alphabetically)
//  2. Priority (exact matches before prefix matches)
//  3. Path length (longer paths first for specificity)
//
// A catch-all rule returning HTTP 404 is always appended as the last rule.
func (b *Builder) Build(routes []gatewayv1.HTTPRoute) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
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

	rules := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(entries)+1)

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

	rules = append(rules, zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
		Service: cloudflare.F(CatchAllService),
	})

	return rules
}

// logUnsupportedHTTPMatchFeatures logs info messages for unsupported HTTPRouteMatch features.
func logUnsupportedHTTPMatchFeatures(namespace, name string, match gatewayv1.HTTPRouteMatch) {
	routeKey := fmt.Sprintf("%s/%s", namespace, name)

	if len(match.Headers) > 0 {
		slog.Info("route configuration partially applied",
			"route", routeKey,
			"reason", "header matching not supported by Cloudflare Tunnel",
			"ignored_headers", len(match.Headers),
		)
	}

	if len(match.QueryParams) > 0 {
		slog.Info("route configuration partially applied",
			"route", routeKey,
			"reason", "query parameter matching not supported by Cloudflare Tunnel",
			"ignored_params", len(match.QueryParams),
		)
	}

	if match.Method != nil {
		slog.Info("route configuration partially applied",
			"route", routeKey,
			"reason", "method matching not supported by Cloudflare Tunnel",
			"ignored_method", string(*match.Method),
		)
	}
}

func (b *Builder) buildRouteEntries(route *gatewayv1.HTTPRoute) []routeEntry {
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
				logUnsupportedHTTPMatchFeatures(route.Namespace, route.Name, match)

				path, priority := b.extractPath(route.Namespace, route.Name, match.Path)
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

func (b *Builder) extractPath(namespace, routeName string, pathMatch *gatewayv1.HTTPPathMatch) (path string, priority int) {
	if pathMatch == nil {
		return "", 0
	}

	pathType := gatewayv1.PathMatchPathPrefix
	if pathMatch.Type != nil {
		pathType = *pathMatch.Type
	}

	path = "/"
	if pathMatch.Value != nil {
		path = *pathMatch.Value
	}

	switch pathType {
	case gatewayv1.PathMatchExact:
		return path, 1
	case gatewayv1.PathMatchRegularExpression:
		// Warn that RegularExpression is treated as PathPrefix
		slog.Info("route configuration partially applied",
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

// logMultipleBackends logs an info message when multiple backendRefs are specified.
func logMultipleBackends(namespace, routeName string, totalBackends int) {
	if totalBackends > 1 {
		slog.Info("route configuration partially applied",
			"route", fmt.Sprintf("%s/%s", namespace, routeName),
			"reason", "multiple backendRefs specified, using only highest weight",
			"total_backends", totalBackends,
			"ignored_backends", totalBackends-1,
		)
	}
}

// logBackendWeights logs info messages for backends with non-default weights.
func logBackendWeights(namespace, routeName string, refs []gatewayv1.HTTPBackendRef) {
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
func (b *Builder) resolveBackendRef(namespace, routeName string, refs []gatewayv1.HTTPBackendRef) string {
	if len(refs) == 0 {
		return ""
	}

	logMultipleBackends(namespace, routeName, len(refs))
	logBackendWeights(namespace, routeName, refs)

	selectedIdx := SelectHighestWeightIndex(wrapHTTPBackendRefs(refs))
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
