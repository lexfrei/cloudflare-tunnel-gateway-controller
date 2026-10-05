package ingress

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cloudflare/cloudflare-go/v7/zero_trust"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
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
	backendGroupCore          = "" // Core resources (Service, Pod, etc.) use empty group
	backendKindService        = "Service"
	backendGroupServiceImport = "multicluster.x-k8s.io"
	backendKindServiceImport  = "ServiceImport"
	backendGroupExternal      = "cf.k8s.lex.la"
	backendKindExternal       = "ExternalBackend"
	// clustersetDomain is the DNS domain multicluster (ServiceImport) Services
	// resolve under, per the KEP-1645 / mcs-api convention.
	clustersetDomain = "clusterset.local"
	schemeHTTP       = "http"
	schemeHTTPS      = "https"
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
	m cfmetrics.Collector,
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

// routeEntry is one hostname a route rule serves and the backend URL the
// rule resolved to.
type routeEntry struct {
	hostname string
	service  string
}

// BackendRefError represents a backend reference that failed validation.
type BackendRefError struct {
	RouteNamespace string
	RouteName      string
	BackendName    string
	BackendNS      string
	Port           int32  // backend port, used to map the failure to a specific proxy backend
	Reason         string // "RefNotPermitted" or other Gateway API reason
	Message        string
	// Domain is the DNS domain the backend's proxy URL is built under, used by
	// the controller to compute the host that matches the failed ref to a
	// specific proxy backend. Empty means the local cluster domain (the default
	// for a Service); a ServiceImport sets it to the clusterset domain.
	Domain string
	// Undecided reports that the reference could not be evaluated, for
	// example because its ReferenceGrants could not be read. It carries no
	// Reason: the status writer keeps the route's previous ResolvedRefs
	// verdict, and the sync is retried.
	Undecided bool
}

// BuildResult contains the build output including rules and any failed references.
type BuildResult struct {
	Rules      []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress
	FailedRefs []BackendRefError
}

// Build converts a list of HTTPRoute resources to Cloudflare Tunnel ingress
// rules, one per hostname, sorted by hostname.
//
// A catch-all rule returning HTTP 404 is always appended as the last rule.
//
// Returns BuildResult containing the generated rules and any backend references
// that failed validation (e.g., due to missing ReferenceGrant).
func (b *Builder) Build(ctx context.Context, routes []gatewayv1.HTTPRoute) BuildResult {
	return b.generic.Build(ctx, routes)
}

// crossNamespaceRefError checks a cross-namespace backend reference against
// the ReferenceGrants in the backend's namespace. toGroup/toKind identify the
// backend resource the grant must permit (e.g. core Service, or
// multicluster.x-k8s.io ServiceImport), so the check is keyed on the actual
// backend kind rather than always Service. It returns nil when the reference
// is allowed, RefNotPermitted when no grant allows it, and an undecided error
// when the grants could not be read.
func crossNamespaceRefError(ctx context.Context, params *serviceResolveParams, toGroup, toKind string) *BackendRefError {
	if params.validator == nil {
		return nil // No validator means validation is disabled
	}

	fromRef := referencegrant.Reference{
		Group:     gatewayv1.GroupName,
		Kind:      params.routeKind,
		Namespace: params.routeNS,
		Name:      params.routeName,
	}

	toRef := referencegrant.Reference{
		Group:     toGroup,
		Kind:      toKind,
		Namespace: params.svcNS,
		Name:      params.svcName,
	}

	route := fmt.Sprintf("%s/%s", params.routeNS, params.routeName)
	target := fmt.Sprintf("%s/%s", params.svcNS, params.svcName)

	allowed, err := params.validator.IsReferenceAllowed(ctx, fromRef, toRef)
	if err != nil {
		level := logging.RepeatsFromContext(ctx).Level("grants "+route+" "+target, err.Error(), slog.LevelError)
		params.logger.Log(ctx, level, "cross-namespace backend reference not evaluated; keeping its previous verdict and retrying",
			"route", route, "target", target, "error", err.Error())

		return &BackendRefError{
			RouteNamespace: params.routeNS,
			RouteName:      params.routeName,
			BackendName:    params.svcName,
			BackendNS:      params.svcNS,
			Message:        fmt.Sprintf("ReferenceGrants for %s could not be read", target),
			Undecided:      true,
		}
	}

	if !allowed {
		params.logger.Info("route configuration partially applied",
			"route", route,
			"reason", "cross-namespace backend reference not permitted by ReferenceGrant",
			"target", target,
		)

		return crossNamespaceDeniedError(params)
	}

	return nil
}
