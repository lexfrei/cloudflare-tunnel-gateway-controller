package ingress

import (
	"context"
	"log/slog"
	"time"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/metrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
)

// RouteAdapter defines the interface for adapting different route types
// (HTTPRoute, GRPCRoute) to a common format for ingress rule generation.
type RouteAdapter[R any] interface {
	// RouteKind returns the kind of route (e.g., "HTTPRoute", "GRPCRoute").
	RouteKind() string

	// GetMeta returns route metadata (namespace, name).
	GetMeta(route *R) (string, string)

	// GetHostnames returns the hostnames from the route spec.
	// Returns ["*"] if no hostnames are specified.
	GetHostnames(route *R) []gatewayv1.Hostname

	// ExtractEntries extracts route entries from a single route.
	// This includes processing all rules, matches, and backend refs.
	ExtractEntries(
		ctx context.Context,
		route *R,
		resolver *backendResolver,
	) ([]routeEntry, []BackendRefError)

	// AddCatchAll returns true if a catch-all rule should be added.
	AddCatchAll() bool
}

// backendResolver handles backend reference resolution with cross-namespace validation.
type backendResolver struct {
	client        client.Reader
	validator     *referencegrant.Validator
	logger        *slog.Logger
	clusterDomain string
	metrics       metrics.Collector
}

// GenericBuilder is a generic builder for converting Gateway API routes to
// Cloudflare Tunnel ingress configuration.
type GenericBuilder[R any] struct {
	clusterDomain string
	validator     *referencegrant.Validator
	client        client.Reader
	metrics       metrics.Collector
	logger        *slog.Logger
	adapter       RouteAdapter[R]
}

// NewGenericBuilder creates a new GenericBuilder with the specified configuration.
func NewGenericBuilder[R any](
	clusterDomain string,
	validator *referencegrant.Validator,
	c client.Reader,
	m metrics.Collector,
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

	//nolint:prealloc // size depends on variable entries per route
	var entries []routeEntry

	//nolint:prealloc // size depends on variable entries per route
	var failedRefs []BackendRefError

	for i := range routes {
		routeEntries, routeFailedRefs := b.adapter.ExtractEntries(ctx, &routes[i], resolver)
		entries = append(entries, routeEntries...)
		failedRefs = append(failedRefs, routeFailedRefs...)
	}

	sortRouteEntries(entries)

	rules := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(entries)+1)

	for _, entry := range entries {
		rule := zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Service: cloudflare.F(entry.service),
		}

		// Only set hostname for specific hostnames, not wildcards.
		// Omitting hostname means "match all" in Cloudflare Tunnel.
		// Explicit "*" hostname is rejected by Cloudflare API if followed by other rules.
		if entry.hostname != "" && entry.hostname != "*" {
			rule.Hostname = cloudflare.F(entry.hostname)
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

	if b.adapter.AddCatchAll() {
		rules = append(rules, zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Service: cloudflare.F(CatchAllService),
		})
	}

	if b.metrics != nil {
		b.metrics.RecordIngressBuildDuration(ctx, b.adapter.RouteKind(), time.Since(startTime))
	}

	return BuildResult{
		Rules:      rules,
		FailedRefs: failedRefs,
	}
}
