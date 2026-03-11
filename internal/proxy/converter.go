package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

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
)

// BackendRefValidator checks whether a cross-namespace backend reference is allowed.
// Called only for cross-namespace refs; same-namespace refs are always permitted.
// Returns true if the reference is authorized (e.g., via ReferenceGrant).
type BackendRefValidator func(ctx context.Context, fromNamespace string, ref gatewayv1.BackendObjectReference) bool

// ConvertHTTPRoutes converts Gateway API HTTPRoute resources into a proxy Config.
// If validator is non-nil, cross-namespace backend refs are checked against it;
// unauthorized refs are skipped with a warning log.
func ConvertHTTPRoutes(
	ctx context.Context,
	routes []*gatewayv1.HTTPRoute,
	clusterDomain string,
	validator BackendRefValidator,
) *Config {
	cfg := &Config{
		Version: configVersionCounter.Add(1),
	}

	for _, route := range routes {
		hostnames := convertHostnames(route.Spec.Hostnames)

		for ruleIdx := range route.Spec.Rules {
			proxyRule := convertHTTPRouteRule(
				ctx, &route.Spec.Rules[ruleIdx], hostnames,
				route.Namespace, clusterDomain, validator,
			)

			// Skip rules with no backends and no redirect filter —
			// proxy rejects these with 400 (validation error).
			if len(proxyRule.Backends) == 0 && !proxyRule.hasRedirectFilter() {
				continue
			}

			cfg.Rules = append(cfg.Rules, proxyRule)
		}
	}

	return cfg
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
) RouteRule {
	proxyRule := RouteRule{
		Hostnames: hostnames,
	}

	for _, match := range rule.Matches {
		proxyRule.Matches = append(proxyRule.Matches, convertMatch(match))
	}

	for filterIdx := range rule.Filters {
		converted := convertFilter(&rule.Filters[filterIdx], namespace, clusterDomain)
		if converted != nil {
			proxyRule.Filters = append(proxyRule.Filters, *converted)
		}
	}

	for backendIdx := range rule.BackendRefs {
		backend, ok := convertBackendRef(ctx, &rule.BackendRefs[backendIdx], namespace, clusterDomain, validator)
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

func convertFilter(filter *gatewayv1.HTTPRouteFilter, namespace, clusterDomain string) *RouteFilter {
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
		return convertMirrorFilter(filter.RequestMirror, namespace, clusterDomain)
	case gatewayv1.HTTPRouteFilterExtensionRef,
		gatewayv1.HTTPRouteFilterCORS,
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

func isServiceBackendRef(ref gatewayv1.BackendObjectReference) bool {
	if ref.Group != nil && *ref.Group != "" && *ref.Group != "core" {
		return false
	}

	if ref.Kind != nil && *ref.Kind != "Service" {
		return false
	}

	return true
}

func convertMirrorFilter(mirror *gatewayv1.HTTPRequestMirrorFilter, namespace, clusterDomain string) *RouteFilter {
	if mirror == nil {
		return nil
	}

	if !isServiceBackendRef(mirror.BackendRef) {
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

	mirrorURL := buildServiceURL(string(mirror.BackendRef.Name), mirrorNS, mirrorPort, clusterDomain)

	return &RouteFilter{
		Type:          FilterRequestMirror,
		RequestMirror: &MirrorConfig{BackendURL: mirrorURL},
	}
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
) (BackendRef, bool) {
	if !isServiceBackendRef(backend.BackendObjectReference) {
		slog.Warn("skipping non-Service backend kind",
			"kind", backend.Kind,
			"name", backend.Name)

		return BackendRef{}, false
	}

	result := BackendRef{
		Weight: 1,
	}

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

	return result, true
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
