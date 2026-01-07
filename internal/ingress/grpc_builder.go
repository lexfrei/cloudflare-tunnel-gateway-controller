package ingress

import (
	"context"
	"log/slog"

	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/metrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/referencegrant"
)

// GRPCBuilder converts Gateway API GRPCRoute resources to Cloudflare Tunnel
// ingress configuration rules. It wraps GenericBuilder with GRPCRouteAdapter.
type GRPCBuilder struct {
	generic *GenericBuilder[gatewayv1.GRPCRoute]
}

// NewGRPCBuilder creates a new GRPCBuilder with the specified cluster domain, validator, client, metrics, and logger.
func NewGRPCBuilder(
	clusterDomain string,
	validator *referencegrant.Validator,
	c client.Reader,
	m metrics.Collector,
	logger *slog.Logger,
) *GRPCBuilder {
	return &GRPCBuilder{
		generic: NewGenericBuilder[gatewayv1.GRPCRoute](
			clusterDomain,
			validator,
			c,
			m,
			logger,
			GRPCRouteAdapter{},
		),
	}
}

// Build converts a list of GRPCRoute resources to Cloudflare Tunnel ingress rules.
//
// Rules are sorted by:
//  1. Hostname (specific hostnames before wildcard "*")
//  2. Priority (exact matches before prefix matches)
//  3. Path length (longer paths first for specificity)
//
// Unlike HTTPRoute builder, this does NOT append a catch-all rule.
// The catch-all rule should be added once after merging all route types.
//
// Returns BuildResult containing the generated rules and any backend references
// that failed validation (e.g., due to missing ReferenceGrant).
func (b *GRPCBuilder) Build(ctx context.Context, routes []gatewayv1.GRPCRoute) BuildResult {
	return b.generic.Build(ctx, routes)
}
