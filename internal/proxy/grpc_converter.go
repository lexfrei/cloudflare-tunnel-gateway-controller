package proxy

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ConvertGRPCRoutes converts GRPCRoute resources into proxy Config rules.
//
// gRPC requests are HTTP/2 POSTs to the path /{service}/{method}, so each
// GRPCMethodMatch maps onto the proxy's existing path matcher:
//
//   - Exact service+method  → exact path  /{service}/{method}
//   - Exact service-only     → prefix path /{service}/   (all methods)
//   - Exact method-only      → regex       /[^/]+/{method} (any service)
//   - RegularExpression      → regex       /{service}/{method} (service/method as written)
//
// gRPC header matches reuse the HTTP header matcher. All backends are forced
// to h2c (cleartext HTTP/2) regardless of the Service port's appProtocol —
// gRPC requires HTTP/2 and in-cluster gRPC is conventionally cleartext.
//
// gRPC-specific filters are not yet supported and are skipped with a warning;
// weighted splitting across multiple backendRefs is not modelled (the proxy
// selects all listed backends with their weights, same as HTTPRoute).
func ConvertGRPCRoutes(
	ctx context.Context,
	routes []*gatewayv1.GRPCRoute,
	clusterDomain string,
	validator BackendRefValidator,
) *Config {
	cfg := &Config{
		Version: configVersionCounter.Add(1),
	}

	for _, route := range routes {
		hostnames := convertHostnames(route.Spec.Hostnames)

		for ruleIdx := range route.Spec.Rules {
			cfg.Rules = append(cfg.Rules, convertGRPCRouteRule(
				ctx, &route.Spec.Rules[ruleIdx], hostnames, route.Namespace, clusterDomain, validator,
			))
		}
	}

	return cfg
}

func convertGRPCRouteRule(
	ctx context.Context,
	rule *gatewayv1.GRPCRouteRule,
	hostnames []string,
	namespace string,
	clusterDomain string,
	validator BackendRefValidator,
) RouteRule {
	proxyRule := RouteRule{Hostnames: hostnames}

	for _, match := range rule.Matches {
		if converted, ok := convertGRPCMatch(match); ok {
			proxyRule.Matches = append(proxyRule.Matches, converted)
		}
	}

	if len(rule.Filters) > 0 {
		slog.Warn("skipping GRPCRoute filters — not supported by the proxy yet",
			"namespace", namespace, "filters", len(rule.Filters))
	}

	for backendIdx := range rule.BackendRefs {
		backend, ok := convertGRPCBackendRef(ctx, &rule.BackendRefs[backendIdx], namespace, clusterDomain, validator)
		if ok {
			proxyRule.Backends = append(proxyRule.Backends, backend)
		}
	}

	return proxyRule
}

// convertGRPCMatch maps a GRPCRouteMatch to a proxy RouteMatch. Returns
// ok=false when the match carries no constraint at all (nil method + no
// headers), which means "match every gRPC request" and is best expressed as
// a rule with no matches rather than an empty RouteMatch.
func convertGRPCMatch(match gatewayv1.GRPCRouteMatch) (RouteMatch, bool) {
	proxyMatch := RouteMatch{}

	if path := grpcMethodToPath(match.Method); path != nil {
		proxyMatch.Path = path
	}

	for _, header := range match.Headers {
		proxyMatch.Headers = append(proxyMatch.Headers, convertGRPCHeaderMatch(header))
	}

	if proxyMatch.Path == nil && len(proxyMatch.Headers) == 0 {
		return RouteMatch{}, false
	}

	return proxyMatch, true
}

// grpcMethodToPath converts a GRPCMethodMatch to the proxy path matcher form.
// Returns nil when both service and method are empty (match all) — the CEL
// validation on the CRD guarantees at least one is set, so nil here is the
// defensive "no constraint" case.
func grpcMethodToPath(method *gatewayv1.GRPCMethodMatch) *PathMatch {
	if method == nil {
		return nil
	}

	service := derefString(method.Service)
	meth := derefString(method.Method)

	if service == "" && meth == "" {
		return nil
	}

	if method.Type != nil && *method.Type == gatewayv1.GRPCMethodMatchRegularExpression {
		svcPattern := service
		if svcPattern == "" {
			svcPattern = "[^/]+"
		}

		methPattern := meth
		if methPattern == "" {
			methPattern = ".*"
		}

		return &PathMatch{Type: PathMatchRegularExpression, Value: "/" + svcPattern + "/" + methPattern}
	}

	switch {
	case service != "" && meth != "":
		return &PathMatch{Type: PathMatchExact, Value: "/" + service + "/" + meth}
	case service != "" && meth == "":
		return &PathMatch{Type: PathMatchPathPrefix, Value: "/" + service + "/"}
	default:
		// Exact method, any service (implementation-specific per spec):
		// match any single service segment followed by the literal method.
		return &PathMatch{Type: PathMatchRegularExpression, Value: "/[^/]+/" + regexp.QuoteMeta(meth)}
	}
}

func convertGRPCHeaderMatch(header gatewayv1.GRPCHeaderMatch) HeaderMatch {
	result := HeaderMatch{
		Name:  string(header.Name),
		Value: header.Value,
		Type:  HeaderMatchExact,
	}

	if header.Type != nil && *header.Type == gatewayv1.GRPCHeaderMatchRegularExpression {
		result.Type = HeaderMatchRegularExpression
	}

	return result
}

// convertGRPCBackendRef mirrors convertBackendRef but forces h2c and a
// cleartext URL scheme — gRPC is HTTP/2, and the tunnel/proxy hop to an
// in-cluster gRPC backend is cleartext. BackendTLSPolicy / Gateway client
// certs are intentionally not applied to gRPC backends in this revision.
func convertGRPCBackendRef(
	ctx context.Context,
	backend *gatewayv1.GRPCBackendRef,
	namespace string,
	clusterDomain string,
	validator BackendRefValidator,
) (BackendRef, bool) {
	if !IsServiceBackendRef(backend.BackendObjectReference) {
		slog.Warn("skipping non-Service gRPC backend kind", "kind", backend.Kind, "name", backend.Name)

		return BackendRef{}, false
	}

	result := BackendRef{Weight: 1}
	if backend.Weight != nil {
		result.Weight = *backend.Weight
	}

	if result.Weight < 0 {
		slog.Warn("skipping gRPC backend with negative weight", "name", string(backend.Name), "weight", result.Weight)

		return BackendRef{}, false
	}

	serviceName := string(backend.Name)

	port := int32(defaultServicePort)
	if backend.Port != nil {
		port = *backend.Port
	}

	if !validatePort(port) {
		slog.Warn("skipping gRPC backend with invalid port", "service", serviceName, "port", port)

		return BackendRef{}, false
	}

	svcNamespace := namespace
	if backend.Namespace != nil {
		svcNamespace = string(*backend.Namespace)
	}

	if !validateCrossNamespace(ctx, svcNamespace, namespace, serviceName, backend.BackendObjectReference, validator) {
		return BackendRef{}, false
	}

	// buildServiceURL forces https on port 443; gRPC h2c is cleartext, so
	// normalise the scheme to http regardless of port.
	url := strings.Replace(buildServiceURL(serviceName, svcNamespace, port, clusterDomain), "https://", "http://", 1)

	result.URL = url
	result.Protocol = BackendProtocolH2C

	return result, true
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
