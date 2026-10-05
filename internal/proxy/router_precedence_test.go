package proxy_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

const precedenceHost = "precedence.example.com"

var precedenceEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// precedenceRoute is one route in a precedence case. Each rule's backend is
// named "<name>-r<ruleIndex>", which is what a case expects to win.
type precedenceRoute[M any] struct {
	namespace string
	name      string
	age       time.Duration // created this long after precedenceEpoch
	rules     [][]M
}

func backendName(name string, ruleIdx int) string {
	return fmt.Sprintf("%s-r%d", name, ruleIdx)
}

func httpPath(pathType gatewayv1.PathMatchType, value string) *gatewayv1.HTTPPathMatch {
	return &gatewayv1.HTTPPathMatch{Type: &pathType, Value: &value}
}

func prefix(value string) gatewayv1.HTTPRouteMatch {
	return gatewayv1.HTTPRouteMatch{Path: httpPath(gatewayv1.PathMatchPathPrefix, value)}
}

func exact(value string) gatewayv1.HTTPRouteMatch {
	return gatewayv1.HTTPRouteMatch{Path: httpPath(gatewayv1.PathMatchExact, value)}
}

func withMethod(match gatewayv1.HTTPRouteMatch, method gatewayv1.HTTPMethod) gatewayv1.HTTPRouteMatch {
	match.Method = &method

	return match
}

func withHeaders(match gatewayv1.HTTPRouteMatch, count int) gatewayv1.HTTPRouteMatch {
	for idx := range count {
		match.Headers = append(match.Headers, gatewayv1.HTTPHeaderMatch{
			Name: gatewayv1.HTTPHeaderName(fmt.Sprintf("X-H%d", idx)), Value: "v",
		})
	}

	return match
}

func withQueryParams(match gatewayv1.HTTPRouteMatch, count int) gatewayv1.HTTPRouteMatch {
	for idx := range count {
		match.QueryParams = append(match.QueryParams, gatewayv1.HTTPQueryParamMatch{
			Name: gatewayv1.HTTPHeaderName(fmt.Sprintf("q%d", idx)), Value: "v",
		})
	}

	return match
}

// equalRules is count rules of four identical matches each, enough equal
// entries in one host bucket that only an explicit tie-break keeps them in
// rule order.
func equalRules(count int) [][]gatewayv1.HTTPRouteMatch {
	rules := make([][]gatewayv1.HTTPRouteMatch, count)
	for idx := range rules {
		rules[idx] = []gatewayv1.HTTPRouteMatch{prefix("/"), prefix("/"), prefix("/"), prefix("/")}
	}

	return rules
}

func buildHTTPRoutes(specs []precedenceRoute[gatewayv1.HTTPRouteMatch]) []*gatewayv1.HTTPRoute {
	routes := make([]*gatewayv1.HTTPRoute, 0, len(specs))

	for _, spec := range specs {
		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:         spec.namespace,
				Name:              spec.name,
				CreationTimestamp: metav1.NewTime(precedenceEpoch.Add(spec.age)),
			},
			Spec: gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{precedenceHost}},
		}

		for ruleIdx, matches := range spec.rules {
			route.Spec.Rules = append(route.Spec.Rules, gatewayv1.HTTPRouteRule{
				Matches:     matches,
				BackendRefs: []gatewayv1.HTTPBackendRef{backendRef(backendName(spec.name, ruleIdx), 80, 1)},
			})
		}

		routes = append(routes, route)
	}

	return routes
}

// precedenceRequest carries every header X-H0..X-H15 and query param
// q0..q15 the match builders above can ask for, so a case's matches decide
// the outcome only through their ranking.
func precedenceRequest(method, path string) *http.Request {
	header := http.Header{"Content-Type": {"application/grpc"}}
	query := url.Values{}

	for idx := range 16 {
		header.Set(fmt.Sprintf("X-H%d", idx), "v")
		query.Set(fmt.Sprintf("q%d", idx), "v")
	}

	return &http.Request{
		Method: method,
		Host:   precedenceHost,
		URL:    &url.URL{Path: path, RawQuery: query.Encode()},
		Header: header,
	}
}

func assertServedBy(t *testing.T, cfg *proxy.Config, req *http.Request, want string) {
	t.Helper()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(cfg))

	result := router.Route(req)
	require.NotNil(t, result)
	require.NotEmpty(t, result.Rule.Backends)
	assert.Contains(t, result.Rule.Backends[0].URL, "://"+want+".")
}

// TestRouter_HTTPRouteMatchPrecedence encodes the HTTPRouteRule.Matches
// precedence list literally: Exact path, then the longest Prefix, then a
// method match, then the most header matches, then the most query param
// matches, compared per match; ties go to the oldest route, then
// {namespace}/{name}, then the first rule. Every case lists the expected loser
// first and older, so the tie-breakers alone would pick the wrong rule.
func TestRouter_HTTPRouteMatchPrecedence(t *testing.T) {
	t.Parallel()

	type match = gatewayv1.HTTPRouteMatch

	tests := []struct {
		name   string
		routes []precedenceRoute[match]
		method string
		path   string
		want   string
	}{
		{
			name: "exact path beats a prefix of the same length with every lower criterion",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{withQueryParams(withHeaders(withMethod(prefix("/foo"), "GET"), 16), 16)}}},
				{namespace: "ns", name: "b", age: time.Hour, rules: [][]match{{exact("/foo")}}},
			},
			path: "/foo", want: "b-r0",
		},
		{
			name: "longer prefix beats method, headers and query params",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{withQueryParams(withHeaders(withMethod(prefix("/"), "GET"), 16), 16)}}},
				{namespace: "ns", name: "b", age: time.Hour, rules: [][]match{{prefix("/api")}}},
			},
			path: "/api/v1", want: "b-r0",
		},
		{
			name: "method beats any number of header matches",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{withQueryParams(withHeaders(prefix("/"), 16), 16)}}},
				{namespace: "ns", name: "b", age: time.Hour, rules: [][]match{{withMethod(prefix("/"), "GET")}}},
			},
			path: "/", want: "b-r0",
		},
		{
			name: "one more header beats any number of query param matches",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{withQueryParams(withHeaders(prefix("/"), 1), 16)}}},
				{namespace: "ns", name: "b", age: time.Hour, rules: [][]match{{withHeaders(prefix("/"), 2)}}},
			},
			path: "/", want: "b-r0",
		},
		{
			name: "more query params win the last criterion",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{withQueryParams(prefix("/"), 1)}}},
				{namespace: "ns", name: "b", age: time.Hour, rules: [][]match{{withQueryParams(prefix("/"), 2)}}},
			},
			path: "/", want: "b-r0",
		},
		{
			name: "an exact arm does not lift its rule's prefix arm",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{exact("/admin"), prefix("/")}}},
				{namespace: "ns", name: "b", age: time.Hour, rules: [][]match{{prefix("/bar")}}},
			},
			path: "/bar", want: "b-r0",
		},
		{
			name: "a method on one arm does not rank the arm that matched",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{withMethod(prefix("/x"), "GET"), prefix("/y")}}},
				{namespace: "ns", name: "b", age: time.Hour, rules: [][]match{{withHeaders(prefix("/y"), 1)}}},
			},
			path: "/y", want: "b-r0",
		},
		{
			name: "tie: the oldest route wins",
			routes: []precedenceRoute[match]{
				{namespace: "a", name: "a", age: time.Hour, rules: [][]match{{prefix("/")}}},
				{namespace: "z", name: "z", rules: [][]match{{prefix("/")}}},
			},
			path: "/", want: "z-r0",
		},
		{
			name: "tie on age: alphabetical {namespace}/{name} wins",
			routes: []precedenceRoute[match]{
				{namespace: "ns-b", name: "a", rules: [][]match{{prefix("/")}}},
				{namespace: "ns-a", name: "z", rules: [][]match{{prefix("/")}}},
			},
			path: "/", want: "z-r0",
		},
		{
			name: "tie within a route: the first rule wins",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{prefix("/")}, {prefix("/")}}},
			},
			path: "/", want: "a-r0",
		},
		{
			name: "tie among many equal matches: the first rule of the oldest route wins",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "c", age: 2 * time.Hour, rules: equalRules(16)},
				{namespace: "ns", name: "b", age: time.Hour, rules: equalRules(16)},
				{namespace: "ns", name: "a", rules: equalRules(16)},
			},
			path: "/", want: "a-r0",
		},
		{
			name: "within a route a more specific later rule wins",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{prefix("/")}, {prefix("/api")}}},
			},
			path: "/api", want: "a-r1",
		},
		{
			name: "a rule without matches ties PathPrefix /: the oldest route wins",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", age: time.Hour, rules: [][]match{{prefix("/")}}},
				{namespace: "ns", name: "z", rules: [][]match{{}}},
			},
			path: "/", want: "z-r0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := proxy.ConvertHTTPRoutes(context.Background(), buildHTTPRoutes(tt.routes), "cluster.local", nil, nil, nil, nil)
			assertServedBy(t, cfg, precedenceRequest(http.MethodGet, tt.path), tt.want)
		})
	}
}

func grpcMethod(matchType *gatewayv1.GRPCMethodMatchType, service, method string) gatewayv1.GRPCRouteMatch {
	match := &gatewayv1.GRPCMethodMatch{Type: matchType}

	if service != "" {
		match.Service = &service
	}

	if method != "" {
		match.Method = &method
	}

	return gatewayv1.GRPCRouteMatch{Method: match}
}

func withGRPCHeaders(match gatewayv1.GRPCRouteMatch, count int) gatewayv1.GRPCRouteMatch {
	for idx := range count {
		match.Headers = append(match.Headers, gatewayv1.GRPCHeaderMatch{
			Name: gatewayv1.GRPCHeaderName(fmt.Sprintf("X-H%d", idx)), Value: "v",
		})
	}

	return match
}

func buildGRPCRoutes(specs []precedenceRoute[gatewayv1.GRPCRouteMatch]) []*gatewayv1.GRPCRoute {
	routes := make([]*gatewayv1.GRPCRoute, 0, len(specs))

	for _, spec := range specs {
		route := &gatewayv1.GRPCRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:         spec.namespace,
				Name:              spec.name,
				CreationTimestamp: metav1.NewTime(precedenceEpoch.Add(spec.age)),
			},
			Spec: gatewayv1.GRPCRouteSpec{Hostnames: []gatewayv1.Hostname{precedenceHost}},
		}

		for ruleIdx, matches := range spec.rules {
			route.Spec.Rules = append(route.Spec.Rules, gatewayv1.GRPCRouteRule{
				Matches:     matches,
				BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef(backendName(spec.name, ruleIdx), 9000, 1)},
			})
		}

		routes = append(routes, route)
	}

	return routes
}

// TestRouter_GRPCRouteMatchPrecedence encodes the GRPCRouteRule.Matches
// precedence list literally: the most characters in a matching service, then
// in a matching method, then the most header matches; ties go to the oldest
// route, then {namespace}/{name}, then the first rule. As above, the expected
// loser is always listed first and older.
func TestRouter_GRPCRouteMatchPrecedence(t *testing.T) {
	t.Parallel()

	type match = gatewayv1.GRPCRouteMatch

	tests := []struct {
		name   string
		routes []precedenceRoute[match]
		want   string
	}{
		{
			name: "a service match beats a method-only match",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{grpcMethod(grpcExact(), "", "Get")}}},
				{namespace: "ns", name: "b", age: time.Hour, rules: [][]match{{grpcMethod(grpcExact(), "foo.Bar", "")}}},
			},
			want: "b-r0",
		},
		{
			name: "a longer service beats a shorter one with a method",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{grpcMethod(grpcRegex(), "f.*", "Get")}}},
				{namespace: "ns", name: "b", age: time.Hour, rules: [][]match{{grpcMethod(grpcExact(), "foo.Bar", "")}}},
			},
			want: "b-r0",
		},
		{
			name: "a service match beats any number of header matches",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{withGRPCHeaders(grpcMethod(grpcExact(), "", "Get"), 16)}}},
				{namespace: "ns", name: "b", age: time.Hour, rules: [][]match{{grpcMethod(grpcExact(), "foo.Bar", "")}}},
			},
			want: "b-r0",
		},
		{
			name: "method characters break a service tie",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{withGRPCHeaders(grpcMethod(grpcExact(), "foo.Bar", ""), 16)}}},
				{namespace: "ns", name: "b", age: time.Hour, rules: [][]match{{grpcMethod(grpcExact(), "foo.Bar", "Get")}}},
			},
			want: "b-r0",
		},
		{
			name: "headers break a service and method tie",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{grpcMethod(grpcExact(), "foo.Bar", "Get")}}},
				{namespace: "ns", name: "b", age: time.Hour, rules: [][]match{{withGRPCHeaders(grpcMethod(grpcRegex(), "foo.Ba.", "G.t"), 1)}}},
			},
			want: "b-r0",
		},
		{
			name: "tie: the oldest route wins",
			routes: []precedenceRoute[match]{
				{namespace: "a", name: "a", age: time.Hour, rules: [][]match{{grpcMethod(grpcExact(), "foo.Bar", "")}}},
				{namespace: "z", name: "z", rules: [][]match{{grpcMethod(grpcExact(), "foo.Bar", "")}}},
			},
			want: "z-r0",
		},
		{
			name: "tie on age: alphabetical {namespace}/{name} wins",
			routes: []precedenceRoute[match]{
				{namespace: "ns-b", name: "a", rules: [][]match{{grpcMethod(grpcExact(), "foo.Bar", "")}}},
				{namespace: "ns-a", name: "z", rules: [][]match{{grpcMethod(grpcExact(), "foo.Bar", "")}}},
			},
			want: "z-r0",
		},
		{
			name: "tie within a route: the first rule wins",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{
					{grpcMethod(grpcExact(), "foo.Bar", "")},
					{grpcMethod(grpcRegex(), "foo.Ba.", "")},
				}},
			},
			want: "a-r0",
		},
		{
			name: "an empty match beside a specific one matches every request",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{{}, grpcMethod(grpcExact(), "x.Y", "Z")}}},
			},
			want: "a-r0",
		},
		{
			name: "a specific match beats an older route's empty match",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", rules: [][]match{{{}, grpcMethod(grpcExact(), "x.Y", "Z")}}},
				{namespace: "ns", name: "b", age: time.Hour, rules: [][]match{{grpcMethod(grpcExact(), "", "Get")}}},
			},
			want: "b-r0",
		},
		{
			name: "an empty match ties a rule without matches: the oldest route wins",
			routes: []precedenceRoute[match]{
				{namespace: "ns", name: "a", age: time.Hour, rules: [][]match{{{}, grpcMethod(grpcExact(), "x.Y", "Z")}}},
				{namespace: "ns", name: "z", rules: [][]match{{}}},
			},
			want: "z-r0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := proxy.ConvertGRPCRoutes(context.Background(), buildGRPCRoutes(tt.routes), "cluster.local", nil, nil, nil, nil)
			assertServedBy(t, cfg, precedenceRequest(http.MethodPost, "/foo.Bar/Get"), tt.want)
		})
	}
}
