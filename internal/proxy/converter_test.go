package proxy_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func TestConvertHTTPRoutes_Basic(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	pathExact := gatewayv1.PathMatchExact

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &pathPrefix,
									Value: new("/"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("web-svc", 80, 1),
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &pathExact,
									Value: new("/api/health"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("api-svc", 8080, 1),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(routes, "cluster.local")

	require.Len(t, cfg.Rules, 2)
	assert.Contains(t, cfg.Rules[0].Hostnames, "example.com")
	assert.Contains(t, cfg.Rules[1].Hostnames, "example.com")
}

func TestConvertHTTPRoutes_MultipleHostnames(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com", "app.example.org"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("app-svc", 80, 1),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(routes, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	assert.Equal(t, []string{"app.example.com", "app.example.org"}, cfg.Rules[0].Hostnames)
}

func TestConvertHTTPRoutes_Filters(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	scheme := "https"
	statusCode := 301

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "redirect", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterRequestRedirect,
								RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
									Scheme:     &scheme,
									StatusCode: &statusCode,
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("svc", 80, 1),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(routes, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)
	assert.Equal(t, proxy.FilterRequestRedirect, cfg.Rules[0].Filters[0].Type)
	require.NotNil(t, cfg.Rules[0].Filters[0].RequestRedirect)
	assert.Equal(t, "https", *cfg.Rules[0].Filters[0].RequestRedirect.Scheme)
}

func TestConvertHTTPRoutes_HeaderMatch(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	headerExact := gatewayv1.HeaderMatchExact
	methodGet := gatewayv1.HTTPMethodGet

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "header-match", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path:   &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/api")},
								Method: &methodGet,
								Headers: []gatewayv1.HTTPHeaderMatch{
									{
										Type:  &headerExact,
										Name:  "X-Env",
										Value: "prod",
									},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("api-svc", 80, 1),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(routes, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Matches, 1)

	match := cfg.Rules[0].Matches[0]
	assert.Equal(t, "GET", match.Method)
	require.Len(t, match.Headers, 1)
	assert.Equal(t, "X-Env", match.Headers[0].Name)
	assert.Equal(t, "prod", match.Headers[0].Value)
}

func TestConvertHTTPRoutes_WeightedBackends(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "canary", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("stable", 80, 80),
							backendRef("canary", 80, 20),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(routes, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 2)
	assert.Equal(t, int32(80), cfg.Rules[0].Backends[0].Weight)
	assert.Equal(t, int32(20), cfg.Rules[0].Backends[1].Weight)
}

func TestConvertHTTPRoutes_NoHostnames(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "catch-all", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							backendRef("default-svc", 80, 1),
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(routes, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Hostnames, "no hostnames means catch-all")
}

func TestConvertHTTPRoutes_Empty(t *testing.T) {
	t.Parallel()

	cfg := proxy.ConvertHTTPRoutes(nil, "cluster.local")

	assert.Empty(t, cfg.Rules)
	assert.True(t, cfg.Version > 0, "version should be positive")
}

func TestConvertQueryMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		route    *gatewayv1.HTTPRoute
		expected proxy.QueryParamMatch
	}{
		{
			name: "exact match with explicit type",
			route: routeWithQueryMatch(gatewayv1.HTTPQueryParamMatch{
				Type:  new(gatewayv1.QueryParamMatchExact),
				Name:  "page",
				Value: "home",
			}),
			expected: proxy.QueryParamMatch{
				Type:  proxy.QueryParamMatchExact,
				Name:  "page",
				Value: "home",
			},
		},
		{
			name: "regex match",
			route: routeWithQueryMatch(gatewayv1.HTTPQueryParamMatch{
				Type:  new(gatewayv1.QueryParamMatchRegularExpression),
				Name:  "id",
				Value: "[0-9]+",
			}),
			expected: proxy.QueryParamMatch{
				Type:  proxy.QueryParamMatchRegularExpression,
				Name:  "id",
				Value: "[0-9]+",
			},
		},
		{
			name: "nil type defaults to exact",
			route: routeWithQueryMatch(gatewayv1.HTTPQueryParamMatch{
				Name:  "key",
				Value: "val",
			}),
			expected: proxy.QueryParamMatch{
				Type:  proxy.QueryParamMatchExact,
				Name:  "key",
				Value: "val",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{tt.route}, "cluster.local")

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Matches, 1)
			require.Len(t, cfg.Rules[0].Matches[0].QueryParams, 1)
			assert.Equal(t, tt.expected, cfg.Rules[0].Matches[0].QueryParams[0])
		})
	}
}

func TestConvertFilter_RequestHeaderModifier(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
			Set: []gatewayv1.HTTPHeader{
				{Name: "X-Custom", Value: "set-value"},
			},
			Add: []gatewayv1.HTTPHeader{
				{Name: "X-Added", Value: "add-value"},
			},
			Remove: []string{"X-Remove-Me"},
		},
	})

	cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	f := cfg.Rules[0].Filters[0]
	assert.Equal(t, proxy.FilterRequestHeaderModifier, f.Type)
	require.NotNil(t, f.RequestHeaderModifier)
	assert.Equal(t, []proxy.HeaderValue{{Name: "X-Custom", Value: "set-value"}}, f.RequestHeaderModifier.Set)
	assert.Equal(t, []proxy.HeaderValue{{Name: "X-Added", Value: "add-value"}}, f.RequestHeaderModifier.Add)
	assert.Equal(t, []string{"X-Remove-Me"}, f.RequestHeaderModifier.Remove)
}

func TestConvertFilter_RequestHeaderModifier_NilBody(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:                  gatewayv1.HTTPRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: nil,
	})

	cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters)
}

func TestConvertFilter_ResponseHeaderModifier(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
		ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
			Set: []gatewayv1.HTTPHeader{
				{Name: "Cache-Control", Value: "no-store"},
			},
		},
	})

	cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	f := cfg.Rules[0].Filters[0]
	assert.Equal(t, proxy.FilterResponseHeaderModifier, f.Type)
	require.NotNil(t, f.ResponseHeaderModifier)
	assert.Equal(t, []proxy.HeaderValue{{Name: "Cache-Control", Value: "no-store"}}, f.ResponseHeaderModifier.Set)
}

func TestConvertFilter_ResponseHeaderModifier_NilBody(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:                   gatewayv1.HTTPRouteFilterResponseHeaderModifier,
		ResponseHeaderModifier: nil,
	})

	cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters)
}

func TestConvertFilter_RequestRedirect_Full(t *testing.T) {
	t.Parallel()

	hostname := gatewayv1.PreciseHostname("new.example.com")
	port := gatewayv1.PortNumber(443)

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterRequestRedirect,
		RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
			Scheme:   new("https"),
			Hostname: &hostname,
			Port:     &port,
			Path: &gatewayv1.HTTPPathModifier{
				Type:            gatewayv1.FullPathHTTPPathModifier,
				ReplaceFullPath: new("/new-path"),
			},
			StatusCode: new(301),
		},
	})

	cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	f := cfg.Rules[0].Filters[0]
	assert.Equal(t, proxy.FilterRequestRedirect, f.Type)
	require.NotNil(t, f.RequestRedirect)
	assert.Equal(t, "https", *f.RequestRedirect.Scheme)
	assert.Equal(t, "new.example.com", *f.RequestRedirect.Hostname)
	assert.Equal(t, int32(443), *f.RequestRedirect.Port)
	assert.Equal(t, "/new-path", *f.RequestRedirect.Path)
	assert.Equal(t, 301, *f.RequestRedirect.StatusCode)
}

func TestConvertFilter_RequestRedirect_NilBody(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:            gatewayv1.HTTPRouteFilterRequestRedirect,
		RequestRedirect: nil,
	})

	cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters)
}

func TestConvertFilter_URLRewrite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		rewrite          *gatewayv1.HTTPURLRewriteFilter
		expectedHostname *string
		expectedPath     *proxy.URLRewritePath
	}{
		{
			name: "hostname only",
			rewrite: &gatewayv1.HTTPURLRewriteFilter{
				Hostname: (*gatewayv1.PreciseHostname)(new("rewritten.example.com")),
			},
			expectedHostname: new("rewritten.example.com"),
			expectedPath:     nil,
		},
		{
			name: "full path rewrite",
			rewrite: &gatewayv1.HTTPURLRewriteFilter{
				Path: &gatewayv1.HTTPPathModifier{
					Type:            gatewayv1.FullPathHTTPPathModifier,
					ReplaceFullPath: new("/replaced"),
				},
			},
			expectedHostname: nil,
			expectedPath: &proxy.URLRewritePath{
				Type:            proxy.URLRewriteFullPath,
				ReplaceFullPath: new("/replaced"),
			},
		},
		{
			name: "prefix match rewrite",
			rewrite: &gatewayv1.HTTPURLRewriteFilter{
				Path: &gatewayv1.HTTPPathModifier{
					Type:               gatewayv1.PrefixMatchHTTPPathModifier,
					ReplacePrefixMatch: new("/new-prefix"),
				},
			},
			expectedHostname: nil,
			expectedPath: &proxy.URLRewritePath{
				Type:               proxy.URLRewritePrefixMatch,
				ReplacePrefixMatch: new("/new-prefix"),
			},
		},
		{
			name: "hostname and path",
			rewrite: &gatewayv1.HTTPURLRewriteFilter{
				Hostname: (*gatewayv1.PreciseHostname)(new("host.example.com")),
				Path: &gatewayv1.HTTPPathModifier{
					Type:            gatewayv1.FullPathHTTPPathModifier,
					ReplaceFullPath: new("/full"),
				},
			},
			expectedHostname: new("host.example.com"),
			expectedPath: &proxy.URLRewritePath{
				Type:            proxy.URLRewriteFullPath,
				ReplaceFullPath: new("/full"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			route := routeWithFilter(gatewayv1.HTTPRouteFilter{
				Type:       gatewayv1.HTTPRouteFilterURLRewrite,
				URLRewrite: tt.rewrite,
			})

			cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Filters, 1)

			f := cfg.Rules[0].Filters[0]
			assert.Equal(t, proxy.FilterURLRewrite, f.Type)
			require.NotNil(t, f.URLRewrite)

			if tt.expectedHostname != nil {
				require.NotNil(t, f.URLRewrite.Hostname)
				assert.Equal(t, *tt.expectedHostname, *f.URLRewrite.Hostname)
			} else {
				assert.Nil(t, f.URLRewrite.Hostname)
			}

			if tt.expectedPath != nil {
				require.NotNil(t, f.URLRewrite.Path)
				assert.Equal(t, tt.expectedPath.Type, f.URLRewrite.Path.Type)

				if tt.expectedPath.ReplaceFullPath != nil {
					require.NotNil(t, f.URLRewrite.Path.ReplaceFullPath)
					assert.Equal(t, *tt.expectedPath.ReplaceFullPath, *f.URLRewrite.Path.ReplaceFullPath)
				}

				if tt.expectedPath.ReplacePrefixMatch != nil {
					require.NotNil(t, f.URLRewrite.Path.ReplacePrefixMatch)
					assert.Equal(t, *tt.expectedPath.ReplacePrefixMatch, *f.URLRewrite.Path.ReplacePrefixMatch)
				}
			} else {
				assert.Nil(t, f.URLRewrite.Path)
			}
		})
	}
}

func TestConvertFilter_URLRewrite_NilBody(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:       gatewayv1.HTTPRouteFilterURLRewrite,
		URLRewrite: nil,
	})

	cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters)
}

func TestConvertFilter_RequestMirror(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterRequestMirror,
		RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
			BackendRef: gatewayv1.BackendObjectReference{
				Name: "mirror-svc",
			},
		},
	})

	cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	f := cfg.Rules[0].Filters[0]
	assert.Equal(t, proxy.FilterRequestMirror, f.Type)
	require.NotNil(t, f.RequestMirror)
	assert.Equal(t, "http://mirror-svc.default.svc.cluster.local:80", f.RequestMirror.BackendURL)
}

func TestConvertFilter_RequestMirror_NilBody(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:          gatewayv1.HTTPRouteFilterRequestMirror,
		RequestMirror: nil,
	})

	cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	assert.Empty(t, cfg.Rules[0].Filters)
}

func TestConvertFilter_UnsupportedTypes(t *testing.T) {
	t.Parallel()

	unsupportedTypes := []gatewayv1.HTTPRouteFilterType{
		gatewayv1.HTTPRouteFilterExtensionRef,
	}

	for _, filterType := range unsupportedTypes {
		t.Run(string(filterType), func(t *testing.T) {
			t.Parallel()

			route := routeWithFilter(gatewayv1.HTTPRouteFilter{
				Type: filterType,
			})

			cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

			require.Len(t, cfg.Rules, 1)
			assert.Empty(t, cfg.Rules[0].Filters)
		})
	}
}

func TestConvertHeaderModifier_AllOperations(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
			Set: []gatewayv1.HTTPHeader{
				{Name: "X-Set-One", Value: "one"},
				{Name: "X-Set-Two", Value: "two"},
			},
			Add: []gatewayv1.HTTPHeader{
				{Name: "X-Add-One", Value: "added"},
			},
			Remove: []string{"X-Del-One", "X-Del-Two"},
		},
	})

	cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	mod := cfg.Rules[0].Filters[0].RequestHeaderModifier
	require.NotNil(t, mod)
	assert.Len(t, mod.Set, 2)
	assert.Len(t, mod.Add, 1)
	assert.Len(t, mod.Remove, 2)
	assert.Equal(t, "X-Set-One", mod.Set[0].Name)
	assert.Equal(t, "X-Add-One", mod.Add[0].Name)
	assert.Equal(t, "X-Del-One", mod.Remove[0])
}

func TestConvertHeaderModifier_Empty(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:                  gatewayv1.HTTPRouteFilterRequestHeaderModifier,
		RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{},
	})

	cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	mod := cfg.Rules[0].Filters[0].RequestHeaderModifier
	require.NotNil(t, mod)
	assert.Empty(t, mod.Set)
	assert.Empty(t, mod.Add)
	assert.Empty(t, mod.Remove)
}

func TestConvertRedirectPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		pathModifier *gatewayv1.HTTPPathModifier
		expectedPath string
	}{
		{
			name: "full path replacement",
			pathModifier: &gatewayv1.HTTPPathModifier{
				Type:            gatewayv1.FullPathHTTPPathModifier,
				ReplaceFullPath: new("/new-full-path"),
			},
			expectedPath: "/new-full-path",
		},
		{
			name: "prefix match replacement",
			pathModifier: &gatewayv1.HTTPPathModifier{
				Type:               gatewayv1.PrefixMatchHTTPPathModifier,
				ReplacePrefixMatch: new("/new-prefix"),
			},
			expectedPath: "/new-prefix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			route := routeWithFilter(gatewayv1.HTTPRouteFilter{
				Type: gatewayv1.HTTPRouteFilterRequestRedirect,
				RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
					Path: tt.pathModifier,
				},
			})

			cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Filters, 1)
			require.NotNil(t, cfg.Rules[0].Filters[0].RequestRedirect)
			require.NotNil(t, cfg.Rules[0].Filters[0].RequestRedirect.Path)
			assert.Equal(t, tt.expectedPath, *cfg.Rules[0].Filters[0].RequestRedirect.Path)
		})
	}
}

func TestConvertRedirectPath_NilFields(t *testing.T) {
	t.Parallel()

	route := routeWithFilter(gatewayv1.HTTPRouteFilter{
		Type:            gatewayv1.HTTPRouteFilterRequestRedirect,
		RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
			// No fields set at all.
		},
	})

	cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1)

	redirect := cfg.Rules[0].Filters[0].RequestRedirect
	require.NotNil(t, redirect)
	assert.Nil(t, redirect.Scheme)
	assert.Nil(t, redirect.Hostname)
	assert.Nil(t, redirect.Port)
	assert.Nil(t, redirect.Path)
	assert.Nil(t, redirect.StatusCode)
}

func TestConvertURLRewritePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		pathModifier *gatewayv1.HTTPPathModifier
		expectedType proxy.URLRewritePathType
		expectedVal  string
	}{
		{
			name: "full path rewrite",
			pathModifier: &gatewayv1.HTTPPathModifier{
				Type:            gatewayv1.FullPathHTTPPathModifier,
				ReplaceFullPath: new("/full-rewrite"),
			},
			expectedType: proxy.URLRewriteFullPath,
			expectedVal:  "/full-rewrite",
		},
		{
			name: "prefix match rewrite",
			pathModifier: &gatewayv1.HTTPPathModifier{
				Type:               gatewayv1.PrefixMatchHTTPPathModifier,
				ReplacePrefixMatch: new("/prefix-rewrite"),
			},
			expectedType: proxy.URLRewritePrefixMatch,
			expectedVal:  "/prefix-rewrite",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			route := routeWithFilter(gatewayv1.HTTPRouteFilter{
				Type: gatewayv1.HTTPRouteFilterURLRewrite,
				URLRewrite: &gatewayv1.HTTPURLRewriteFilter{
					Path: tt.pathModifier,
				},
			})

			cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Filters, 1)

			rewritePath := cfg.Rules[0].Filters[0].URLRewrite.Path
			require.NotNil(t, rewritePath)
			assert.Equal(t, tt.expectedType, rewritePath.Type)

			if tt.expectedType == proxy.URLRewriteFullPath {
				require.NotNil(t, rewritePath.ReplaceFullPath)
				assert.Equal(t, tt.expectedVal, *rewritePath.ReplaceFullPath)
			} else {
				require.NotNil(t, rewritePath.ReplacePrefixMatch)
				assert.Equal(t, tt.expectedVal, *rewritePath.ReplacePrefixMatch)
			}
		})
	}
}

func TestConvertTimeouts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		timeouts        *gatewayv1.HTTPRouteTimeouts
		expectedRequest time.Duration
		expectedBackend time.Duration
	}{
		{
			name: "both timeouts set",
			timeouts: &gatewayv1.HTTPRouteTimeouts{
				Request:        durationPtr("10s"),
				BackendRequest: durationPtr("5s"),
			},
			expectedRequest: 10 * time.Second,
			expectedBackend: 5 * time.Second,
		},
		{
			name: "request timeout only",
			timeouts: &gatewayv1.HTTPRouteTimeouts{
				Request: durationPtr("30s"),
			},
			expectedRequest: 30 * time.Second,
			expectedBackend: 0,
		},
		{
			name: "backend timeout only",
			timeouts: &gatewayv1.HTTPRouteTimeouts{
				BackendRequest: durationPtr("15s"),
			},
			expectedRequest: 0,
			expectedBackend: 15 * time.Second,
		},
		{
			name: "invalid request timeout is ignored",
			timeouts: &gatewayv1.HTTPRouteTimeouts{
				Request:        durationPtr("not-a-duration"),
				BackendRequest: durationPtr("5s"),
			},
			expectedRequest: 0,
			expectedBackend: 5 * time.Second,
		},
		{
			name: "invalid backend timeout is ignored",
			timeouts: &gatewayv1.HTTPRouteTimeouts{
				Request:        durationPtr("10s"),
				BackendRequest: durationPtr("garbage"),
			},
			expectedRequest: 10 * time.Second,
			expectedBackend: 0,
		},
		{
			name: "millisecond precision",
			timeouts: &gatewayv1.HTTPRouteTimeouts{
				Request: durationPtr("500ms"),
			},
			expectedRequest: 500 * time.Millisecond,
			expectedBackend: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			route := routeWithTimeouts(tt.timeouts)

			cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

			require.Len(t, cfg.Rules, 1)
			require.NotNil(t, cfg.Rules[0].Timeouts)
			assert.Equal(t, tt.expectedRequest, cfg.Rules[0].Timeouts.Request)
			assert.Equal(t, tt.expectedBackend, cfg.Rules[0].Timeouts.Backend)
		})
	}
}

func TestConvertTimeouts_Nil(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "no-timeouts", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("svc", 80, 1),
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

	require.Len(t, cfg.Rules, 1)
	assert.Nil(t, cfg.Rules[0].Timeouts)
}

func TestConvertHeaderMatch_EdgeCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		header   gatewayv1.HTTPHeaderMatch
		expected proxy.HeaderMatch
	}{
		{
			name: "nil type defaults to exact",
			header: gatewayv1.HTTPHeaderMatch{
				Name:  "X-Test",
				Value: "value",
			},
			expected: proxy.HeaderMatch{
				Type:  proxy.HeaderMatchExact,
				Name:  "X-Test",
				Value: "value",
			},
		},
		{
			name: "regex type",
			header: gatewayv1.HTTPHeaderMatch{
				Type:  new(gatewayv1.HeaderMatchRegularExpression),
				Name:  "X-Pattern",
				Value: "v[0-9]+",
			},
			expected: proxy.HeaderMatch{
				Type:  proxy.HeaderMatchRegularExpression,
				Name:  "X-Pattern",
				Value: "v[0-9]+",
			},
		},
		{
			name: "explicit exact type",
			header: gatewayv1.HTTPHeaderMatch{
				Type:  new(gatewayv1.HeaderMatchExact),
				Name:  "Content-Type",
				Value: "application/json",
			},
			expected: proxy.HeaderMatch{
				Type:  proxy.HeaderMatchExact,
				Name:  "Content-Type",
				Value: "application/json",
			},
		},
		{
			name: "empty value",
			header: gatewayv1.HTTPHeaderMatch{
				Name:  "X-Empty",
				Value: "",
			},
			expected: proxy.HeaderMatch{
				Type:  proxy.HeaderMatchExact,
				Name:  "X-Empty",
				Value: "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			route := routeWithHeaderMatch(tt.header)

			cfg := proxy.ConvertHTTPRoutes([]*gatewayv1.HTTPRoute{route}, "cluster.local")

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Matches, 1)
			require.Len(t, cfg.Rules[0].Matches[0].Headers, 1)
			assert.Equal(t, tt.expected, cfg.Rules[0].Matches[0].Headers[0])
		})
	}
}

// Helper functions.

func routeWithQueryMatch(query gatewayv1.HTTPQueryParamMatch) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "query-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path:        &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")},
							QueryParams: []gatewayv1.HTTPQueryParamMatch{query},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("svc", 80, 1),
					},
				},
			},
		},
	}
}

func routeWithFilter(filter gatewayv1.HTTPRouteFilter) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "filter-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					Filters: []gatewayv1.HTTPRouteFilter{filter},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("svc", 80, 1),
					},
				},
			},
		},
	}
}

func routeWithTimeouts(timeouts *gatewayv1.HTTPRouteTimeouts) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "timeout-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("svc", 80, 1),
					},
					Timeouts: timeouts,
				},
			},
		},
	}
}

func routeWithHeaderMatch(header gatewayv1.HTTPHeaderMatch) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "header-route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: []gatewayv1.HTTPRouteMatch{
						{
							Path:    &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")},
							Headers: []gatewayv1.HTTPHeaderMatch{header},
						},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{
						backendRef("svc", 80, 1),
					},
				},
			},
		},
	}
}

func durationPtr(val string) *gatewayv1.Duration {
	d := gatewayv1.Duration(val)

	return &d
}

func backendRef(name string, port, weight int) gatewayv1.HTTPBackendRef {
	portNum := gatewayv1.PortNumber(port)
	weightInt := int32(weight)

	return gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(name),
				Port: &portNum,
			},
			Weight: &weightInt,
		},
	}
}
