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
// protocolResolver reads the backend Service port's appProtocol. gRPC is HTTP/2
// by definition, so the only meaningful appProtocol axis here is TLS-vs-cleartext:
// a TLS appProtocol (https / HTTPS / kubernetes.io/wss) with no BackendTLSPolicy
// fails the backend closed (HTTP 502, ResolvedRefs=False / UnsupportedProtocol),
// mirroring the HTTP path, instead of silently dialing cleartext h2c. Every other
// value (nil resolver, unset, kubernetes.io/h2c, or unrecognised) keeps the h2c
// default — the correct gRPC transport regardless.
//
// The core RequestHeaderModifier and extended ResponseHeaderModifier filters
// are served through the shared header-modifier pipeline; RequestMirror and
// ExtensionRef are not served yet and fail closed (HTTP 500).
// Multiple backendRefs are weighted: every listed backend is emitted with its
// weight, and the proxy's weighted-random selection splits traffic in
// proportion to those weights (same as HTTPRoute).
//
//nolint:dupl // the two Convert*Routes wrappers are intentionally parallel lambda wiring into convertRoutesGeneric; the route types differ, so they cannot merge further.
func ConvertGRPCRoutes(
	ctx context.Context,
	routes []*gatewayv1.GRPCRoute,
	clusterDomain string,
	validator BackendRefValidator,
	protocolResolver BackendProtocolResolver,
	tlsResolver BackendTLSResolver,
	gatewayCertResolver GatewayClientCertResolver,
) *Config {
	return convertRoutesGeneric(ctx, routes, gatewayCertResolver, routeKindView[*gatewayv1.GRPCRoute]{
		hostnames:  func(route *gatewayv1.GRPCRoute) []gatewayv1.Hostname { return route.Spec.Hostnames },
		parentRefs: func(route *gatewayv1.GRPCRoute) []gatewayv1.ParentReference { return route.Spec.ParentRefs },
		ruleCount:  func(route *gatewayv1.GRPCRoute) int { return len(route.Spec.Rules) },
		convertRule: func(ctx context.Context, route *gatewayv1.GRPCRoute, ruleIdx int,
			hostnames []string, clientCert *ClientCertConfig, sink *diagSink,
		) RouteRule {
			return convertGRPCRouteRule(
				ctx, &route.Spec.Rules[ruleIdx], hostnames, route.Namespace, clusterDomain,
				validator, protocolResolver, tlsResolver, clientCert, sink,
			)
		},
	})
}

func convertGRPCRouteRule(
	ctx context.Context,
	rule *gatewayv1.GRPCRouteRule,
	hostnames []string,
	namespace string,
	clusterDomain string,
	validator BackendRefValidator,
	protocolResolver BackendProtocolResolver,
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

	for filterIdx := range rule.Filters {
		converted, failClosed := convertGRPCFilter(&rule.Filters[filterIdx], filterScopeRule, sink)
		if failClosed {
			proxyRule.UnavailableStatus = http.StatusInternalServerError
		}

		if converted != nil {
			proxyRule.Filters = append(proxyRule.Filters, *converted)
		}
	}

	for backendIdx := range rule.BackendRefs {
		backend, ok := convertGRPCBackendRef(
			ctx, &rule.BackendRefs[backendIdx], namespace, clusterDomain,
			validator, protocolResolver, tlsResolver, clientCert, sink,
		)
		if ok {
			proxyRule.Backends = append(proxyRule.Backends, backend)
		}
	}

	return proxyRule
}

// convertGRPCFilter maps a single GRPCRouteFilter into the proxy wire shape. The
// core RequestHeaderModifier and the extended ResponseHeaderModifier are served
// through the same header-modifier pipeline as HTTPRoute — gRPC metadata is
// carried as HTTP/2 headers, so the header-modifier code applies unchanged.
// RequestMirror (extended) and ExtensionRef (implementation-specific) are not
// served yet: per the Gateway API spec an unsupported filter MUST NOT be
// silently dropped, so they fail closed (matched requests receive HTTP 500) and
// the route status carries the UnsupportedValue. scope controls whether a
// fail-closed filter takes the whole rule down or only the backend fraction.
func convertGRPCFilter(filter *gatewayv1.GRPCRouteFilter, scope string, sink *diagSink) (*RouteFilter, bool) {
	switch filter.Type {
	case gatewayv1.GRPCRouteFilterRequestHeaderModifier:
		return convertRequestHeaderFilter(filter.RequestHeaderModifier), false
	case gatewayv1.GRPCRouteFilterResponseHeaderModifier:
		return convertResponseHeaderFilter(filter.ResponseHeaderModifier), false
	case gatewayv1.GRPCRouteFilterRequestMirror, gatewayv1.GRPCRouteFilterExtensionRef:
		// Unsupported: handled by the fail-closed path below. Listed explicitly so
		// the exhaustive linter confirms every enum value is accounted for.
	}

	sink.add(
		DiagnosticAccepted,
		string(gatewayv1.RouteReasonUnsupportedValue),
		unsupportedGRPCFilterMessage(scope, string(filter.Type)),
		scope == filterScopeRule,
	)
	slog.Warn("failing closed: unsupported GRPCRoute filter type", "type", filter.Type, "scope", scope)

	return nil, true
}

// applyGRPCBackendFilters converts a GRPCRoute backendRef's per-backend filters
// and applies them to result: supported header modifiers are appended to
// result.Filters; an unsupported filter (RequestMirror, ExtensionRef) fails only
// this backend's traffic fraction closed (HTTP 500), the gRPC analogue of the
// HTTP per-backend filter fail-closed. The rule keeps serving its other backends.
func applyGRPCBackendFilters(result *BackendRef, filters []gatewayv1.GRPCRouteFilter, sink *diagSink) {
	for filterIdx := range filters {
		converted, failClosed := convertGRPCFilter(&filters[filterIdx], filterScopeBackend, sink)
		if failClosed {
			result.UnavailableStatus = http.StatusInternalServerError
		}

		if converted != nil {
			result.Filters = append(result.Filters, *converted)
		}
	}
}

// unsupportedGRPCFilterMessage builds the actionable status message for a
// GRPCRoute filter type the proxy cannot serve. It names the offending type, the
// consequence (HTTP 500 for matched requests), and the supported alternatives.
func unsupportedGRPCFilterMessage(scope, filterType string) string {
	return fmt.Sprintf(
		"GRPCRoute filter type %q on this %s is not supported; matching requests receive HTTP 500. "+
			"Remove the filter or replace it with a supported type "+
			"(RequestHeaderModifier, ResponseHeaderModifier).",
		filterType, scope,
	)
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
// Gateway API spec. When the Service port declares a TLS appProtocol
// (https / kubernetes.io/wss) but no BackendTLSPolicy attached, the backend
// fails closed instead of being dialed cleartext h2c (see
// applyGRPCBackendTransport), mirroring the HTTP path.
func convertGRPCBackendRef(
	ctx context.Context,
	backend *gatewayv1.GRPCBackendRef,
	namespace string,
	clusterDomain string,
	validator BackendRefValidator,
	protocolResolver BackendProtocolResolver,
	tlsResolver BackendTLSResolver,
	clientCert *ClientCertConfig,
	sink *diagSink,
) (BackendRef, bool) {
	common := resolveCommonBackendRef(ctx, &backend.BackendRef, namespace, clusterDomain, validator)

	switch common.outcome {
	case backendRefDropped:
		return BackendRef{}, false
	case backendRefFinal:
		return common.backend, common.keep
	case backendRefExternal:
		// ExternalBackend: URL comes from the CRD (resolved controller-side via
		// a sentinel); the backendRef port is ignored in favour of spec.port.
		return convertGRPCExternalBackendRef(ctx, common.backend.Weight, backend, namespace, common.svcNamespace,
			common.serviceName, clusterDomain, validator, sink)
	case backendRefResolved:
	}

	result := common.backend

	applyGRPCBackendFilters(&result, backend.Filters, sink)

	applyGRPCBackendTransport(ctx, &result, protocolResolver, tlsResolver, clientCert,
		common.svcNamespace, common.serviceName, common.port, sink)

	return result, true
}

// convertGRPCExternalBackendRef builds a gRPC BackendRef for an ExternalBackend
// ref. Like the HTTP convertExternalBackendRef path it emits a sentinel URL the
// controller rewrites from the ExternalBackend spec and runs NO BackendTLSPolicy
// resolver: a BackendTLSPolicy targetRef is Service-shaped and cannot
// legitimately target an ExternalBackend, so resolving here would only risk
// cross-wiring a like-named Service's TLS config (same namespace/name/port) onto
// it. The TLS decision is scheme-driven instead — the backend is left h2c (gRPC
// default), and when the resolved ExternalBackend uses the https scheme the
// controller clears h2c so the default transport negotiates HTTP/2 over ALPN.
func convertGRPCExternalBackendRef(
	ctx context.Context,
	weight int32,
	backend *gatewayv1.GRPCBackendRef,
	namespace, svcNamespace, serviceName, clusterDomain string,
	validator BackendRefValidator,
	sink *diagSink,
) (BackendRef, bool) {
	if !validateCrossNamespace(ctx, svcNamespace, namespace, serviceName, backend.BackendObjectReference, validator) {
		return markInvalidBackend(weight, serviceName, svcNamespace, defaultServicePort, clusterDomain,
			"cross-namespace reference not permitted by ReferenceGrant")
	}

	result := BackendRef{
		Weight:   weight,
		URL:      ExternalBackendSentinelURL(svcNamespace, serviceName),
		Protocol: BackendProtocolH2C,
	}

	applyGRPCBackendFilters(&result, backend.Filters, sink)

	return result, true
}

// applyGRPCBackendTransport resolves the proxy → gRPC-backend transport on
// result: a BackendTLSPolicy (with optional Gateway client cert) puts TLS on the
// wire and ALPN negotiates HTTP/2; with no policy the backend is dialed cleartext
// h2c — unless the Service port declares a TLS appProtocol (https / HTTPS /
// kubernetes.io/wss), in which case the operator asked for TLS but there is no CA
// to verify the backend, so the backend fails closed (HTTP 502, ResolvedRefs=False
// / UnsupportedProtocol) rather than being silently dialed cleartext. Mirrors the
// HTTP path (convertBackendRef → unpolicedTLSAppProtocol). Extracted from
// convertGRPCBackendRef to keep it within the funlen budget.
func applyGRPCBackendTransport(
	ctx context.Context,
	result *BackendRef,
	protocolResolver BackendProtocolResolver,
	tlsResolver BackendTLSResolver,
	clientCert *ClientCertConfig,
	svcNamespace, serviceName string,
	port int32,
	sink *diagSink,
) {
	result.TLS, result.URL = resolveBackendTLS(ctx, tlsResolver, svcNamespace, serviceName, port, result.URL)
	result.TLS = attachGatewayClientCert(result.TLS, clientCert)

	if result.TLS != nil {
		// TLS is on — newTLSTransport handles ALPN HTTP/2 negotiation, the
		// h2c marker would be ignored on that path (and is misleading), so
		// leave Protocol at the default and keep the https:// URL.
		return
	}

	// No BackendTLSPolicy. A TLS appProtocol means the operator asked for TLS but
	// there is no CA to verify the backend — fail closed instead of silently
	// dialing cleartext h2c, the same as the HTTP path. gRPC is HTTP/2 by
	// definition, so every other appProtocol value (unset, h2c, or unrecognised)
	// keeps the h2c default below — the correct gRPC transport regardless.
	if appProto := lookupAppProtocol(ctx, protocolResolver, svcNamespace, serviceName, port); isTLSAppProtocol(appProto) {
		// tlsAttached is false here (past the result.TLS != nil return), so
		// unpolicedTLSAppProtocol always records the ResolvedRefs / UnsupportedProtocol
		// diagnostic and reports fail-closed; call it for that side-effect and set
		// the 502 unconditionally.
		unpolicedTLSAppProtocol(false, svcNamespace, serviceName, port, appProto, sink)

		result.UnavailableStatus = http.StatusBadGateway

		return
	}

	// Backward-compat path: no policy, cleartext appProtocol — force cleartext h2c.
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
