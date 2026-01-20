package ingress

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/metrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
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
	backendGroupCore      = ""     // Core resources (Service, Pod, etc.) use empty group
	backendGroupCoreAlias = "core" // Accept "core" as backwards-compatible alias
	backendKindService    = "Service"
	schemeHTTP            = "http"
	schemeHTTPS           = "https"
)

// Builder converts Gateway API HTTPRoute resources to Cloudflare Tunnel
// ingress configuration rules. It wraps GenericBuilder with HTTPRouteAdapter.
type Builder struct {
	generic *GenericBuilder[gatewayv1.HTTPRoute]
}

// NewBuilder creates a new Builder with the specified cluster domain, validator, client, metrics, and logger.
func NewBuilder(
	clusterDomain string,
	validator *referencegrant.Validator,
	c client.Reader,
	m metrics.Collector,
	logger *slog.Logger,
) *Builder {
	return &Builder{
		generic: NewGenericBuilder[gatewayv1.HTTPRoute](
			clusterDomain,
			validator,
			c,
			m,
			logger,
			HTTPRouteAdapter{},
		),
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

// sortRouteEntries sorts entries for Cloudflare Tunnel ingress configuration.
// Wildcard hostname "*" must always come last (Cloudflare requirement).
// Specific hostnames are sorted alphabetically, then by priority (exact > prefix),
// then by path length (longer paths first for specificity), then alphabetically
// by path for deterministic ordering.
func sortRouteEntries(entries []routeEntry) {
	sort.Slice(entries, func(idx, jdx int) bool {
		// Wildcard hostname "*" must always come last
		if entries[idx].hostname == "*" && entries[jdx].hostname != "*" {
			return false
		}

		if entries[idx].hostname != "*" && entries[jdx].hostname == "*" {
			return true
		}

		if entries[idx].hostname != entries[jdx].hostname {
			return entries[idx].hostname < entries[jdx].hostname
		}

		if entries[idx].priority != entries[jdx].priority {
			return entries[idx].priority > entries[jdx].priority
		}

		if len(entries[idx].path) != len(entries[jdx].path) {
			return len(entries[idx].path) > len(entries[jdx].path)
		}

		// Alphabetical path order for deterministic sorting
		return entries[idx].path < entries[jdx].path
	})
}

// BackendRefError represents a backend reference that failed validation.
type BackendRefError struct {
	RouteNamespace string
	RouteName      string
	BackendName    string
	BackendNS      string
	Reason         string // "RefNotPermitted" or other Gateway API reason
	Message        string
}

// BuildResult contains the build output including rules and any failed references.
type BuildResult struct {
	Rules      []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress
	FailedRefs []BackendRefError
}

// Build converts a list of HTTPRoute resources to Cloudflare Tunnel ingress rules.
//
// Rules are sorted by:
//  1. Hostname (specific hostnames before wildcard "*")
//  2. Priority (exact matches before prefix matches)
//  3. Path length (longer paths first for specificity)
//
// A catch-all rule returning HTTP 404 is always appended as the last rule.
//
// Returns BuildResult containing the generated rules and any backend references
// that failed validation (e.g., due to missing ReferenceGrant).
func (b *Builder) Build(ctx context.Context, routes []gatewayv1.HTTPRoute) BuildResult {
	return b.generic.Build(ctx, routes)
}

// validateCrossNamespaceRef validates cross-namespace backend references using ReferenceGrant.
// Returns true if the reference is allowed, false otherwise.
func validateCrossNamespaceRef(
	ctx context.Context,
	validator *referencegrant.Validator,
	logger *slog.Logger,
	routeKind, namespace, routeName, svcNamespace, svcName string,
) bool {
	if validator == nil {
		return true // No validator means validation is disabled
	}

	fromRef := referencegrant.Reference{
		Group:     gatewayv1.GroupName,
		Kind:      routeKind,
		Namespace: namespace,
		Name:      routeName,
	}

	toRef := referencegrant.Reference{
		Group:     backendGroupCore,
		Kind:      backendKindService,
		Namespace: svcNamespace,
		Name:      svcName,
	}

	allowed, err := validator.IsReferenceAllowed(ctx, fromRef, toRef)
	if err != nil {
		logger.Info("route configuration partially applied",
			"route", fmt.Sprintf("%s/%s", namespace, routeName),
			"reason", "failed to validate cross-namespace reference",
			"target", fmt.Sprintf("%s/%s", svcNamespace, svcName),
			"error", err.Error(),
		)

		return false
	}

	if !allowed {
		logger.Info("route configuration partially applied",
			"route", fmt.Sprintf("%s/%s", namespace, routeName),
			"reason", "cross-namespace backend reference not permitted by ReferenceGrant",
			"target", fmt.Sprintf("%s/%s", svcNamespace, svcName),
		)

		return false
	}

	return true
}
