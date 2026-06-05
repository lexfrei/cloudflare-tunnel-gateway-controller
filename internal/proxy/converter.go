package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// configVersionCounter provides monotonically increasing config versions.
//
//nolint:gochecknoglobals // package-level atomic counter is the simplest correct approach
var configVersionCounter atomic.Int64

// init seeds the config version counter from the current time so that versions
// are always higher than any previously issued value. Without this, a controller
// restart would reset the counter to 0 and the proxy's UpdateConfig would reject
// the new (lower) versions as stale.
//
//nolint:gochecknoinits // init is the correct place to seed a package-level atomic counter
func init() {
	configVersionCounter.Store(time.Now().UnixMilli())
}

const (
	defaultServicePort = 80
	httpsPort          = 443
	minPort            = 1
	maxPort            = 65535

	schemeHTTP  = "http"
	schemeHTTPS = "https"
	// schemeHTTPSUpper is the uppercase appProtocol spelling some operators set;
	// treated identically to schemeHTTPS for the TLS-vs-cleartext decision.
	schemeHTTPSUpper = "HTTPS"

	// appProtocolH2C is the Kubernetes Service appProtocol value selecting
	// HTTP/2 cleartext to the backend.
	appProtocolH2C = "kubernetes.io/h2c"
	// appProtocolWS is the Kubernetes Service appProtocol value selecting
	// WebSocket over cleartext to the backend. The WebSocket upgrade itself is
	// decided per-request by the Connection: Upgrade + Upgrade: websocket
	// headers — appProtocol is only a hint for sidecars / metrics. The proxy
	// keeps the default plaintext HTTP/1.1 transport; httputil.ReverseProxy
	// natively handles the 101 Switching Protocols response and hijacks the
	// underlying net.Conn.
	appProtocolWS = "kubernetes.io/ws"
	// appProtocolWSS is the Kubernetes Service appProtocol value selecting
	// WebSocket over TLS to the backend. Same precondition as appProtocol:
	// https — operators MUST attach a BackendTLSPolicy so the proxy has a CA
	// to verify against. Without a policy the dial goes plaintext and the
	// backend will refuse the upgrade.
	appProtocolWSS = "kubernetes.io/wss"
)

// BackendRefValidator checks whether a cross-namespace backend reference is allowed.
// Called only for cross-namespace refs; same-namespace refs are always permitted.
// Returns true if the reference is authorized (e.g., via ReferenceGrant).
type BackendRefValidator func(ctx context.Context, fromNamespace string, ref gatewayv1.BackendObjectReference) bool

// BackendProtocolResolver returns the Service port's appProtocol for a backend
// (e.g. "kubernetes.io/h2c"), or "" when none is set or the Service is unknown.
// It lets the converter pick the backend transport without itself reading the API.
type BackendProtocolResolver func(ctx context.Context, namespace, serviceName string, port int32) string

// BackendTLSResolver returns the TLS config the proxy must apply when dialing
// the given backend Service port, or nil when no BackendTLSPolicy targets it.
// It lets the converter inject TLS settings without itself reading the API.
type BackendTLSResolver func(ctx context.Context, namespace, serviceName string, port int32) *BackendTLSConfig

// ClientCertConfig carries the PEM-encoded TLS client certificate (optionally
// a chain) and matching private key that the proxy must present during backend
// mTLS handshakes. Sourced from a Gateway's
// spec.tls.backend.clientCertificateRef.
type ClientCertConfig struct {
	CertPEM []byte
	KeyPEM  []byte
}

// GatewayClientCertResolver returns the resolved client certificate for the
// Gateway identified by gatewayNN, or nil when that Gateway does not
// configure backend mTLS. The converter calls it once per route (first parent
// wins) and stamps the result onto every backend's BackendTLSConfig.
type GatewayClientCertResolver func(ctx context.Context, gatewayNN types.NamespacedName) *ClientCertConfig

// sortRoutesByPrecedence returns a copy of routes ordered by the Gateway API
// cross-Route match precedence tiebreak (httproute_types.go:192-197 /
// grpcroute_types.go:205-209 / listenerset_types.go:192): oldest Route by
// creationTimestamp first, then alphabetically by {namespace}/{name}. Flattening
// rules in this order makes the router's stable ruleIndex tiebreak resolve
// equal-priority cross-Route ties exactly as the spec mandates, and makes the
// generated config deterministic regardless of the API List order the routes
// arrived in. The input slice is left untouched.
func sortRoutesByPrecedence[T metav1.Object](routes []T) []T {
	sorted := make([]T, len(routes))
	copy(sorted, routes)

	slices.SortStableFunc(sorted, func(left, right T) int {
		lt, rt := left.GetCreationTimestamp(), right.GetCreationTimestamp()
		if !lt.Equal(&rt) {
			if lt.Before(&rt) {
				return -1
			}

			return 1
		}

		if c := strings.Compare(left.GetNamespace(), right.GetNamespace()); c != 0 {
			return c
		}

		return strings.Compare(left.GetName(), right.GetName())
	})

	return sorted
}

// ConvertHTTPRoutes converts Gateway API HTTPRoute resources into a proxy Config.
//
// validator, protocolResolver, tlsResolver and gatewayCertResolver may all be nil:
//   - nil validator: cross-namespace backend refs are accepted unconditionally
//     (used by tests that don't model ReferenceGrant).
//   - nil protocolResolver: backend protocol stays the default HTTP/1.1.
//   - nil tlsResolver: no BackendTLSPolicy is applied; plaintext to backends.
//   - nil gatewayCertResolver: no Gateway-level client certificate is attached
//     to any backend TLS handshake (one-way TLS only).
func ConvertHTTPRoutes(
	ctx context.Context,
	routes []*gatewayv1.HTTPRoute,
	clusterDomain string,
	validator BackendRefValidator,
	protocolResolver BackendProtocolResolver,
	tlsResolver BackendTLSResolver,
	gatewayCertResolver GatewayClientCertResolver,
) *Config {
	cfg := &Config{
		Version: configVersionCounter.Add(1),
	}

	sink := &diagSink{}

	for _, route := range sortRoutesByPrecedence(routes) {
		sink.route(route.Namespace, route.Name)
		hostnames := convertHostnames(route.Spec.Hostnames)
		clientCert := resolveFirstParentClientCert(ctx, route, gatewayCertResolver)

		for ruleIdx := range route.Spec.Rules {
			sink.at(ruleIdx)
			proxyRule := convertHTTPRouteRule(
				ctx, &route.Spec.Rules[ruleIdx], hostnames,
				route.Namespace, clusterDomain, validator, protocolResolver, tlsResolver, clientCert, sink,
			)

			// Rules with no backends and no redirect filter are kept —
			// per Gateway API spec, unresolvable backend refs must return HTTP 500.
			// The proxy handler returns 500 when no backend is available.

			cfg.Rules = append(cfg.Rules, proxyRule)
		}
	}

	cfg.Diagnostics = sink.items

	return cfg
}

// kindGateway identifies the parentRef Kind we recognise when walking
// parents looking for a client certificate. Shared by the HTTPRoute and
// GRPCRoute helpers in this file and grpc_converter.go. The Group is
// matched via gatewayv1.GroupName from the upstream package — no
// proxy-side magic string.
const kindGateway = "Gateway"

// resolveFirstParentClientCert walks the HTTPRoute's parentRefs in declaration
// order, asking gatewayCertResolver for each parent's client certificate, and
// returns the first non-nil result. Multiple parents with conflicting certs
// are a spec edge case the conformance suite does not exercise; this
// "first-wins" rule is documented in docs/gateway-api/limitations.md.
func resolveFirstParentClientCert(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	resolver GatewayClientCertResolver,
) *ClientCertConfig {
	return resolveFirstParentClientCertFromRefs(ctx, route.Spec.ParentRefs, route.Namespace, resolver)
}

// resolveFirstParentClientCertFromRefs is the route-type-agnostic core of the
// parent-cert lookup: it walks ParentReferences directly so HTTPRoute and
// GRPCRoute can share the rule without forcing a generics dance over each
// route type's ParentRefs accessor.
func resolveFirstParentClientCertFromRefs(
	ctx context.Context,
	parentRefs []gatewayv1.ParentReference,
	routeNamespace string,
	resolver GatewayClientCertResolver,
) *ClientCertConfig {
	if resolver == nil {
		return nil
	}

	for _, ref := range parentRefs {
		if ref.Group != nil && *ref.Group != "" && *ref.Group != gatewayv1.GroupName {
			continue
		}

		if ref.Kind != nil && *ref.Kind != "" && *ref.Kind != kindGateway {
			continue
		}

		ns := routeNamespace
		if ref.Namespace != nil {
			ns = string(*ref.Namespace)
		}

		if cert := resolver(ctx, types.NamespacedName{Namespace: ns, Name: string(ref.Name)}); cert != nil {
			return cert
		}
	}

	return nil
}

func convertHostnames(hostnames []gatewayv1.Hostname) []string {
	result := make([]string, 0, len(hostnames))

	for _, hostname := range hostnames {
		result = append(result, string(hostname))
	}

	return result
}

func convertHTTPRouteRule(
	ctx context.Context,
	rule *gatewayv1.HTTPRouteRule,
	hostnames []string,
	namespace string,
	clusterDomain string,
	validator BackendRefValidator,
	resolver BackendProtocolResolver,
	tlsResolver BackendTLSResolver,
	clientCert *ClientCertConfig,
	sink *diagSink,
) RouteRule {
	proxyRule := RouteRule{
		Hostnames: hostnames,
	}

	for _, match := range rule.Matches {
		proxyRule.Matches = append(proxyRule.Matches, convertMatch(match))
	}

	for filterIdx := range rule.Filters {
		converted, failClosed := convertFilter(
			ctx, &rule.Filters[filterIdx], namespace, clusterDomain, validator, tlsResolver, clientCert, sink, filterScopeRule,
		)
		if failClosed {
			proxyRule.UnavailableStatus = http.StatusInternalServerError
		}

		if converted != nil {
			proxyRule.Filters = append(proxyRule.Filters, *converted)
		}
	}

	for backendIdx := range rule.BackendRefs {
		backend, ok := convertBackendRef(ctx, &rule.BackendRefs[backendIdx], namespace, clusterDomain, validator, resolver, tlsResolver, clientCert, sink)
		if ok {
			proxyRule.Backends = append(proxyRule.Backends, backend)
		}
	}

	if rule.Timeouts != nil {
		timeouts, err := convertTimeouts(rule.Timeouts)
		if err != nil {
			// The rule still serves, just without the unparseable timeout, so
			// this is report-only: WholeRule=false drives PartiallyInvalid, not
			// Accepted=False.
			sink.add(
				DiagnosticAccepted,
				string(gatewayv1.RouteReasonUnsupportedValue),
				invalidTimeoutsMessage(err),
				false,
			)
			slog.Warn("dropping invalid route timeouts", "error", err)
		} else {
			proxyRule.Timeouts = timeouts
		}
	}

	warnIfWSResponseFilterStripsHandshake(&proxyRule, sink)

	return proxyRule
}

// wsHandshakeRequiredHeaders is the set of response headers that RFC 6455
// §4.2.2 makes load-bearing for a WebSocket upgrade. Stripping any of them
// in a ResponseHeaderModifier filter on a WS-marked route breaks the
// handshake silently from the client's perspective: the 101 reaches the
// client missing a critical header, and the client just disconnects.
//
// Canonicalised because http.Header.Set/Get/Del normalise on the wire;
// matching against the canonical form keeps the membership check
// case-insensitive without per-call lower-casing.
//
//nolint:gochecknoglobals // RFC 6455 §4.2.2 reference set; package-level so the converter doesn't rebuild it per route
var wsHandshakeRequiredHeaders = map[string]struct{}{
	"Sec-Websocket-Accept": {}, // http.CanonicalMIMEHeaderKey form
	"Upgrade":              {},
	"Connection":           {},
}

// warnIfWSResponseFilterStripsHandshake fires a converter-time WARN when
// a rule on a WebSocket-marked backend carries a ResponseHeaderModifier
// whose Remove list intersects wsHandshakeRequiredHeaders. The proxy's
// runtime path (proxyWebSocketUpgrade) faithfully applies the filter
// pipeline to the 101 response per Gateway API spec, so a Remove list
// that strips a handshake header silently breaks every upgrade on the
// route. The warning surfaces the misconfiguration in controller logs
// at apply time, before the operator's WS clients fail opaquely.
//
// The guard is intentionally scoped to WS-marked backends only.
// Stripping the same headers on a plain-HTTP route is the operator's
// call -- those headers have no special meaning on a regular HTTP
// response, and warning would be noise on every route that defends
// against client-side hijack-via-upgrade attempts.
//
// Filters are inspected at both rule scope (proxyRule.Filters) and
// per-backend scope (backend.Filters); the same shape can land in
// either place via HTTPRouteRule.Filters or HTTPBackendRef.Filters.
func warnIfWSResponseFilterStripsHandshake(rule *RouteRule, sink *diagSink) {
	hasWSBackend := false

	for idx := range rule.Backends {
		if rule.Backends[idx].WebSocket {
			hasWSBackend = true

			break
		}
	}

	if !hasWSBackend {
		return
	}

	// Rule-scope filters apply to every backend on the rule.
	for idx := range rule.Filters {
		warnIfHandshakeStrip(&rule.Filters[idx], filterScopeRule, sink)
	}

	// Per-backend filters: only check filters on WS-marked backends.
	for backendIdx := range rule.Backends {
		if !rule.Backends[backendIdx].WebSocket {
			continue
		}

		for filterIdx := range rule.Backends[backendIdx].Filters {
			warnIfHandshakeStrip(&rule.Backends[backendIdx].Filters[filterIdx], filterScopeBackend, sink)
		}
	}
}

// warnIfHandshakeStrip checks a single RouteFilter for handshake-header removal.
// The filter is honored as written (per spec the pipeline runs unconditionally),
// but stripping a load-bearing WebSocket handshake header silently breaks every
// upgrade on the route, so it is surfaced as a Warning Event diagnostic — an
// operator-authored conflict, not a controller-side drop. Scope ("rule" or
// "backend") goes into the log attributes and the Event message so an operator
// can correlate it back to the exact HTTPRoute field they edited.
func warnIfHandshakeStrip(filter *RouteFilter, scope string, sink *diagSink) {
	if filter.Type != FilterResponseHeaderModifier || filter.ResponseHeaderModifier == nil {
		return
	}

	var offending []string

	// Echo the operator's original casing back in the log; canonicalise
	// only for the membership check. The two differ because stdlib
	// canonicalisation lower-cases the second letter of multi-letter
	// words (`Sec-WebSocket-Accept` -> `Sec-Websocket-Accept`), which
	// is not how operators write the value in HTTPRoute YAML.
	for _, name := range filter.ResponseHeaderModifier.Remove {
		if _, ok := wsHandshakeRequiredHeaders[http.CanonicalHeaderKey(name)]; ok {
			offending = append(offending, name)
		}
	}

	if len(offending) == 0 {
		return
	}

	slog.Warn(
		"ResponseHeaderModifier removes a WebSocket handshake header on a WS-marked backend; clients will fail to complete the upgrade",
		"scope", scope,
		"headers", offending,
	)
	sink.event(EventTypeWarning, wsHandshakeStripMessage(scope, offending))
}

// wsHandshakeStripMessage builds the Event message for a ResponseHeaderModifier
// that strips WebSocket handshake headers.
func wsHandshakeStripMessage(scope string, headers []string) string {
	return fmt.Sprintf(
		"A %s-scope ResponseHeaderModifier removes WebSocket handshake header(s) %v on a WebSocket backend; "+
			"clients will fail to complete the upgrade. Remove %v from the filter's removeHeaders to keep WebSocket working.",
		scope, headers, headers,
	)
}

func convertMatch(match gatewayv1.HTTPRouteMatch) RouteMatch {
	proxyMatch := RouteMatch{}

	if match.Path != nil {
		proxyMatch.Path = convertPathMatch(match.Path)
	}

	if match.Method != nil {
		proxyMatch.Method = string(*match.Method)
	}

	for _, header := range match.Headers {
		proxyMatch.Headers = append(proxyMatch.Headers, convertHeaderMatch(header))
	}

	for _, query := range match.QueryParams {
		proxyMatch.QueryParams = append(proxyMatch.QueryParams, convertQueryMatch(query))
	}

	return proxyMatch
}

func convertPathMatch(pathMatch *gatewayv1.HTTPPathMatch) *PathMatch {
	result := &PathMatch{
		Type: PathMatchPathPrefix,
	}

	if pathMatch.Type != nil {
		switch *pathMatch.Type {
		case gatewayv1.PathMatchExact:
			result.Type = PathMatchExact
		case gatewayv1.PathMatchPathPrefix:
			result.Type = PathMatchPathPrefix
		case gatewayv1.PathMatchRegularExpression:
			result.Type = PathMatchRegularExpression
		}
	}

	if pathMatch.Value != nil {
		result.Value = *pathMatch.Value
	}

	return result
}

func convertHeaderMatch(header gatewayv1.HTTPHeaderMatch) HeaderMatch {
	result := HeaderMatch{
		Name:  string(header.Name),
		Value: header.Value,
		Type:  HeaderMatchExact,
	}

	if header.Type != nil && *header.Type == gatewayv1.HeaderMatchRegularExpression {
		result.Type = HeaderMatchRegularExpression
	}

	return result
}

func convertQueryMatch(query gatewayv1.HTTPQueryParamMatch) QueryParamMatch {
	result := QueryParamMatch{
		Name:  string(query.Name),
		Value: query.Value,
		Type:  QueryParamMatchExact,
	}

	if query.Type != nil && *query.Type == gatewayv1.QueryParamMatchRegularExpression {
		result.Type = QueryParamMatchRegularExpression
	}

	return result
}

// filterScopeRule and filterScopeBackend label where an unsupported filter was
// found, for the diagnostic message and to decide whether the whole rule (rule
// scope) or only a backend fraction (backend scope) fails closed.
const (
	filterScopeRule    = "rule"
	filterScopeBackend = "backend"
)

// convertFilter maps a single HTTPRouteFilter into the proxy wire shape. The
// boolean return reports whether the filter type is unsupported and the rule
// must therefore fail closed: per the Gateway API spec an unresolvable custom
// filter (ExtensionRef) or unknown filter type MUST NOT be silently skipped —
// matched requests MUST receive an HTTP error response, and the route's status
// MUST reflect the UnsupportedValue. The caller stamps the rule (or backend)
// with UnavailableStatus and the converter records a diagnostic via sink. A nil
// return with failClosed=false is a benign skip (nil filter payload, which the
// CRD admission webhook already prevents) and leaves the rule servable.
func convertFilter(
	ctx context.Context,
	filter *gatewayv1.HTTPRouteFilter,
	namespace, clusterDomain string,
	validator BackendRefValidator,
	tlsResolver BackendTLSResolver,
	clientCert *ClientCertConfig,
	sink *diagSink,
	scope string,
) (*RouteFilter, bool) {
	switch filter.Type {
	case gatewayv1.HTTPRouteFilterRequestHeaderModifier:
		return convertRequestHeaderFilter(filter.RequestHeaderModifier), false
	case gatewayv1.HTTPRouteFilterResponseHeaderModifier:
		return convertResponseHeaderFilter(filter.ResponseHeaderModifier), false
	case gatewayv1.HTTPRouteFilterRequestRedirect:
		return convertRedirectFilter(filter.RequestRedirect), false
	case gatewayv1.HTTPRouteFilterURLRewrite:
		return convertURLRewriteFilter(filter.URLRewrite), false
	case gatewayv1.HTTPRouteFilterRequestMirror:
		return convertMirrorFilter(ctx, filter.RequestMirror, namespace, clusterDomain, validator, tlsResolver, clientCert, sink), false
	case gatewayv1.HTTPRouteFilterCORS:
		return convertCORSFilter(filter.CORS), false
	case gatewayv1.HTTPRouteFilterExtensionRef, gatewayv1.HTTPRouteFilterExternalAuth:
		// Unsupported: handled by the fail-closed path below. Listed explicitly so
		// the exhaustive linter confirms every enum value is accounted for.
	}

	// ExtensionRef, ExternalAuth, and any unknown type are unsupported: fail the
	// rule (or backend) closed and surface it on the route status. A rule-scoped
	// filter takes the whole rule down; a backend-scoped one only the backend's
	// traffic fraction.
	sink.add(
		DiagnosticAccepted,
		string(gatewayv1.RouteReasonUnsupportedValue),
		unsupportedFilterMessage(scope, string(filter.Type)),
		scope == filterScopeRule,
	)
	slog.Warn("failing closed: unsupported filter type", "type", filter.Type, "scope", scope)

	return nil, true
}

// unsupportedFilterMessage builds the actionable status message for a filter
// type the proxy cannot serve. It names the offending type, the consequence
// (HTTP 500 for matched requests), and the supported alternatives.
func unsupportedFilterMessage(scope, filterType string) string {
	return fmt.Sprintf(
		"%s filter type %q is not supported; matching requests receive HTTP 500. "+
			"Remove the filter or replace it with a supported type "+
			"(RequestHeaderModifier, ResponseHeaderModifier, RequestRedirect, URLRewrite, RequestMirror, CORS).",
		scope, filterType,
	)
}

func convertRequestHeaderFilter(modifier *gatewayv1.HTTPHeaderFilter) *RouteFilter {
	if modifier == nil {
		slog.Warn("skipping RequestHeaderModifier filter with nil config")

		return nil
	}

	return &RouteFilter{
		Type:                  FilterRequestHeaderModifier,
		RequestHeaderModifier: convertHeaderModifier(modifier),
	}
}

func convertResponseHeaderFilter(modifier *gatewayv1.HTTPHeaderFilter) *RouteFilter {
	if modifier == nil {
		slog.Warn("skipping ResponseHeaderModifier filter with nil config")

		return nil
	}

	return &RouteFilter{
		Type:                   FilterResponseHeaderModifier,
		ResponseHeaderModifier: convertHeaderModifier(modifier),
	}
}

func convertRedirectFilter(redirect *gatewayv1.HTTPRequestRedirectFilter) *RouteFilter {
	if redirect == nil {
		slog.Warn("skipping RequestRedirect filter with nil config")

		return nil
	}

	return &RouteFilter{
		Type:            FilterRequestRedirect,
		RequestRedirect: convertRedirectConfig(redirect),
	}
}

func convertURLRewriteFilter(rewrite *gatewayv1.HTTPURLRewriteFilter) *RouteFilter {
	if rewrite == nil {
		slog.Warn("skipping URLRewrite filter with nil config")

		return nil
	}

	return &RouteFilter{
		Type:       FilterURLRewrite,
		URLRewrite: convertURLRewrite(rewrite),
	}
}

const (
	// ServiceImportGroup is the API group of a multicluster.x-k8s.io ServiceImport.
	ServiceImportGroup = "multicluster.x-k8s.io"
	// ServiceImportKind is the Kind of a ServiceImport backendRef.
	ServiceImportKind = "ServiceImport"
	// ClustersetDomain is the DNS domain under which multicluster (ServiceImport)
	// Services resolve, per the KEP-1645 / mcs-api convention.
	ClustersetDomain = "clusterset.local"
)

// IsServiceBackendRef reports whether the BackendObjectReference points at a
// core Service (the default Kind when Group/Kind are nil). Exported so the
// controller package can reuse the same predicate without duplicating it.
//
// This predicate intentionally stays Service-only: callers that key Service
// EndpointSlice lookups, the zero-endpoint 503 probe, or BackendTLSPolicy
// targeting on it must NOT begin matching ServiceImport/ExternalBackend.
func IsServiceBackendRef(ref gatewayv1.BackendObjectReference) bool {
	if ref.Group != nil && *ref.Group != "" && *ref.Group != "core" {
		return false
	}

	if ref.Kind != nil && *ref.Kind != "Service" {
		return false
	}

	return true
}

// IsServiceImportBackendRef reports whether the ref targets a
// multicluster.x-k8s.io ServiceImport. Unlike Service, ServiceImport has no
// implicit default, so the group must be set explicitly.
func IsServiceImportBackendRef(ref gatewayv1.BackendObjectReference) bool {
	return ref.Group != nil && string(*ref.Group) == ServiceImportGroup &&
		ref.Kind != nil && string(*ref.Kind) == ServiceImportKind
}

// IsSupportedBackendRef reports whether the ref is a backend kind the proxy can
// resolve to a dialable URL: a core Service, a multicluster ServiceImport, or a
// cf.k8s.lex.la ExternalBackend.
func IsSupportedBackendRef(ref gatewayv1.BackendObjectReference) bool {
	return IsServiceBackendRef(ref) || IsServiceImportBackendRef(ref) || IsExternalBackendRef(ref)
}

// isMirrorableBackendRef reports whether the ref can be a RequestMirror
// destination. Only kinds with an in-cluster DNS form qualify (Service,
// ServiceImport): the converter builds the mirror URL directly via
// buildServiceURL, with no controller-side sentinel rewrite, so an
// ExternalBackend — whose URL lives in its spec — cannot be a mirror target.
func isMirrorableBackendRef(ref gatewayv1.BackendObjectReference) bool {
	return IsServiceBackendRef(ref) || IsServiceImportBackendRef(ref)
}

// backendDomain returns the DNS domain the backend resolves under: the
// clusterset domain for a ServiceImport, otherwise the local cluster domain.
func backendDomain(ref gatewayv1.BackendObjectReference, clusterDomain string) string {
	if IsServiceImportBackendRef(ref) {
		return ClustersetDomain
	}

	return clusterDomain
}

// mirrorDropDiagnostic records a ResolvedRefs-target diagnostic for a dropped
// RequestMirror. The mirror is best-effort — dropping it does not break the
// main request — so it is report-only (WholeRule=false): the route stays
// Accepted, the dropped mirror shows on ResolvedRefs.
func mirrorDropDiagnostic(sink *diagSink, reason gatewayv1.RouteConditionReason, mirrorName, detail string) {
	sink.add(
		DiagnosticResolvedRefs,
		string(reason),
		fmt.Sprintf("The RequestMirror to Service %q was dropped because %s; the main request is unaffected.", mirrorName, detail),
		false,
	)
}

// validateMirrorBackendRef validates a RequestMirror's backendRef and, on any
// failure, records a report-only ResolvedRefs diagnostic (a dropped mirror does
// not break the main request) and returns ok=false. On success it returns the
// resolved namespace and port for the mirror destination.
func validateMirrorBackendRef(
	ctx context.Context,
	mirror *gatewayv1.HTTPRequestMirrorFilter,
	namespace string,
	validator BackendRefValidator,
	sink *diagSink,
) (string, int32, bool) {
	mirrorName := string(mirror.BackendRef.Name)

	// A mirror destination must have an in-cluster DNS form (Service or
	// ServiceImport). An ExternalBackend is excluded: the converter has no
	// client to resolve its real URL and the sentinel-rewrite step does not walk
	// mirror filters, so accepting it would silently mirror to a bogus
	// cluster-local address. ExternalBackend is a valid primary backend, just
	// not a mirror destination.
	if !isMirrorableBackendRef(mirror.BackendRef) {
		mirrorDropDiagnostic(sink, gatewayv1.RouteReasonInvalidKind, mirrorName,
			"its backendRef must be a Service or ServiceImport (ExternalBackend is not supported as a mirror destination)")
		slog.Warn("skipping mirror with unsupported backend kind",
			"kind", mirror.BackendRef.Kind, "name", mirror.BackendRef.Name)

		return "", 0, false
	}

	mirrorPort := int32(defaultServicePort)
	if mirror.BackendRef.Port != nil {
		mirrorPort = *mirror.BackendRef.Port
	}

	if !validatePort(mirrorPort) {
		mirrorDropDiagnostic(sink, gatewayv1.RouteReasonUnsupportedValue, mirrorName,
			fmt.Sprintf("its backendRef port %d is out of range", mirrorPort))
		slog.Warn("skipping mirror with invalid port", "service", mirrorName, "port", mirrorPort)

		return "", 0, false
	}

	mirrorNS := namespace
	if mirror.BackendRef.Namespace != nil {
		mirrorNS = string(*mirror.BackendRef.Namespace)
	}

	if !validateCrossNamespace(ctx, mirrorNS, namespace, mirrorName, mirror.BackendRef, validator) {
		mirrorDropDiagnostic(sink, gatewayv1.RouteReasonRefNotPermitted, mirrorName,
			fmt.Sprintf("its cross-namespace backendRef to %q is not permitted by a ReferenceGrant", mirrorNS))

		return "", 0, false
	}

	return mirrorNS, mirrorPort, true
}

func convertMirrorFilter(
	ctx context.Context,
	mirror *gatewayv1.HTTPRequestMirrorFilter,
	namespace, clusterDomain string,
	validator BackendRefValidator,
	tlsResolver BackendTLSResolver,
	clientCert *ClientCertConfig,
	sink *diagSink,
) *RouteFilter {
	if mirror == nil {
		return nil
	}

	mirrorNS, mirrorPort, ok := validateMirrorBackendRef(ctx, mirror, namespace, validator, sink)
	if !ok {
		return nil
	}

	mirrorURL := buildServiceURL(string(mirror.BackendRef.Name), mirrorNS, mirrorPort, backendDomain(mirror.BackendRef, clusterDomain))

	mirrorConfig := &MirrorConfig{
		BackendURL: mirrorURL,
		Percent:    mirrorPercent(mirror),
	}

	// Resolve any BackendTLSPolicy targeting the mirror destination and
	// stamp the resulting TLS config on MirrorConfig. When the proxy
	// compiles this filter, the mirror Client borrows a per-cert
	// RoundTripper from the Handler's transport pool — same shape the
	// main leg uses — so the mirror dial actually honors the operator's
	// TLS expectation instead of silently bypassing it.
	//
	// The parent Gateway's clientCertificateRef is stamped on top of the
	// TLS config the same way attachGatewayClientCert handles it for the
	// main leg: with both inputs non-nil, the mirror leg does mTLS too.
	if tlsResolver != nil {
		if tls := tlsResolver(ctx, mirrorNS, string(mirror.BackendRef.Name), mirrorPort); tls != nil {
			mirrorConfig.TLS = attachGatewayClientCert(tls, clientCert)
			mirrorConfig.BackendURL = forceHTTPSScheme(mirrorURL)
		}
	}

	return &RouteFilter{
		Type:          FilterRequestMirror,
		RequestMirror: mirrorConfig,
	}
}

// forceHTTPSScheme rewrites the scheme of rawURL to https. Used when the
// mirror destination carries a BackendTLSPolicy: the URL must dial over
// TLS regardless of what buildServiceURL emitted for the port.
func forceHTTPSScheme(rawURL string) string {
	if rest, ok := strings.CutPrefix(rawURL, schemeHTTP+"://"); ok {
		return schemeHTTPS + "://" + rest
	}

	return rawURL
}

// convertCORSFilter maps the upstream HTTPCORSFilter into the proxy's
// CORSConfig wire shape. Returns nil (silently skipped, with a warning) when
// the .CORS payload is missing — that's a malformed HTTPRoute that the CRD
// admission webhook would normally block, but the converter must not panic
// or ship a half-config. Optional fields:
//
//   - AllowCredentials is a *bool upstream; nil → false in the proxy config.
//   - MaxAge stays zero when omitted; the proxy applies the spec default
//     (5 seconds) at emit time so the controller doesn't need to mirror
//     CRD-default logic that may shift in future Gateway API releases.
func convertCORSFilter(cors *gatewayv1.HTTPCORSFilter) *RouteFilter {
	if cors == nil {
		slog.Warn("skipping CORS filter with nil config")

		return nil
	}

	cfg := &CORSConfig{
		MaxAge: cors.MaxAge,
	}

	if cors.AllowCredentials != nil {
		cfg.AllowCredentials = *cors.AllowCredentials
	}

	if len(cors.AllowOrigins) > 0 {
		cfg.AllowOrigins = make([]string, 0, len(cors.AllowOrigins))
		for _, origin := range cors.AllowOrigins {
			cfg.AllowOrigins = append(cfg.AllowOrigins, string(origin))
		}
	}

	if len(cors.AllowMethods) > 0 {
		cfg.AllowMethods = make([]string, 0, len(cors.AllowMethods))
		for _, method := range cors.AllowMethods {
			cfg.AllowMethods = append(cfg.AllowMethods, string(method))
		}
	}

	if len(cors.AllowHeaders) > 0 {
		cfg.AllowHeaders = make([]string, 0, len(cors.AllowHeaders))
		for _, header := range cors.AllowHeaders {
			cfg.AllowHeaders = append(cfg.AllowHeaders, string(header))
		}
	}

	if len(cors.ExposeHeaders) > 0 {
		cfg.ExposeHeaders = make([]string, 0, len(cors.ExposeHeaders))
		for _, header := range cors.ExposeHeaders {
			cfg.ExposeHeaders = append(cfg.ExposeHeaders, string(header))
		}
	}

	return &RouteFilter{
		Type: FilterCORS,
		CORS: cfg,
	}
}

// mirrorPercent normalises HTTPRequestMirrorFilter.Percent and Fraction into a
// single 0-100 value. Returns nil when neither is set (i.e. mirror everything).
// Per Gateway API only one of Percent or Fraction may be specified.
//
// Always returns a freshly allocated pointer so the proxy config is fully
// detached from the HTTPRoute object's storage. Arithmetic uses int64 to avoid
// overflow when a Fraction uses large numerator/denominator values that are
// individually valid int32 but whose product overflows. The result is clamped
// to [0, 100] defensively, even though the CRD already enforces this — the
// proxy data plane treats out-of-range as a programming error and snapping to
// the nearest legal value is safer than producing UB in shouldMirror.
func mirrorPercent(mirror *gatewayv1.HTTPRequestMirrorFilter) *int32 {
	if mirror.Percent != nil {
		clamped := clampPercent(int64(*mirror.Percent))

		return &clamped
	}

	if mirror.Fraction == nil {
		return nil
	}

	denominator := int32(100)
	if mirror.Fraction.Denominator != nil {
		denominator = *mirror.Fraction.Denominator
	}

	if denominator <= 0 {
		slog.Warn("skipping mirror Fraction with non-positive denominator",
			"numerator", mirror.Fraction.Numerator,
			"denominator", denominator,
		)

		return nil
	}

	// int64 arithmetic prevents Numerator*100 from wrapping on large
	// Numerators (CRD validation only requires Numerator <= Denominator,
	// not a Maximum cap, so values in the billions are legal input).
	resolved := clampPercent(int64(mirror.Fraction.Numerator) * 100 / int64(denominator))

	return &resolved
}

// clampPercent snaps an arbitrary integer to the [0, 100] range and narrows
// the type to int32 (the wire format used by MirrorConfig.Percent).
func clampPercent(value int64) int32 {
	if value < 0 {
		return 0
	}

	if value > 100 {
		return 100
	}

	return int32(value)
}

func convertHeaderModifier(modifier *gatewayv1.HTTPHeaderFilter) *HeaderModifier {
	result := &HeaderModifier{}

	for _, header := range modifier.Set {
		result.Set = append(result.Set, HeaderValue{
			Name:  string(header.Name),
			Value: header.Value,
		})
	}

	for _, header := range modifier.Add {
		result.Add = append(result.Add, HeaderValue{
			Name:  string(header.Name),
			Value: header.Value,
		})
	}

	result.Remove = append(result.Remove, modifier.Remove...)

	return result
}

func convertRedirectConfig(redirect *gatewayv1.HTTPRequestRedirectFilter) *RedirectConfig {
	result := &RedirectConfig{}

	if redirect.Scheme != nil {
		result.Scheme = redirect.Scheme
	}

	if redirect.Hostname != nil {
		hostname := string(*redirect.Hostname)
		result.Hostname = &hostname
	}

	if redirect.Port != nil {
		port := *redirect.Port
		result.Port = &port
	}

	if redirect.Path != nil {
		result.Path = convertRedirectPath(redirect.Path)
	}

	if redirect.StatusCode != nil {
		result.StatusCode = redirect.StatusCode
	}

	return result
}

func convertRedirectPath(pathModifier *gatewayv1.HTTPPathModifier) *RedirectPath {
	switch pathModifier.Type {
	case gatewayv1.FullPathHTTPPathModifier:
		if pathModifier.ReplaceFullPath == nil {
			return nil
		}

		return &RedirectPath{
			Type:  RedirectPathFullReplace,
			Value: *pathModifier.ReplaceFullPath,
		}
	case gatewayv1.PrefixMatchHTTPPathModifier:
		if pathModifier.ReplacePrefixMatch == nil {
			return nil
		}

		return &RedirectPath{
			Type:  RedirectPathPrefixReplace,
			Value: *pathModifier.ReplacePrefixMatch,
		}
	default:
		return nil
	}
}

func convertURLRewrite(rewrite *gatewayv1.HTTPURLRewriteFilter) *URLRewriteConfig {
	result := &URLRewriteConfig{}

	if rewrite.Hostname != nil {
		hostname := string(*rewrite.Hostname)
		result.Hostname = &hostname
	}

	if rewrite.Path != nil {
		result.Path = convertURLRewritePath(rewrite.Path)
	}

	return result
}

func convertURLRewritePath(pathModifier *gatewayv1.HTTPPathModifier) *URLRewritePath {
	switch pathModifier.Type {
	case gatewayv1.FullPathHTTPPathModifier:
		return &URLRewritePath{
			Type:            URLRewriteFullPath,
			ReplaceFullPath: pathModifier.ReplaceFullPath,
		}
	case gatewayv1.PrefixMatchHTTPPathModifier:
		return &URLRewritePath{
			Type:               URLRewritePrefixMatch,
			ReplacePrefixMatch: pathModifier.ReplacePrefixMatch,
		}
	default:
		return nil
	}
}

func convertBackendRef(
	ctx context.Context,
	backend *gatewayv1.HTTPBackendRef,
	namespace string,
	clusterDomain string,
	validator BackendRefValidator,
	resolver BackendProtocolResolver,
	tlsResolver BackendTLSResolver,
	clientCert *ClientCertConfig,
	sink *diagSink,
) (BackendRef, bool) {
	result, ok := initBackendRefBaseline(backend)
	if !ok {
		return BackendRef{}, false
	}

	serviceName := string(backend.Name)

	port := int32(defaultServicePort)
	if backend.Port != nil {
		port = *backend.Port
	}

	svcNamespace := resolveBackendNamespace(backend, namespace)

	// An invalid backendRef MUST return 500 for its traffic fraction per the
	// Gateway API spec, not be silently dropped (which would hand its share to
	// the valid siblings). A weight>0 invalid ref therefore stays in the
	// weighted pool marked 500; a weight-0 ref carries no traffic and is
	// dropped. markInvalidBackend enforces that.
	if !IsSupportedBackendRef(backend.BackendObjectReference) {
		return markInvalidBackend(result.Weight, serviceName, svcNamespace, port, clusterDomain, "unsupported backend kind")
	}

	// An ExternalBackend's URL lives in its spec (resolved controller-side); its
	// backendRef port is ignored in favour of spec.port, so skip port validation.
	if IsExternalBackendRef(backend.BackendObjectReference) {
		return convertExternalBackendRef(ctx, result.Weight, backend, namespace, svcNamespace, serviceName,
			clusterDomain, validator, tlsResolver, clientCert, sink)
	}

	if !validatePort(port) {
		return markInvalidBackend(result.Weight, serviceName, svcNamespace, port, clusterDomain, "invalid port")
	}

	if !validateCrossNamespace(ctx, svcNamespace, namespace, serviceName, backend.BackendObjectReference, validator) {
		return markInvalidBackend(result.Weight, serviceName, svcNamespace, port, clusterDomain,
			"cross-namespace reference not permitted by ReferenceGrant")
	}

	result.URL = buildServiceURL(serviceName, svcNamespace, port, backendDomain(backend.BackendObjectReference, clusterDomain))

	// Resolve TLS first so the protocol resolver can know whether to silently
	// pass through `appProtocol: https` (policy attached → suppressed) or warn
	// (no policy → operator misconfigured a TLS hint with no actual TLS).
	result.TLS, result.URL = resolveBackendTLS(ctx, tlsResolver, svcNamespace, serviceName, port, result.URL)
	result.TLS = attachGatewayClientCert(result.TLS, clientCert)

	var protoFailClosed bool

	result.Protocol, result.URL, result.WebSocket, protoFailClosed = resolveBackendProtocol(
		ctx, resolver, svcNamespace, serviceName, port, result.URL, result.TLS != nil, sink,
	)
	if protoFailClosed {
		// A TLS appProtocol without a BackendTLSPolicy: dialing plaintext would
		// fail anyway, so return 502 for this backend's traffic fraction.
		result.UnavailableStatus = http.StatusBadGateway
	}

	applyBackendFilters(ctx, &result, backend.Filters, namespace, clusterDomain, validator, tlsResolver, clientCert, sink)

	return result, true
}

// convertExternalBackendRef builds a BackendRef for an ExternalBackend ref. The
// converter has no Kubernetes client, so it cannot read the ExternalBackend's
// scheme/host/port; it emits a sentinel URL (encoding namespace/name) that the
// controller rewrites to the real URL before pushing the config. Cross-namespace
// refs are still validated here against a ReferenceGrant keyed on the
// ExternalBackend group/kind; per-backend filters are applied as usual.
func convertExternalBackendRef(
	ctx context.Context,
	weight int32,
	backend *gatewayv1.HTTPBackendRef,
	namespace, svcNamespace, serviceName, clusterDomain string,
	validator BackendRefValidator,
	tlsResolver BackendTLSResolver,
	clientCert *ClientCertConfig,
	sink *diagSink,
) (BackendRef, bool) {
	if !validateCrossNamespace(ctx, svcNamespace, namespace, serviceName, backend.BackendObjectReference, validator) {
		return markInvalidBackend(weight, serviceName, svcNamespace, defaultServicePort, clusterDomain,
			"cross-namespace reference not permitted by ReferenceGrant")
	}

	result := BackendRef{Weight: weight, URL: ExternalBackendSentinelURL(svcNamespace, serviceName)}
	applyBackendFilters(ctx, &result, backend.Filters, namespace, clusterDomain, validator, tlsResolver, clientCert, sink)

	return result, true
}

// applyBackendFilters resolves per-backend HTTPRoute filters onto result. An
// unsupported filter fails closed for this backend's traffic fraction (HTTP
// 500), matching an invalid backendRef rather than serving without the filter.
func applyBackendFilters(
	ctx context.Context,
	result *BackendRef,
	filters []gatewayv1.HTTPRouteFilter,
	namespace, clusterDomain string,
	validator BackendRefValidator,
	tlsResolver BackendTLSResolver,
	clientCert *ClientCertConfig,
	sink *diagSink,
) {
	var failClosed bool

	result.Filters, failClosed = convertBackendFilters(ctx, filters, namespace, clusterDomain, validator, tlsResolver, clientCert, sink)
	if failClosed {
		result.UnavailableStatus = http.StatusInternalServerError
	}
}

// markInvalidBackend handles a backendRef that failed validation (unsupported
// Kind, invalid port, or unauthorized cross-namespace reference). Per the
// Gateway API spec an invalid backendRef MUST return 500 for the proportion of
// traffic that would have been routed to it. A backend that carries traffic
// (weight > 0) is therefore kept in the weighted pool with UnavailableStatus
// set to 500 — the handler returns that status without dialing, so the
// placeholder URL is never contacted — while a zero-weight invalid backend is
// dropped because it carries no traffic and so loses no fraction.
//
// The placeholder URL is built from the ref's own name/namespace/port (falling
// back to the default port when the port itself is what's invalid) purely so
// the backend has a stable, parseable host for the router; it is never dialed.
//
// Note on the invalid-port case: the controller's separate failed-ref path
// (ingress builder → markUnavailableBackends) reports the real, out-of-range
// port, so its host key (e.g. ...:70000) does NOT match this placeholder's
// (...:80) and that redundant marking is a no-op. That is harmless precisely
// because this function already marked the backend 500 inline — the 500 does
// not depend on the controller's path matching. The two paths are independent
// and either one alone is sufficient.
func markInvalidBackend(weight int32, name, svcNamespace string, port int32, clusterDomain, reason string) (BackendRef, bool) {
	if weight <= 0 {
		slog.Warn("dropping zero-weight invalid backend", "service", name, "reason", reason)

		return BackendRef{}, false
	}

	urlPort := port
	if !validatePort(urlPort) {
		urlPort = int32(defaultServicePort)
	}

	slog.Warn("marking invalid backend 500 for its traffic fraction",
		"service", name, "reason", reason, "weight", weight)

	return BackendRef{
		Weight:            weight,
		URL:               buildServiceURL(name, svcNamespace, urlPort, clusterDomain),
		UnavailableStatus: http.StatusInternalServerError,
	}, true
}

// initBackendRefBaseline validates a backend ref's weight, returning a
// partially-initialised BackendRef when the weight is non-negative. Extracted
// from convertBackendRef to keep that function under the funlen budget.
func initBackendRefBaseline(backend *gatewayv1.HTTPBackendRef) (BackendRef, bool) {
	result := BackendRef{Weight: 1}
	if backend.Weight != nil {
		result.Weight = *backend.Weight
	}

	if result.Weight < 0 {
		slog.Warn("skipping backend with negative weight",
			"name", string(backend.Name),
			"weight", result.Weight,
		)

		return BackendRef{}, false
	}

	return result, true
}

// attachGatewayClientCert returns a backend TLS config that carries the
// Gateway-level client keypair stamped on top of the supplied policy config.
// When either input is nil — no BackendTLSPolicy targets the backend, or the
// parent Gateway does not configure clientCertificateRef — the original
// tlsCfg is returned unchanged. Per Gateway API spec the client cert is
// presented only when backend TLS is active; sending a cert over plaintext
// makes no sense.
//
// The supplied tlsCfg is treated as immutable: a shallow struct copy is
// taken before mutation, and the two slice fields (SubjectAltNames,
// SubjectAltNameURIs) are slices.Clone'd so a downstream append on
// `stamped` cannot reach back into the resolver's cached *BackendTLSConfig
// and cross-contaminate sibling backends. This is defensive — the current
// converter never appends to those fields here — but the cost is one
// short slice allocation per matched backend and the guarantee is
// enforced by code, not a comment.
func attachGatewayClientCert(tlsCfg *BackendTLSConfig, clientCert *ClientCertConfig) *BackendTLSConfig {
	if tlsCfg == nil || clientCert == nil {
		return tlsCfg
	}

	stamped := *tlsCfg
	stamped.SubjectAltNames = slices.Clone(tlsCfg.SubjectAltNames)
	stamped.SubjectAltNameURIs = slices.Clone(tlsCfg.SubjectAltNameURIs)
	stamped.ClientCertPEM = clientCert.CertPEM
	stamped.ClientKeyPEM = clientCert.KeyPEM

	return &stamped
}

// resolveBackendTLS applies the BackendTLSPolicy resolver. When a policy
// targets the backend, the returned TLS config is attached and the URL scheme
// is forced to https (buildServiceURL's port-443 heuristic alone misses TLS on
// non-standard ports).
func resolveBackendTLS(
	ctx context.Context,
	resolver BackendTLSResolver,
	namespace, serviceName string,
	port int32,
	rawURL string,
) (*BackendTLSConfig, string) {
	if resolver == nil {
		return nil, rawURL
	}

	tls := resolver(ctx, namespace, serviceName, port)
	if tls == nil {
		return nil, rawURL
	}

	return tls, forceHTTPSScheme(rawURL)
}

// resolveBackendProtocol applies the protocol resolver to a backend reference
// and adjusts the URL scheme accordingly. h2c is cleartext, so an https://
// scheme (which buildServiceURL emits for port 443) is rewritten to http://
// to avoid a silent TLS-vs-plaintext mismatch.
//
// kubernetes.io/ws is accepted silently — the WebSocket upgrade is decided
// per-request by Connection: Upgrade headers, not by transport selection, so
// the default plaintext HTTP/1.1 transport is exactly right. kubernetes.io/wss
// mirrors the `appProtocol: https` precondition: with a BackendTLSPolicy
// attached the URL is already https://, without one the proxy logs a WARN and
// the backend will refuse the upgrade. Unrecognised appProtocol values fall
// through to a generic "unsupported" WARN.
//
// `tlsAttached` reports whether a BackendTLSPolicy already applied a TLS config
// to this backend. When the operator declared `appProtocol: https`/`wss` (or
// `HTTPS`) but no policy attached, the proxy would otherwise silently dial
// plaintext — defeating the operator's stated TLS intent. In that case the
// function logs a WARN so the misconfiguration is visible.
//
// The third return value reports whether the appProtocol marks this backend
// as WebSocket-capable (true for `kubernetes.io/ws` and `kubernetes.io/wss`,
// false otherwise). The Handler uses this to gate the upgrade-aware timeout
// skip on operator declaration, not on client-controlled headers — gating
// solely on `Connection: Upgrade` would let any request bypass the route's
// declared timeouts.
func resolveBackendProtocol(
	ctx context.Context,
	resolver BackendProtocolResolver,
	namespace, serviceName string,
	port int32,
	rawURL string,
	tlsAttached bool,
	sink *diagSink,
) (BackendProtocol, string, bool, bool) {
	if resolver == nil {
		return BackendProtocolHTTP, rawURL, false, false
	}

	appProto := resolver(ctx, namespace, serviceName, port)

	switch appProto {
	case appProtocolH2C:
		proto, u := resolveH2C(rawURL, tlsAttached, namespace, serviceName, port, sink)

		return proto, u, false, false
	case appProtocolWS:
		if tlsAttached {
			warnCleartextHintSuppressed(
				"appProtocol kubernetes.io/ws suppressed by BackendTLSPolicy on the same Service — WebSocket will run over TLS (consider appProtocol kubernetes.io/wss instead)",
				namespace, serviceName, port, sink)
		}

		return BackendProtocolHTTP, rawURL, true, false
	case appProtocolWSS:
		// TLS-bearing WebSocket: without a BackendTLSPolicy the proxy has no
		// trust anchor and the upgrade would fail. Fail the backend closed
		// (502) and surface it, instead of dialing plaintext to a TLS backend.
		fc := unpolicedTLSAppProtocol(tlsAttached, namespace, serviceName, port, appProto, sink)

		return BackendProtocolHTTP, rawURL, true, fc
	case schemeHTTPS, schemeHTTPSUpper:
		fc := unpolicedTLSAppProtocol(tlsAttached, namespace, serviceName, port, appProto, sink)

		return BackendProtocolHTTP, rawURL, false, fc
	case "", "http", "HTTP":
		// Default / cleartext hints match the default transport — silent.
		return BackendProtocolHTTP, rawURL, false, false
	default:
		// Unrecognised appProtocol: HTTP/1.1 is a safe default the backend may
		// well speak, so keep serving (report-only) but surface that the hint
		// was not honoured.
		sink.add(
			DiagnosticResolvedRefs,
			string(gatewayv1.RouteReasonUnsupportedProtocol),
			unknownAppProtocolMessage(serviceName, appProto),
			false,
		)
		slog.Warn("unsupported backend appProtocol; falling back to HTTP/1.1",
			"namespace", namespace,
			"service", serviceName,
			"port", port,
			"appProtocol", appProto,
		)

		return BackendProtocolHTTP, rawURL, false, false
	}
}

// isTLSAppProtocol reports whether a Service-port appProtocol selects a TLS
// transport (https, HTTPS, or kubernetes.io/wss). These values require a
// BackendTLSPolicy; without one the backend must fail closed rather than be
// dialed cleartext. Kept in sync with the TLS-bearing cases of
// resolveBackendProtocol, and reused by the gRPC transport path
// (applyGRPCBackendTransport) so both routes make the same TLS-vs-cleartext call.
func isTLSAppProtocol(appProto string) bool {
	switch appProto {
	case schemeHTTPS, schemeHTTPSUpper, appProtocolWSS:
		return true
	default:
		return false
	}
}

// lookupAppProtocol returns the Service-port appProtocol via the resolver, or ""
// when the resolver is nil (no backend-protocol resolution wired in).
func lookupAppProtocol(ctx context.Context, resolver BackendProtocolResolver, namespace, serviceName string, port int32) string {
	if resolver == nil {
		return ""
	}

	return resolver(ctx, namespace, serviceName, port)
}

// unpolicedTLSAppProtocol handles a TLS-bearing appProtocol (https, wss). When a
// BackendTLSPolicy is attached the hint is honoured silently. Without one the
// proxy has no CA to verify against, so per the Gateway API spec this is an
// unsupported app protocol: the backend fails closed (502) and a
// ResolvedRefs-target diagnostic is recorded. Returns whether the backend must
// fail closed.
func unpolicedTLSAppProtocol(tlsAttached bool, namespace, serviceName string, port int32, appProto string, sink *diagSink) bool {
	if tlsAttached {
		return false
	}

	sink.add(
		DiagnosticResolvedRefs,
		string(gatewayv1.RouteReasonUnsupportedProtocol),
		unpolicedTLSMessage(serviceName, appProto),
		false,
	)
	slog.Warn("failing backend closed: TLS appProtocol without a BackendTLSPolicy",
		"namespace", namespace,
		"service", serviceName,
		"port", port,
		"appProtocol", appProto,
	)

	return true
}

// unpolicedTLSMessage builds the actionable status message for a TLS appProtocol
// declared without a BackendTLSPolicy.
func unpolicedTLSMessage(serviceName, appProto string) string {
	return fmt.Sprintf(
		"Service %q declares appProtocol %q but no BackendTLSPolicy targets it; "+
			"the proxy has no CA to verify the backend, so requests to it receive HTTP 502. "+
			"Attach a BackendTLSPolicy to the Service to enable TLS to this backend.",
		serviceName, appProto,
	)
}

// unknownAppProtocolMessage builds the status message for an unrecognised
// appProtocol value the proxy falls back to HTTP/1.1 for.
func unknownAppProtocolMessage(serviceName, appProto string) string {
	return fmt.Sprintf(
		"Service %q declares an unsupported appProtocol %q; the proxy serves it over HTTP/1.1. "+
			"Use a supported value (kubernetes.io/h2c, kubernetes.io/ws, kubernetes.io/wss, https) "+
			"or remove the appProtocol if HTTP/1.1 is correct.",
		serviceName, appProto,
	)
}

// resolveH2C handles the `kubernetes.io/h2c` branch. Extracted so
// resolveBackendProtocol stays within the funlen budget while keeping the
// behaviour and comments next to the rest of the protocol-dispatch logic.
//
// When a BackendTLSPolicy attached, TLS wins (cleartext h2c cannot coexist
// with TLS). Leave the URL https:// so stdlib applies TLSClientConfig and
// HTTP/2 is negotiated over the ALPN handshake; emit a WARN about the
// suppressed h2c hint so the operator sees the conflict. Otherwise rewrite
// any https:// URL (which buildServiceURL emits for port 443) back to
// http:// so the h2c transport dials cleartext.
func resolveH2C(rawURL string, tlsAttached bool, namespace, serviceName string, port int32, sink *diagSink) (BackendProtocol, string) {
	if tlsAttached {
		warnCleartextHintSuppressed(
			"appProtocol kubernetes.io/h2c suppressed by BackendTLSPolicy on the same Service — HTTP/2 will be negotiated over TLS via ALPN",
			namespace, serviceName, port, sink)

		return BackendProtocolHTTP, rawURL
	}

	return BackendProtocolH2C, strings.Replace(rawURL, schemeHTTPS+"://", schemeHTTP+"://", 1)
}

// warnCleartextHintSuppressed logs a WARN and records a Normal Event diagnostic
// when an operator declared a cleartext appProtocol (h2c, ws) but a
// BackendTLSPolicy attached to the same Service forced TLS anyway. The proxy
// still does the right thing — TLS wins because it's the higher-priority signal
// — so this is a benign override surfaced as a Kubernetes Event, not a
// condition: the route is fully Accepted, the operator is just told the
// contradictory hint was superseded.
func warnCleartextHintSuppressed(message, namespace, serviceName string, port int32, sink *diagSink) {
	slog.Warn(message,
		"namespace", namespace,
		"service", serviceName,
		"port", port,
	)
	sink.event(EventTypeNormal, message)
}

// convertBackendFilters converts the per-backend HTTPRouteFilters into proxy
// RouteFilters, skipping any that aren't supported or have nil config.
// clientCert is forwarded so a per-backend RequestMirror filter on a
// destination that also carries a BackendTLSPolicy receives the parent
// Gateway's client cert the same way a rule-level mirror does.
func convertBackendFilters(
	ctx context.Context,
	filters []gatewayv1.HTTPRouteFilter,
	namespace, clusterDomain string,
	validator BackendRefValidator,
	tlsResolver BackendTLSResolver,
	clientCert *ClientCertConfig,
	sink *diagSink,
) ([]RouteFilter, bool) {
	if len(filters) == 0 {
		return nil, false
	}

	result := make([]RouteFilter, 0, len(filters))

	var failClosed bool

	for filterIdx := range filters {
		converted, filterFailClosed := convertFilter(
			ctx, &filters[filterIdx], namespace, clusterDomain, validator, tlsResolver, clientCert, sink, filterScopeBackend,
		)
		if filterFailClosed {
			failClosed = true
		}

		if converted != nil {
			result = append(result, *converted)
		}
	}

	return result, failClosed
}

func resolveBackendNamespace(backend *gatewayv1.HTTPBackendRef, fallback string) string {
	if backend.Namespace != nil {
		return string(*backend.Namespace)
	}

	return fallback
}

func validateCrossNamespace(
	ctx context.Context,
	targetNS, sourceNS, serviceName string,
	ref gatewayv1.BackendObjectReference,
	validator BackendRefValidator,
) bool {
	if targetNS == sourceNS || validator == nil {
		return true
	}

	if !validator(ctx, sourceNS, ref) {
		slog.Warn("skipping cross-namespace backend ref not permitted by ReferenceGrant",
			"service", serviceName,
			"from_namespace", sourceNS,
			"to_namespace", targetNS,
		)

		return false
	}

	return true
}

func validatePort(port int32) bool {
	return port >= minPort && port <= maxPort
}

func buildServiceURL(name, namespace string, port int32, clusterDomain string) string {
	clusterDomain = strings.TrimSuffix(clusterDomain, ".")

	scheme := schemeHTTP
	if port == httpsPort {
		scheme = schemeHTTPS
	}

	return fmt.Sprintf("%s://%s.%s.svc.%s:%d", scheme, name, namespace, clusterDomain, port)
}

// convertTimeouts parses Gateway API timeout values into Go durations.
// NOTE: Gateway API Duration (GEP-2257) is a subset of Go's time.Duration
// format (only s, ms, h, m are specified). Using time.ParseDuration is
// intentionally permissive — Kubernetes admission webhooks validate the
// format before it reaches the controller.
// invalidTimeoutsMessage builds the actionable status message for a rule whose
// timeouts could not be parsed. The rule still serves without the timeout.
func invalidTimeoutsMessage(err error) string {
	return fmt.Sprintf(
		"The rule's timeout could not be parsed (%v); the rule is served without it. "+
			"Use a GEP-2257 duration (e.g. \"10s\", \"500ms\") or remove the timeout.",
		err,
	)
}

func convertTimeouts(timeouts *gatewayv1.HTTPRouteTimeouts) (*RouteTimeouts, error) {
	result := &RouteTimeouts{}

	if timeouts.Request != nil {
		duration, err := time.ParseDuration(string(*timeouts.Request))
		if err != nil {
			return nil, fmt.Errorf("invalid request timeout %q: %w", *timeouts.Request, err)
		}

		result.Request = duration
	}

	if timeouts.BackendRequest != nil {
		duration, err := time.ParseDuration(string(*timeouts.BackendRequest))
		if err != nil {
			return nil, fmt.Errorf("invalid backend timeout %q: %w", *timeouts.BackendRequest, err)
		}

		result.Backend = duration
	}

	return result, nil
}
