package proxy_test

import (
	"testing"

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
									Value: strPtr("/"),
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
									Value: strPtr("/api/health"),
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
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: strPtr("/")}},
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
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: strPtr("/")}},
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
								Path:   &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: strPtr("/api")},
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
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: strPtr("/")}},
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
							{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: strPtr("/")}},
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

// Helper functions.

func strPtr(s string) *string {
	return &s
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
