package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// prefixRoute builds a one-rule HTTPRoute on app.example.com with a single
// PathPrefix match and the given filters.
func prefixRoute(prefix, backendSvc string, filters ...gatewayv1.HTTPRouteFilter) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "prefix", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.example.com"},
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{
					{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: &prefix}},
				},
				Filters: filters,
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(backendSvc), Port: &port,
					},
				}}},
			}},
		},
	}
}

func routeOf(t *testing.T, cfg *proxy.Config, path string) *proxy.RouteResult {
	t.Helper()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(cfg))

	return router.Route(&http.Request{
		Method: http.MethodGet,
		Host:   "app.example.com",
		URL:    &url.URL{Path: path},
		Header: http.Header{},
	})
}

// TestHTTPRoutePathPrefix_TrailingSlashIgnored runs the example on
// PathMatchPathPrefix: "the paths /abc, /abc/, and /abc/def would all match
// the prefix /abc, but the path /abcd would not", with "a trailing / is
// ignored" applied to the configured prefix.
func TestHTTPRoutePathPrefix_TrailingSlashIgnored(t *testing.T) {
	t.Parallel()

	paths := []struct {
		path string
		want bool
	}{
		{"/abc", true},
		{"/abc/", true},
		{"/abc/def", true},
		{"/abcd", false},
	}

	for _, prefix := range []string{"/abc", "/abc/"} {
		cfg := proxy.ConvertHTTPRoutes(context.Background(),
			[]*gatewayv1.HTTPRoute{prefixRoute(prefix, "svc")}, "cluster.local", nil, nil, nil, nil)

		for _, tt := range paths {
			t.Run(prefix+" "+tt.path, func(t *testing.T) {
				t.Parallel()

				assert.Equal(t, tt.want, routeOf(t, cfg, tt.path) != nil)
			})
		}
	}
}

// TestGRPCRouteServiceOnly_DoesNotMatchBareServicePath keeps the gRPC
// service-only match to method paths: it shares the PathPrefix matcher with
// HTTPRoute, and the HTTPRoute trailing-slash rule must not reach it.
func TestGRPCRouteServiceOnly_DoesNotMatchBareServicePath(t *testing.T) {
	t.Parallel()

	svc := "svc.A"
	cfg := proxy.ConvertGRPCRoutes(context.Background(), []*gatewayv1.GRPCRoute{{
		ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{
			Hostnames: []gatewayv1.Hostname{"app.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{{
				Matches:     []gatewayv1.GRPCRouteMatch{{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc}}},
				BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("grpc-svc", 9000, 1)},
			}},
		},
	}}, "cluster.local", nil, nil, nil, nil)

	assert.NotNil(t, routeOf(t, cfg, "/svc.A/Method"))
	assert.Nil(t, routeOf(t, cfg, "/svc.A"))
	assert.Nil(t, routeOf(t, cfg, "/svc.AB/Method"))
}

// withSlashedPrefix repeats each spec-table row with a trailing "/" on the
// configured prefix. HTTPPathModifier.ReplacePrefixMatch says a trailing "/"
// is ignored, so every row must produce the same path either way.
func withSlashedPrefix(rows []prefixReplaceCase) []prefixReplaceCase {
	out := append([]prefixReplaceCase(nil), rows...)

	for _, row := range rows {
		if row.prefix[len(row.prefix)-1] == '/' {
			continue
		}

		row.name += " (prefix configured with trailing slash)"
		row.prefix += "/"
		out = append(out, row)
	}

	return out
}

// TestHandler_ReplacePrefixMatchRewrite_SpecTable runs every row of the
// ReplacePrefixMatch table from an HTTPRoute through the router and handler
// to the backend.
func TestHandler_ReplacePrefixMatchRewrite_SpecTable(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "rewrite")

	for _, tt := range withSlashedPrefix(specTableRows()) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			replacement := tt.replacement
			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{
				prefixRoute(tt.prefix, "svc", gatewayv1.HTTPRouteFilter{
					Type: gatewayv1.HTTPRouteFilterURLRewrite,
					URLRewrite: &gatewayv1.HTTPURLRewriteFilter{Path: &gatewayv1.HTTPPathModifier{
						Type:               gatewayv1.PrefixMatchHTTPPathModifier,
						ReplacePrefixMatch: &replacement,
					}},
				}),
			}, "cluster.local", nil, nil, nil, nil)
			require.Len(t, cfg.Rules, 1)
			cfg.Rules[0].Backends[0].URL = backend.URL

			router := proxy.NewRouter()
			require.NoError(t, router.UpdateConfig(cfg))

			recorder := httptest.NewRecorder()
			proxy.NewHandler(router).ServeHTTP(recorder, httptest.NewRequestWithContext(
				t.Context(), http.MethodGet, "http://app.example.com"+tt.requestPath, nil))

			assert.Equal(t, http.StatusOK, recorder.Code)
			assert.Equal(t, tt.want, recorder.Header().Get("X-Received-Path"))
		})
	}
}

// TestHandler_ReplacePrefixMatchRedirect_SpecTable is the redirect twin of
// TestHandler_ReplacePrefixMatchRewrite_SpecTable.
func TestHandler_ReplacePrefixMatchRedirect_SpecTable(t *testing.T) {
	t.Parallel()

	for _, tt := range withSlashedPrefix(specTableRows()) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			replacement := tt.replacement
			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{
				prefixRoute(tt.prefix, "svc", gatewayv1.HTTPRouteFilter{
					Type: gatewayv1.HTTPRouteFilterRequestRedirect,
					RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{Path: &gatewayv1.HTTPPathModifier{
						Type:               gatewayv1.PrefixMatchHTTPPathModifier,
						ReplacePrefixMatch: &replacement,
					}},
				}),
			}, "cluster.local", nil, nil, nil, nil)

			router := proxy.NewRouter()
			require.NoError(t, router.UpdateConfig(cfg))

			recorder := httptest.NewRecorder()
			proxy.NewHandler(router).ServeHTTP(recorder, httptest.NewRequestWithContext(
				t.Context(), http.MethodGet, "http://app.example.com"+tt.requestPath, nil))

			location, err := url.Parse(recorder.Header().Get("Location"))
			require.NoError(t, err)
			assert.Equal(t, tt.want, location.Path)
		})
	}
}

// prefixReplaceRule is a rule on app.example.com with the given matches and a
// single ReplacePrefixMatch filter of "/new", as a redirect or a rewrite.
func prefixReplaceRule(matches []proxy.RouteMatch, redirect bool, backendURL string) proxy.RouteRule {
	rule := proxy.RouteRule{
		Hostnames: []string{"app.example.com"},
		Matches:   matches,
		Backends:  []proxy.BackendRef{{URL: backendURL, Weight: 1}},
	}

	if redirect {
		rule.Filters = []proxy.RouteFilter{{
			Type: proxy.FilterRequestRedirect,
			RequestRedirect: &proxy.RedirectConfig{
				Path: &proxy.RedirectPath{Type: proxy.RedirectPathPrefixReplace, Value: "/new"},
			},
		}}
	} else {
		rule.Filters = []proxy.RouteFilter{{
			Type: proxy.FilterURLRewrite,
			URLRewrite: &proxy.URLRewriteConfig{
				Path: &proxy.URLRewritePath{Type: proxy.URLRewritePrefixMatch, ReplacePrefixMatch: new("/new")},
			},
		}}
	}

	return rule
}

// servedPath returns the path a request for /foo/bar ends up at: the redirect
// Location path, or the path the backend received.
func servedPath(t *testing.T, rule proxy.RouteRule) string {
	t.Helper()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{Version: 1, Rules: []proxy.RouteRule{rule}}))

	recorder := httptest.NewRecorder()
	proxy.NewHandler(router).ServeHTTP(recorder, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://app.example.com/foo/bar", nil))

	if location := recorder.Header().Get("Location"); location != "" {
		parsed, err := url.Parse(location)
		require.NoError(t, err)

		return parsed.Path
	}

	require.Equal(t, http.StatusOK, recorder.Code)

	return recorder.Header().Get("X-Received-Path")
}

// TestHandler_ReplacePrefixMatch_RedirectAndRewriteAgree runs the redirect
// and the rewrite over the same matches. A rule without matches, and a match
// without a path, carry the spec's default match, a PathPrefix of "/"
// (HTTPRouteRule.Matches and HTTPRouteMatch.Path defaults), so the prefix
// "/" is replaced. A match that is not a PathPrefix has no prefix to replace
// ("ReplacePrefixMatch is only compatible with a PathPrefix HTTPRouteMatch"),
// so both filters leave the path as it is.
func TestHandler_ReplacePrefixMatch_RedirectAndRewriteAgree(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "prefix")

	tests := []struct {
		name    string
		matches []proxy.RouteMatch
		want    string
	}{
		{name: "no matches", matches: nil, want: "/new/foo/bar"},
		{
			name:    "match without a path",
			matches: []proxy.RouteMatch{{Method: http.MethodGet}},
			want:    "/new/foo/bar",
		},
		{
			name:    "exact match",
			matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/foo/bar"}}},
			want:    "/foo/bar",
		},
		{
			name:    "regular expression match",
			matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchRegularExpression, Value: "^/foo/.*"}}},
			want:    "/foo/bar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, servedPath(t, prefixReplaceRule(tt.matches, true, backend.URL)), "redirect")
			assert.Equal(t, tt.want, servedPath(t, prefixReplaceRule(tt.matches, false, backend.URL)), "rewrite")
		})
	}
}
