package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// grpcSegmentPattern matches exactly one gRPC path segment (no slash) — used
// to fill in an empty service or method in a RegularExpression method match.
const grpcSegmentPattern = "[^/]+"

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
// gRPC header matches reuse the HTTP header matcher. By default backends are
// dialed h2c (cleartext HTTP/2) — gRPC requires HTTP/2 and in-cluster gRPC is
// conventionally cleartext. When a BackendTLSPolicy targets the backend's
// Service the converter stamps BackendTLSConfig on the backend, flips the URL
// to https://, and drops the h2c marker so newTLSTransport's ALPN negotiates
// HTTP/2 over TLS. The parent Gateway's clientCertificateRef is layered on
// top of that TLS config for mTLS; on its own — with no policy — it has no
// effect, because Gateway API spec forbids presenting a client cert over
// plaintext (attachGatewayClientCert returns the original config unchanged
// when tlsCfg is nil).
//
// gRPC-specific filters are not yet supported and are skipped with a warning.
// Multiple backendRefs are weighted: every listed backend is emitted with its
// weight, and the proxy's weighted-random selection splits traffic in
// proportion to those weights (same as HTTPRoute).
func ConvertGRPCRoutes(
	ctx context.Context,
	routes []*gatewayv1.GRPCRoute,
	clusterDomain string,
	validator BackendRefValidator,
	tlsResolver BackendTLSResolver,
	gatewayCertResolver GatewayClientCertResolver,
) *Config {
	cfg := &Config{
		Version: configVersionCounter.Add(1),
	}

	sink := &diagSink{}

	for _, route := range routes {
		sink.route(route.Namespace, route.Name)
		hostnames := convertHostnames(route.Spec.Hostnames)
		clientCert := resolveFirstParentClientCertForGRPCRoute(ctx, route, gatewayCertResolver)

		for ruleIdx := range route.Spec.Rules {
			sink.at(ruleIdx)
			cfg.Rules = append(cfg.Rules, convertGRPCRouteRule(
				ctx, &route.Spec.Rules[ruleIdx], hostnames, route.Namespace, clusterDomain,
				validator, tlsResolver, clientCert, sink,
			))
		}
	}

	cfg.Diagnostics = sink.items

	return cfg
}

// resolveFirstParentClientCertForGRPCRoute is the GRPCRoute entry point into
// the shared parent-cert walker — same first-wins rule HTTPRoute uses.
func resolveFirstParentClientCertForGRPCRoute(
	ctx context.Context,
	route *gatewayv1.GRPCRoute,
	resolver GatewayClientCertResolver,
) *ClientCertConfig {
	return resolveFirstParentClientCertFromRefs(ctx, route.Spec.ParentRefs, route.Namespace, resolver)
}

func convertGRPCRouteRule(
	ctx context.Context,
	rule *gatewayv1.GRPCRouteRule,
	hostnames []string,
	namespace string,
	clusterDomain string,
	validator BackendRefValidator,
	tlsResolver BackendTLSResolver,
	clientCert *ClientCertConfig,
	sink *diagSink,
) RouteRule {
	proxyRule := RouteRule{Hostnames: hostnames}

	for _, match := range rule.Matches {
		if converted, ok := convertGRPCMatch(match); ok {
			proxyRule.Matches = append(proxyRule.Matches, converted)
		}
	}

	if len(rule.Filters) > 0 {
		// The proxy serves no GRPCRoute filters yet. Per the Gateway API spec an
		// unsupported filter MUST NOT be silently dropped — the rule fails closed
		// (matched gRPC requests receive HTTP 500) and the route status carries
		// the UnsupportedValue so the operator is not left guessing.
		sink.add(
			DiagnosticAccepted,
			string(gatewayv1.RouteReasonUnsupportedValue),
			grpcFiltersUnsupportedMessage(len(rule.Filters)),
			true,
		)

		proxyRule.UnavailableStatus = http.StatusInternalServerError

		slog.Warn("failing GRPCRoute rule closed: filters not supported by the proxy",
			"namespace", namespace, "filters", len(rule.Filters))
	}

	for backendIdx := range rule.BackendRefs {
		backend, ok := convertGRPCBackendRef(
			ctx, &rule.BackendRefs[backendIdx], namespace, clusterDomain,
			validator, tlsResolver, clientCert, sink,
		)
		if ok {
			proxyRule.Backends = append(proxyRule.Backends, backend)
		}
	}

	return proxyRule
}

// grpcFiltersUnsupportedMessage builds the actionable status message for a
// GRPCRoute rule whose filters the proxy cannot serve. It names the count, the
// consequence (HTTP 500 for matching requests), and the remediation.
func grpcFiltersUnsupportedMessage(count int) string {
	return fmt.Sprintf(
		"%d GRPCRoute filter(s) on this rule are not supported by the proxy; "+
			"matching requests receive HTTP 500. Remove the filters from the rule "+
			"to restore routing.",
		count,
	)
}

// grpcBackendFiltersUnsupportedMessage builds the actionable status message for
// a GRPCRoute backendRef whose per-backend filters the proxy cannot serve. Only
// that backend's traffic fraction fails closed; the rule keeps serving.
func grpcBackendFiltersUnsupportedMessage(serviceName string, count int) string {
	return fmt.Sprintf(
		"%d GRPCRoute filter(s) on the backendRef to Service %q are not supported by the proxy; "+
			"requests routed to that backend receive HTTP 500. Remove the filters from the backendRef "+
			"to restore routing to it.",
		count, serviceName,
	)
}

// grpcBackendFiltersUnsupported reports whether a GRPCRoute backendRef carries
// per-backend filters the proxy cannot serve (none are supported yet). When it
// does, it records a backend-scope diagnostic and returns true so the caller
// fails that backend's traffic fraction closed (HTTP 500). The rule itself keeps
// serving its other backends — the gRPC analogue of the HTTP per-backend
// filter fail-closed.
func grpcBackendFiltersUnsupported(filters []gatewayv1.GRPCRouteFilter, namespace, serviceName string, sink *diagSink) bool {
	if len(filters) == 0 {
		return false
	}

	sink.add(
		DiagnosticAccepted,
		string(gatewayv1.RouteReasonUnsupportedValue),
		grpcBackendFiltersUnsupportedMessage(serviceName, len(filters)),
		false,
	)
	slog.Warn("failing GRPCRoute backend closed: per-backend filters not supported by the proxy",
		"namespace", namespace, "service", serviceName, "filters", len(filters))

	return true
}

// convertGRPCMatch maps a GRPCRouteMatch to a proxy RouteMatch. Returns
// ok=false when the match carries no constraint at all (nil method + no
// headers), which means "match every gRPC request" and is best expressed as
// a rule with no matches rather than an empty RouteMatch.
//
// Consequence for the multi-match case: when a single rule's matches[] array
// mixes an empty (match-all) match with specific ones, the empty match is
// dropped, narrowing the rule from "match all" to "match only the specific
// entries". That author configuration is nonsensical — a match-all sibling
// makes the specific matches redundant — so we deliberately drop it rather
// than promote the whole rule to a catch-all that would shadow other routes.
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

	// gRPC request paths are exactly /{service}/{method} with no extra
	// segments or query, so generated regexes are fully anchored (^…$). The
	// proxy's regex matcher is substring-based (regexp.MatchString), so
	// without anchors a rule for method "Echo" would also match
	// "/svc/EchoStream" and "/svcExtra/Echo". Each user pattern is wrapped in
	// a non-capturing group so a top-level alternation (e.g. method "Foo|Bar")
	// stays scoped to its segment — otherwise "^/svc/Foo|Bar$" parses as
	// "(^/svc/Foo)|(Bar$)" and matches any path ending in "Bar".
	if method.Type != nil && *method.Type == gatewayv1.GRPCMethodMatchRegularExpression {
		svcPattern := service
		if svcPattern == "" {
			svcPattern = grpcSegmentPattern
		}

		methPattern := meth
		if methPattern == "" {
			methPattern = grpcSegmentPattern
		}

		return &PathMatch{Type: PathMatchRegularExpression, Value: "^/(?:" + svcPattern + ")/(?:" + methPattern + ")$"}
	}

	switch {
	case service != "" && meth != "":
		return &PathMatch{Type: PathMatchExact, Value: "/" + service + "/" + meth}
	case service != "" && meth == "":
		return &PathMatch{Type: PathMatchPathPrefix, Value: "/" + service + "/"}
	default:
		// Exact method, any service (implementation-specific per spec):
		// match any single service segment followed by the literal method.
		return &PathMatch{Type: PathMatchRegularExpression, Value: "^/" + grpcSegmentPattern + "/" + regexp.QuoteMeta(meth) + "$"}
	}
}

// convertGRPCHeaderMatch maps a gRPC header match onto the proxy header matcher.
// Unlike path regexes, header regexes are passed through unanchored — the same
// as HTTPRoute header matching, and Gateway API leaves header-regex semantics
// implementation-specific — so this is deliberately consistent, not a gap.
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

// convertGRPCBackendRef mirrors convertBackendRef. By default it forces h2c
// and a cleartext URL scheme — gRPC is HTTP/2, and an in-cluster gRPC backend
// is conventionally cleartext. When a BackendTLSPolicy targets the backend
// Service the converter instead stamps BackendTLSConfig on the backend, keeps
// the https:// URL, and drops the h2c marker so newTLSTransport's ALPN
// negotiates HTTP/2 over TLS. A parent-Gateway clientCertificateRef is
// layered on top of the policy's TLS config (mTLS) only when the policy
// itself put TLS on the wire; with no policy attached, the client cert is
// silently dropped — sending a cert over plaintext is meaningless per
// Gateway API spec.
func convertGRPCBackendRef(
	ctx context.Context,
	backend *gatewayv1.GRPCBackendRef,
	namespace string,
	clusterDomain string,
	validator BackendRefValidator,
	tlsResolver BackendTLSResolver,
	clientCert *ClientCertConfig,
	sink *diagSink,
) (BackendRef, bool) {
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

	svcNamespace := namespace
	if backend.Namespace != nil {
		svcNamespace = string(*backend.Namespace)
	}

	// An invalid backendRef MUST return 500 for its traffic fraction per the
	// Gateway API spec rather than be dropped (mirrors convertBackendRef): a
	// weight>0 invalid ref stays in the weighted pool marked 500, a weight-0 ref
	// is dropped.
	if !IsSupportedBackendRef(backend.BackendObjectReference) {
		return markInvalidBackend(result.Weight, serviceName, svcNamespace, port, clusterDomain, "unsupported backend kind")
	}

	if !validatePort(port) {
		return markInvalidBackend(result.Weight, serviceName, svcNamespace, port, clusterDomain, "invalid port")
	}

	if !validateCrossNamespace(ctx, svcNamespace, namespace, serviceName, backend.BackendObjectReference, validator) {
		return markInvalidBackend(result.Weight, serviceName, svcNamespace, port, clusterDomain,
			"cross-namespace reference not permitted by ReferenceGrant")
	}

	result.URL = buildServiceURL(serviceName, svcNamespace, port, backendDomain(backend.BackendObjectReference, clusterDomain))

	if grpcBackendFiltersUnsupported(backend.Filters, svcNamespace, serviceName, sink) {
		result.UnavailableStatus = http.StatusInternalServerError
	}

	applyGRPCBackendTransport(ctx, &result, tlsResolver, clientCert, svcNamespace, serviceName, port)

	return result, true
}

// applyGRPCBackendTransport resolves the proxy → gRPC-backend transport on
// result: a BackendTLSPolicy (with optional Gateway client cert) puts TLS on the
// wire and ALPN negotiates HTTP/2; with no policy the backend is dialed cleartext
// h2c. Extracted from convertGRPCBackendRef to keep it within the funlen budget.
func applyGRPCBackendTransport(
	ctx context.Context,
	result *BackendRef,
	tlsResolver BackendTLSResolver,
	clientCert *ClientCertConfig,
	svcNamespace, serviceName string,
	port int32,
) {
	result.TLS, result.URL = resolveBackendTLS(ctx, tlsResolver, svcNamespace, serviceName, port, result.URL)
	result.TLS = attachGatewayClientCert(result.TLS, clientCert)

	if result.TLS != nil {
		// TLS is on — newTLSTransport handles ALPN HTTP/2 negotiation, the
		// h2c marker would be ignored on that path (and is misleading), so
		// leave Protocol at the default and keep the https:// URL.
		return
	}

	// Backward-compat path: no policy, no client cert — force cleartext h2c.
	// buildServiceURL emits https:// for port 443; rewrite to http:// so the
	// h2c transport dials cleartext.
	result.URL = strings.Replace(result.URL, "https://", "http://", 1)
	result.Protocol = BackendProtocolH2C
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
