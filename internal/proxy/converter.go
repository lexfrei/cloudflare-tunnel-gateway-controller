package proxy

import (
	"fmt"
	"strings"
	"time"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const defaultServicePort = 80

// ConvertHTTPRoutes converts Gateway API HTTPRoute resources into a proxy Config.
func ConvertHTTPRoutes(routes []*gatewayv1.HTTPRoute, clusterDomain string) *Config {
	cfg := &Config{
		Version: time.Now().UnixNano(),
	}

	for _, route := range routes {
		hostnames := convertHostnames(route.Spec.Hostnames)

		for ruleIdx := range route.Spec.Rules {
			proxyRule := convertHTTPRouteRule(&route.Spec.Rules[ruleIdx], hostnames, route.Namespace, clusterDomain)
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
	rule *gatewayv1.HTTPRouteRule,
	hostnames []string,
	namespace string,
	clusterDomain string,
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
		proxyRule.Backends = append(proxyRule.Backends, convertBackendRef(&rule.BackendRefs[backendIdx], namespace, clusterDomain))
	}

	if rule.Timeouts != nil {
		proxyRule.Timeouts = convertTimeouts(rule.Timeouts)
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
		if filter.RequestHeaderModifier == nil {
			return nil
		}

		return &RouteFilter{
			Type:                  FilterRequestHeaderModifier,
			RequestHeaderModifier: convertHeaderModifier(filter.RequestHeaderModifier),
		}

	case gatewayv1.HTTPRouteFilterResponseHeaderModifier:
		if filter.ResponseHeaderModifier == nil {
			return nil
		}

		return &RouteFilter{
			Type:                   FilterResponseHeaderModifier,
			ResponseHeaderModifier: convertHeaderModifier(filter.ResponseHeaderModifier),
		}

	case gatewayv1.HTTPRouteFilterRequestRedirect:
		if filter.RequestRedirect == nil {
			return nil
		}

		return &RouteFilter{
			Type:            FilterRequestRedirect,
			RequestRedirect: convertRedirectConfig(filter.RequestRedirect),
		}

	case gatewayv1.HTTPRouteFilterURLRewrite:
		if filter.URLRewrite == nil {
			return nil
		}

		return &RouteFilter{
			Type:       FilterURLRewrite,
			URLRewrite: convertURLRewrite(filter.URLRewrite),
		}

	case gatewayv1.HTTPRouteFilterRequestMirror:
		return convertMirrorFilter(filter.RequestMirror, namespace, clusterDomain)

	case gatewayv1.HTTPRouteFilterExtensionRef,
		gatewayv1.HTTPRouteFilterCORS,
		gatewayv1.HTTPRouteFilterExternalAuth:
		return nil
	}

	return nil
}

func convertMirrorFilter(mirror *gatewayv1.HTTPRequestMirrorFilter, namespace, clusterDomain string) *RouteFilter {
	if mirror == nil {
		return nil
	}

	mirrorPort := int32(defaultServicePort)
	if mirror.BackendRef.Port != nil {
		mirrorPort = *mirror.BackendRef.Port
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

func convertRedirectPath(pathModifier *gatewayv1.HTTPPathModifier) *string {
	switch pathModifier.Type {
	case gatewayv1.FullPathHTTPPathModifier:
		return pathModifier.ReplaceFullPath
	case gatewayv1.PrefixMatchHTTPPathModifier:
		return pathModifier.ReplacePrefixMatch
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
	backend *gatewayv1.HTTPBackendRef,
	namespace string,
	clusterDomain string,
) BackendRef {
	result := BackendRef{
		Weight: 1,
	}

	if backend.Weight != nil {
		result.Weight = *backend.Weight
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

	result.URL = buildServiceURL(serviceName, svcNamespace, port, clusterDomain)

	return result
}

func buildServiceURL(name, namespace string, port int32, clusterDomain string) string {
	clusterDomain = strings.TrimSuffix(clusterDomain, ".")

	return fmt.Sprintf("http://%s.%s.svc.%s:%d", name, namespace, clusterDomain, port)
}

func convertTimeouts(timeouts *gatewayv1.HTTPRouteTimeouts) *RouteTimeouts {
	result := &RouteTimeouts{}

	if timeouts.Request != nil {
		duration, err := time.ParseDuration(string(*timeouts.Request))
		if err == nil {
			result.Request = duration
		}
	}

	if timeouts.BackendRequest != nil {
		duration, err := time.ParseDuration(string(*timeouts.BackendRequest))
		if err == nil {
			result.Backend = duration
		}
	}

	return result
}
