package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

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

	for _, route := range routes {
		hostnames := convertHostnames(route.Spec.Hostnames)
		clientCert := resolveFirstParentClientCert(ctx, route, gatewayCertResolver)

		for ruleIdx := range route.Spec.Rules {
			proxyRule := convertHTTPRouteRule(
				ctx, &route.Spec.Rules[ruleIdx], hostnames,
				route.Namespace, clusterDomain, validator, protocolResolver, tlsResolver, clientCert,
			)

			// Rules with no backends and no redirect filter are kept —
			// per Gateway API spec, unresolvable backend refs must return HTTP 500.
			// The proxy handler returns 500 when no backend is available.

			cfg.Rules = append(cfg.Rules, proxyRule)
		}
	}

	return cfg
}

// resolveFirstParentClientCert walks the route's parentRefs in declaration
// order, asking gatewayCertResolver for each parent's client certificate, and
// returns the first non-nil result. Multiple parents with conflicting certs
// are a spec edge case the conformance suite does not exercise; this
// "first-wins" rule is documented in docs/gateway-api/limitations.md.
func resolveFirstParentClientCert(
	ctx context.Context,
	route *gatewayv1.HTTPRoute,
	resolver GatewayClientCertResolver,
) *ClientCertConfig {
	if resolver == nil {
		return nil
	}

	for _, ref := range route.Spec.ParentRefs {
		if ref.Group != nil && *ref.Group != "" && *ref.Group != "gateway.networking.k8s.io" {
			continue
		}

		if ref.Kind != nil && *ref.Kind != "" && *ref.Kind != "Gateway" {
			continue
		}

		ns := route.Namespace
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
) RouteRule {
	proxyRule := RouteRule{
		Hostnames: hostnames,
	}

	for _, match := range rule.Matches {
		proxyRule.Matches = append(proxyRule.Matches, convertMatch(match))
	}

	for filterIdx := range rule.Filters {
		converted := convertFilter(ctx, &rule.Filters[filterIdx], namespace, clusterDomain, validator, tlsResolver)
		if converted != nil {
			proxyRule.Filters = append(proxyRule.Filters, *converted)
		}
	}

	for backendIdx := range rule.BackendRefs {
		backend, ok := convertBackendRef(ctx, &rule.BackendRefs[backendIdx], namespace, clusterDomain, validator, resolver, tlsResolver, clientCert)
		if ok {
			proxyRule.Backends = append(proxyRule.Backends, backend)
		}
	}

	if rule.Timeouts != nil {
		timeouts, err := convertTimeouts(rule.Timeouts)
		if err != nil {
			slog.Warn("skipping invalid route timeouts", "error", err)
		} else {
			proxyRule.Timeouts = timeouts
		}
	}

	return proxyRule
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

func convertFilter(
	ctx context.Context,
	filter *gatewayv1.HTTPRouteFilter,
	namespace, clusterDomain string,
	validator BackendRefValidator,
	tlsResolver BackendTLSResolver,
) *RouteFilter {
	switch filter.Type {
	case gatewayv1.HTTPRouteFilterRequestHeaderModifier:
		return convertRequestHeaderFilter(filter.RequestHeaderModifier)
	case gatewayv1.HTTPRouteFilterResponseHeaderModifier:
		return convertResponseHeaderFilter(filter.ResponseHeaderModifier)
	case gatewayv1.HTTPRouteFilterRequestRedirect:
		return convertRedirectFilter(filter.RequestRedirect)
	case gatewayv1.HTTPRouteFilterURLRewrite:
		return convertURLRewriteFilter(filter.URLRewrite)
	case gatewayv1.HTTPRouteFilterRequestMirror:
		return convertMirrorFilter(ctx, filter.RequestMirror, namespace, clusterDomain, validator, tlsResolver)
	case gatewayv1.HTTPRouteFilterCORS:
		return convertCORSFilter(filter.CORS)
	case gatewayv1.HTTPRouteFilterExtensionRef,
		gatewayv1.HTTPRouteFilterExternalAuth:
		slog.Warn("skipping unsupported filter type", "type", filter.Type)

		return nil
	}

	slog.Warn("skipping unknown filter type", "type", filter.Type)

	return nil
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

// IsServiceBackendRef reports whether the BackendObjectReference points at a
// core Service (the default Kind when Group/Kind are nil). Exported so the
// controller package can reuse the same predicate without duplicating it.
func IsServiceBackendRef(ref gatewayv1.BackendObjectReference) bool {
	if ref.Group != nil && *ref.Group != "" && *ref.Group != "core" {
		return false
	}

	if ref.Kind != nil && *ref.Kind != "Service" {
		return false
	}

	return true
}

func convertMirrorFilter(
	ctx context.Context,
	mirror *gatewayv1.HTTPRequestMirrorFilter,
	namespace, clusterDomain string,
	validator BackendRefValidator,
	tlsResolver BackendTLSResolver,
) *RouteFilter {
	if mirror == nil {
		return nil
	}

	if !IsServiceBackendRef(mirror.BackendRef) {
		slog.Warn("skipping mirror with non-Service backend kind",
			"kind", mirror.BackendRef.Kind,
			"name", mirror.BackendRef.Name)

		return nil
	}

	mirrorPort := int32(defaultServicePort)
	if mirror.BackendRef.Port != nil {
		mirrorPort = *mirror.BackendRef.Port
	}

	if !validatePort(mirrorPort) {
		slog.Warn("skipping mirror with invalid port", "service", string(mirror.BackendRef.Name), "port", mirrorPort)

		return nil
	}

	mirrorNS := namespace
	if mirror.BackendRef.Namespace != nil {
		mirrorNS = string(*mirror.BackendRef.Namespace)
	}

	if !validateCrossNamespace(ctx, mirrorNS, namespace, string(mirror.BackendRef.Name), mirror.BackendRef, validator) {
		return nil
	}

	mirrorURL := buildServiceURL(string(mirror.BackendRef.Name), mirrorNS, mirrorPort, clusterDomain)

	// RequestMirror runs through a side-channel HTTP client that does NOT
	// share the main proxy's TLS-aware transport pool. If a BackendTLSPolicy
	// targets the mirror destination, the mirrored copy would be sent in
	// plaintext, silently bypassing the operator's TLS intent. Document the
	// gap loudly so operators see the silent downgrade; an actual TLS-aware
	// mirror dial is tracked as a follow-up (see limitations.md).
	if tlsResolver != nil {
		if tls := tlsResolver(ctx, mirrorNS, string(mirror.BackendRef.Name), mirrorPort); tls != nil {
			slog.Warn("RequestMirror target has a matching BackendTLSPolicy but the mirror filter dials plaintext — mirrored copy bypasses backend TLS enforcement",
				"namespace", mirrorNS,
				"service", string(mirror.BackendRef.Name),
				"port", mirrorPort,
			)
		}
	}

	return &RouteFilter{
		Type: FilterRequestMirror,
		RequestMirror: &MirrorConfig{
			BackendURL: mirrorURL,
			Percent:    mirrorPercent(mirror),
		},
	}
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
) (BackendRef, bool) {
	if !IsServiceBackendRef(backend.BackendObjectReference) {
		slog.Warn("skipping non-Service backend kind",
			"kind", backend.Kind,
			"name", backend.Name)

		return BackendRef{}, false
	}

	result, ok := initBackendRefBaseline(backend)
	if !ok {
		return BackendRef{}, false
	}

	serviceName := string(backend.Name)

	port := int32(defaultServicePort)
	if backend.Port != nil {
		port = *backend.Port
	}

	if !validatePort(port) {
		slog.Warn("skipping backend with invalid port", "service", serviceName, "port", port)

		return BackendRef{}, false
	}

	svcNamespace := resolveBackendNamespace(backend, namespace)

	if !validateCrossNamespace(ctx, svcNamespace, namespace, serviceName, backend.BackendObjectReference, validator) {
		return BackendRef{}, false
	}

	result.URL = buildServiceURL(serviceName, svcNamespace, port, clusterDomain)

	// Resolve TLS first so the protocol resolver can know whether to silently
	// pass through `appProtocol: https` (policy attached → suppressed) or warn
	// (no policy → operator misconfigured a TLS hint with no actual TLS).
	result.TLS, result.URL = resolveBackendTLS(ctx, tlsResolver, svcNamespace, serviceName, port, result.URL)
	result.TLS = attachGatewayClientCert(result.TLS, clientCert)
	result.Protocol, result.URL, result.WebSocket = resolveBackendProtocol(ctx, resolver, svcNamespace, serviceName, port, result.URL, result.TLS != nil)

	result.Filters = convertBackendFilters(ctx, backend.Filters, namespace, clusterDomain, validator, tlsResolver)

	return result, true
}

// initBackendRefBaseline validates a backend ref's kind and weight, returning
// a partially-initialised BackendRef when both checks pass. Extracted from
// convertBackendRef to keep that function under the funlen budget.
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
// The supplied tlsCfg is treated as immutable — a shallow copy is taken
// before mutation so a resolver implementation that caches and returns the
// same *BackendTLSConfig pointer for multiple backends does not silently
// cross-contaminate routes with each other's client cert.
func attachGatewayClientCert(tlsCfg *BackendTLSConfig, clientCert *ClientCertConfig) *BackendTLSConfig {
	if tlsCfg == nil || clientCert == nil {
		return tlsCfg
	}

	stamped := *tlsCfg
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

	return tls, strings.Replace(rawURL, schemeHTTP+"://", schemeHTTPS+"://", 1)
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
) (BackendProtocol, string, bool) {
	if resolver == nil {
		return BackendProtocolHTTP, rawURL, false
	}

	appProto := resolver(ctx, namespace, serviceName, port)

	switch appProto {
	case appProtocolH2C:
		return resolveH2C(rawURL, tlsAttached, namespace, serviceName, port)
	case appProtocolWS:
		if tlsAttached {
			warnCleartextHintSuppressed(
				"appProtocol kubernetes.io/ws suppressed by BackendTLSPolicy on the same Service — WebSocket will run over TLS (consider appProtocol kubernetes.io/wss instead)",
				namespace, serviceName, port)
		}

		return BackendProtocolHTTP, rawURL, true
	case appProtocolWSS:
		warnUnpolicedTLSHint(tlsAttached,
			"backend declares appProtocol wss but no BackendTLSPolicy targets it — WebSocket upgrade will be attempted in plaintext",
			namespace, serviceName, port, appProto)

		return BackendProtocolHTTP, rawURL, true
	case "https", "HTTPS":
		warnUnpolicedTLSHint(tlsAttached,
			"backend declares appProtocol https but no BackendTLSPolicy targets it — request will be sent in plaintext",
			namespace, serviceName, port, appProto)

		return BackendProtocolHTTP, rawURL, false
	case "", "http", "HTTP":
		// Default / cleartext hints match the default transport — silent.
		return BackendProtocolHTTP, rawURL, false
	default:
		slog.Warn("unsupported backend appProtocol; falling back to HTTP/1.1",
			"namespace", namespace,
			"service", serviceName,
			"port", port,
			"appProtocol", appProto,
		)

		return BackendProtocolHTTP, rawURL, false
	}
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
func resolveH2C(rawURL string, tlsAttached bool, namespace, serviceName string, port int32) (BackendProtocol, string, bool) {
	if tlsAttached {
		warnCleartextHintSuppressed(
			"appProtocol kubernetes.io/h2c suppressed by BackendTLSPolicy on the same Service — HTTP/2 will be negotiated over TLS via ALPN",
			namespace, serviceName, port)

		return BackendProtocolHTTP, rawURL, false
	}

	return BackendProtocolH2C, strings.Replace(rawURL, schemeHTTPS+"://", schemeHTTP+"://", 1), false
}

// warnUnpolicedTLSHint logs a WARN when an operator declared a TLS-bearing
// appProtocol (https, wss) without attaching a BackendTLSPolicy. Without a
// CA the proxy cannot verify and will dial plaintext — the backend will
// reject the request and the operator deserves a clear signal about the
// misconfiguration. Extracted from resolveBackendProtocol because the
// pattern is shared by both `https` and `wss`.
func warnUnpolicedTLSHint(tlsAttached bool, message, namespace, serviceName string, port int32, appProto string) {
	if tlsAttached {
		return
	}

	slog.Warn(message,
		"namespace", namespace,
		"service", serviceName,
		"port", port,
		"appProtocol", appProto,
	)
}

// warnCleartextHintSuppressed logs a WARN when an operator declared a
// cleartext appProtocol (h2c, ws) but a BackendTLSPolicy attached to the
// same Service forced TLS anyway. The proxy still does the right thing —
// TLS wins because it's the higher-priority signal — but surfacing the
// conflict lets the operator notice the contradictory hint instead of
// shipping a misleading appProtocol value forever.
func warnCleartextHintSuppressed(message, namespace, serviceName string, port int32) {
	slog.Warn(message,
		"namespace", namespace,
		"service", serviceName,
		"port", port,
	)
}

// convertBackendFilters converts the per-backend HTTPRouteFilters into proxy
// RouteFilters, skipping any that aren't supported or have nil config.
func convertBackendFilters(
	ctx context.Context,
	filters []gatewayv1.HTTPRouteFilter,
	namespace, clusterDomain string,
	validator BackendRefValidator,
	tlsResolver BackendTLSResolver,
) []RouteFilter {
	if len(filters) == 0 {
		return nil
	}

	result := make([]RouteFilter, 0, len(filters))

	for filterIdx := range filters {
		converted := convertFilter(ctx, &filters[filterIdx], namespace, clusterDomain, validator, tlsResolver)
		if converted != nil {
			result = append(result, *converted)
		}
	}

	return result
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
