package ingress

import (
	"context"
	"log/slog"
	"maps"
	"slices"
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
// Adapters only extract their typed fields; backend resolution and entry
// assembly run through one shared implementation so the route kinds cannot
// drift.
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

	// RuleBackendRefs returns each rule's backendRefs, one slice per rule.
	RuleBackendRefs(route *R) [][]gatewayv1.BackendRef

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

// Build converts a list of routes to Cloudflare Tunnel ingress rules: one
// rule per distinct hostname, sorted by hostname. The in-process proxy does
// all path and match handling, so no rule carries a path. A hostname served
// by several rules names the lexicographically smallest of their backend
// URLs, which keeps the document independent of route order.
func (b *GenericBuilder[R]) Build(ctx context.Context, routes []R) BuildResult {
	startTime := time.Now()

	resolver := &backendResolver{
		client:        b.client,
		validator:     b.validator,
		logger:        b.logger,
		clusterDomain: b.clusterDomain,
		metrics:       b.metrics,
	}

	var failedRefs []BackendRefError

	services := make(map[string]string)

	for i := range routes {
		routeEntries, routeFailedRefs := extractProjectedEntries(ctx, b.adapter, &routes[i], resolver)
		failedRefs = append(failedRefs, routeFailedRefs...)

		for _, entry := range routeEntries {
			if !entryReachesDocument(entry) {
				b.logger.Info("skipping wildcard route from tunnel config (handled by proxy)",
					"service", entry.service,
				)

				continue
			}

			if current, ok := services[entry.hostname]; !ok || entry.service < current {
				services[entry.hostname] = entry.service
			}
		}
	}

	rules := hostnameRules(services)

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

// entryReachesDocument reports whether a projected entry's hostname is listed
// in the tunnel document. The "*" wildcard is not: the Cloudflare API rejects
// an empty hostname and the in-process proxy matches those itself.
func entryReachesDocument(entry routeEntry) bool {
	return entry.hostname != "*"
}

// hostnameRules renders one rule per hostname, sorted by hostname.
func hostnameRules(services map[string]string) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
	rules := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(services)+1)

	for _, hostname := range slices.Sorted(maps.Keys(services)) {
		rules = append(rules, zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Hostname: cloudflare.F(hostname),
			Service:  cloudflare.F(services[hostname]),
		})
	}

	return rules
}
