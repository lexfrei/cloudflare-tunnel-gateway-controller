package ingress_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
)

// setupTestLogger creates a test logger that writes to a buffer and returns
// both the buffer (for assertion) and a cleanup function to restore the default logger.
func setupTestLogger() (*bytes.Buffer, func()) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	logger := slog.New(handler)
	oldDefault := slog.Default()
	slog.SetDefault(logger)

	cleanup := func() {
		slog.SetDefault(oldDefault)
	}

	return &buf, cleanup
}

func TestBuild_WarnMultipleBackendRefs(t *testing.T) {
	buf, cleanup := setupTestLogger()
	defer cleanup()

	builder := ingress.NewBuilder("cluster.local")
	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRefWithWeight("service1", nil, int32Ptr(8080)),
							newHTTPBackendRefWithWeight("service2", nil, int32Ptr(8080)),
							newHTTPBackendRefWithWeight("service3", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(routes)

	logs := buf.String()
	assert.Contains(t, logs, "route configuration partially applied")
	assert.Contains(t, logs, "route=default/test-route")
	assert.Contains(t, logs, "multiple backendRefs specified")
	assert.Contains(t, logs, "total_backends=3")
	assert.Contains(t, logs, "ignored_backends=2")
}

func TestBuild_WarnBackendRefWeights(t *testing.T) {
	buf, cleanup := setupTestLogger()
	defer cleanup()

	builder := ingress.NewBuilder("cluster.local")
	weight50 := int32(50)
	weight30 := int32(30)

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "weighted-route",
				Namespace: "production",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRefWithWeight("service1", &weight50, int32Ptr(8080)),
							newHTTPBackendRefWithWeight("service2", &weight30, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(routes)

	logs := buf.String()
	assert.Contains(t, logs, "route configuration partially applied")
	assert.Contains(t, logs, "route=production/weighted-route")
	assert.Contains(t, logs, "backendRef weight ignored")
	assert.Contains(t, logs, "traffic splitting not supported")
	// Should log for both backends with weights
	assert.Equal(t, 2, strings.Count(logs, "weight="))
}

func TestBuild_WarnHeaderMatching(t *testing.T) {
	buf, cleanup := setupTestLogger()
	defer cleanup()

	builder := ingress.NewBuilder("cluster.local")
	headerType := gatewayv1.HeaderMatchExact

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "header-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Headers: []gatewayv1.HTTPHeaderMatch{
									{
										Type:  &headerType,
										Name:  "X-Custom-Header",
										Value: "custom-value",
									},
									{
										Type:  &headerType,
										Name:  "Authorization",
										Value: "Bearer token",
									},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRefWithWeight("service1", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(routes)

	logs := buf.String()
	assert.Contains(t, logs, "route configuration partially applied")
	assert.Contains(t, logs, "route=default/header-route")
	assert.Contains(t, logs, "header matching not supported")
	assert.Contains(t, logs, "ignored_headers=2")
}

func TestBuild_WarnQueryParamMatching(t *testing.T) {
	buf, cleanup := setupTestLogger()
	defer cleanup()

	builder := ingress.NewBuilder("cluster.local")
	queryType := gatewayv1.QueryParamMatchExact

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "query-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								QueryParams: []gatewayv1.HTTPQueryParamMatch{
									{
										Type:  &queryType,
										Name:  "version",
										Value: "v2",
									},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRefWithWeight("service1", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(routes)

	logs := buf.String()
	assert.Contains(t, logs, "route configuration partially applied")
	assert.Contains(t, logs, "route=default/query-route")
	assert.Contains(t, logs, "query parameter matching not supported")
	assert.Contains(t, logs, "ignored_params=1")
}

func TestBuild_WarnMethodMatching(t *testing.T) {
	buf, cleanup := setupTestLogger()
	defer cleanup()

	builder := ingress.NewBuilder("cluster.local")
	method := gatewayv1.HTTPMethodPost

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "method-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Method: &method,
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRefWithWeight("service1", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(routes)

	logs := buf.String()
	assert.Contains(t, logs, "route configuration partially applied")
	assert.Contains(t, logs, "route=default/method-route")
	assert.Contains(t, logs, "method matching not supported")
	assert.Contains(t, logs, "ignored_method=POST")
}

func TestBuild_WarnRegularExpressionPath(t *testing.T) {
	buf, cleanup := setupTestLogger()
	defer cleanup()

	builder := ingress.NewBuilder("cluster.local")
	pathType := gatewayv1.PathMatchRegularExpression
	pathValue := "/api/v[0-9]+/.*"

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "regex-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &pathType,
									Value: &pathValue,
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRefWithWeight("service1", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(routes)

	logs := buf.String()
	assert.Contains(t, logs, "route configuration partially applied")
	assert.Contains(t, logs, "route=default/regex-route")
	assert.Contains(t, logs, "RegularExpression path type treated as PathPrefix")
	assert.Contains(t, logs, "path=/api/v[0-9]+/.*")
}

func TestBuild_WarnFilters(t *testing.T) {
	buf, cleanup := setupTestLogger()
	defer cleanup()

	builder := ingress.NewBuilder("cluster.local")
	filterType := gatewayv1.HTTPRouteFilterRequestHeaderModifier

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "filter-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: filterType,
								RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
									Set: []gatewayv1.HTTPHeader{
										{
											Name:  "X-Custom-Header",
											Value: "value",
										},
									},
								},
							},
							{
								Type: gatewayv1.HTTPRouteFilterRequestRedirect,
								RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
									Hostname: preciseHostnamePtr("redirect.example.com"),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRefWithWeight("service1", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(routes)

	logs := buf.String()
	assert.Contains(t, logs, "route configuration partially applied")
	assert.Contains(t, logs, "route=default/filter-route")
	assert.Contains(t, logs, "filters not supported")
	assert.Contains(t, logs, "ignored_filters=2")
}

func TestBuild_MultipleWarnings(t *testing.T) {
	buf, cleanup := setupTestLogger()
	defer cleanup()

	builder := ingress.NewBuilder("cluster.local")
	method := gatewayv1.HTTPMethodGet
	headerType := gatewayv1.HeaderMatchExact
	weight50 := int32(50)

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "complex-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Method: &method,
								Headers: []gatewayv1.HTTPHeaderMatch{
									{
										Type:  &headerType,
										Name:  "X-Test",
										Value: "test",
									},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRefWithWeight("service1", &weight50, int32Ptr(8080)),
							newHTTPBackendRefWithWeight("service2", &weight50, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(routes)

	logs := buf.String()
	// Should have warnings for: multiple backends, weights, method, headers
	assert.Contains(t, logs, "multiple backendRefs specified")
	assert.Contains(t, logs, "backendRef weight ignored")
	assert.Contains(t, logs, "method matching not supported")
	assert.Contains(t, logs, "header matching not supported")
	// All warnings should reference the same route
	assert.GreaterOrEqual(t, strings.Count(logs, "route=default/complex-route"), 4)
}

func TestBuild_NoWarningsForValidConfig(t *testing.T) {
	buf, cleanup := setupTestLogger()
	defer cleanup()

	builder := ingress.NewBuilder("cluster.local")
	pathType := gatewayv1.PathMatchPathPrefix
	pathValue := "/api"

	routes := []gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "valid-route",
				Namespace: "default",
			},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &pathType,
									Value: &pathValue,
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							newHTTPBackendRefWithWeight("service1", nil, int32Ptr(8080)),
						},
					},
				},
			},
		},
	}

	_ = builder.Build(routes)

	logs := buf.String()
	// Should have no warnings for a properly configured route
	assert.Empty(t, logs, "expected no warnings for valid configuration")
}

// newHTTPBackendRefWithWeight creates an HTTPBackendRef with optional weight.
func newHTTPBackendRefWithWeight(name string, weight *int32, port *int32) gatewayv1.HTTPBackendRef {
	ref := gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(name),
			},
			Weight: weight,
		},
	}
	if port != nil {
		portNum := gatewayv1.PortNumber(*port)
		ref.BackendRef.Port = &portNum
	}

	return ref
}

// preciseHostnamePtr converts string to *gatewayv1.PreciseHostname.
func preciseHostnamePtr(s string) *gatewayv1.PreciseHostname {
	h := gatewayv1.PreciseHostname(s)
	return &h
}
