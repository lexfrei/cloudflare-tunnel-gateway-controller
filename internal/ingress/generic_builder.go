package ingress

import (
	"context"
	"log/slog"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/zero_trust"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
)

// RouteAdapter defines the interface for adapting different route types
// (HTTPRoute, GRPCRoute) to a common format for ingress rule generation.
// Adapters are pure projections: they translate their typed rules into the
// kind-neutral projectedRule shape, and every downstream step (filter
// logging, backend resolution, entry assembly) runs through one shared
// implementation so the route kinds cannot drift.
type RouteAdapter[R any] interface {
	// RouteKind returns the short route kind used for metrics labeling
	// (e.g., "http", "grpc").
	RouteKind() string

	// GatewayKind returns the Gateway API kind name used in ReferenceGrant
	// checks and failed-ref reporting (e.g., "HTTPRoute", "GRPCRoute").
	GatewayKind() string

	// GetMeta returns route metadata (namespace, name).
	GetMeta(route *R) (string, string)

	// GetHostnames returns the hostnames from the route spec.
	// Returns ["*"] if no hostnames are specified.
	GetHostnames(route *R) []gatewayv1.Hostname

	// ProjectRules translates the route's typed rules into the kind-neutral
	// projection, logging any per-kind features the tunnel ingress cannot
	// express along the way.
	ProjectRules(route *R, resolver *backendResolver) []projectedRule

	// AddCatchAll returns true if a catch-all rule should be added.
	AddCatchAll() bool
}

// backendResolver handles backend reference resolution with cross-namespace validation.
type backendResolver struct {
	client        client.Reader
	validator     *referencegrant.Validator
	logger        *slog.Logger
	clusterDomain string
	metrics       cfmetrics.Collector
}

// GenericBuilder is a generic builder for converting Gateway API routes to
// Cloudflare Tunnel ingress configuration.
type GenericBuilder[R any] struct {
	clusterDomain string
	validator     *referencegrant.Validator
	client        client.Reader
	metrics       cfmetrics.Collector
	logger        *slog.Logger
	adapter       RouteAdapter[R]
}

// NewGenericBuilder creates a new GenericBuilder with the specified configuration.
func NewGenericBuilder[R any](
	clusterDomain string,
	validator *referencegrant.Validator,
	c client.Reader,
	m cfmetrics.Collector,
	logger *slog.Logger,
	adapter RouteAdapter[R],
) *GenericBuilder[R] {
	if logger == nil {
		logger = slog.Default()
	}

	return &GenericBuilder[R]{
		clusterDomain: clusterDomain,
		validator:     validator,
		client:        c,
		metrics:       m,
		logger:        logger.With("builder", adapter.RouteKind()),
		adapter:       adapter,
	}
}

// Build converts a list of routes to Cloudflare Tunnel ingress rules.
//
// Rules are sorted by:
//  1. Hostname (specific hostnames before wildcard "*")
//  2. Priority (exact matches before prefix matches)
//  3. Path length (longer paths first for specificity)
func (b *GenericBuilder[R]) Build(ctx context.Context, routes []R) BuildResult {
	startTime := time.Now()

	resolver := &backendResolver{
		client:        b.client,
		validator:     b.validator,
		logger:        b.logger,
		clusterDomain: b.clusterDomain,
		metrics:       b.metrics,
	}

	var entries []routeEntry

	var failedRefs []BackendRefError

	rulesByNamespace := make(map[string]int, len(routes))

	for i := range routes {
		namespace, _ := b.adapter.GetMeta(&routes[i])
		routeEntries, routeFailedRefs := extractProjectedEntries(ctx, b.adapter, &routes[i], resolver)
		entries = append(entries, routeEntries...)
		failedRefs = append(failedRefs, routeFailedRefs...)
		rulesByNamespace[namespace] += countIngressBearing(routeEntries)
	}

	sortRouteEntries(entries)

	rules := entriesToIngressRules(entries, b.logger)

	if b.adapter.AddCatchAll() {
		rules = append(rules, zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Service: cloudflare.F(CatchAllService),
		})
	}

	if b.metrics != nil {
		b.metrics.RecordIngressBuildDuration(ctx, b.adapter.RouteKind(), time.Since(startTime))
	}

	return BuildResult{
		Rules:            rules,
		FailedRefs:       failedRefs,
		RulesByNamespace: rulesByNamespace,
	}
}

// entryReachesDocument reports whether a projected entry becomes a rule in the
// tunnel document. Wildcard-hostname entries do not: the Cloudflare API rejects
// an empty hostname and the in-process proxy matches those itself.
//
// entriesToIngressRules and countIngressBearing both ask this question, and
// they must not answer it differently — a second reason to drop an entry,
// added to the former alone, would make the latter overstate a namespace's
// share of the rule budget with nothing to catch it.
func entryReachesDocument(entry routeEntry) bool {
	return entry.hostname != "*"
}

// countIngressBearing counts the entries of one route that survive into the
// tunnel document, which is that route's contribution to the rule budget.
func countIngressBearing(entries []routeEntry) int {
	count := 0

	for _, entry := range entries {
		if entryReachesDocument(entry) {
			count++
		}
	}

	return count
}

// entriesToIngressRules converts sorted route entries into Cloudflare ingress rules.
// Wildcard entries (hostname == "*") are skipped — Cloudflare API rejects rules
// with empty hostname (error 1056). These routes are handled by the in-process
// L7 proxy via its OverrideProxy hook, not by Cloudflare's edge ingress.
func entriesToIngressRules(
	entries []routeEntry,
	logger *slog.Logger,
) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	rules := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(entries))

	for _, entry := range entries {
		if !entryReachesDocument(entry) {
			logger.Info("skipping wildcard route from tunnel config (handled by proxy)",
				"service", entry.service,
			)

			continue
		}

		rule := zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Service:  cloudflare.F(entry.service),
			Hostname: cloudflare.F(entry.hostname),
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
