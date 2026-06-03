package proxy_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestConvertHTTPRoutes_UnsupportedRuleFilter_FailsClosedWithDiagnostic pins
// the spec-mandated behaviour for an unsupported rule-level filter
// (ExtensionRef / ExternalAuth / unknown): the rule fails closed (HTTP 500 for
// matched requests) and the converter records an Accepted-target diagnostic
// carrying UnsupportedValue with an actionable message. Per the Gateway API
// spec such a filter MUST NOT be silently dropped.
func TestConvertHTTPRoutes_UnsupportedRuleFilter_FailsClosedWithDiagnostic(t *testing.T) {
	t.Parallel()

	extName := gatewayv1.ObjectName("myfilter")
	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterExtensionRef,
								ExtensionRef: &gatewayv1.LocalObjectReference{
									Group: "networking.example.net",
									Kind:  "MyFilter",
									Name:  extName,
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("web-svc", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].UnavailableStatus,
		"rule with an unsupported filter must fail closed")

	require.Len(t, cfg.Diagnostics, 1)
	diag := cfg.Diagnostics[0]
	assert.Equal(t, "default", diag.Namespace)
	assert.Equal(t, "web", diag.Name)
	assert.Equal(t, 0, diag.RuleIndex)
	assert.Equal(t, proxy.DiagnosticAccepted, diag.Target)
	assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedValue), diag.Reason)
	assert.True(t, diag.WholeRule, "a rule-level unsupported filter takes the whole rule down")
	assert.Contains(t, diag.Message, "ExtensionRef", "message must name the offending filter type")
	assert.Contains(t, diag.Message, "500", "message must name the consequence")
}

// TestConvertHTTPRoutes_SupportedFilter_NoDiagnostic confirms the happy path:
// a supported filter produces no diagnostic and leaves the rule servable.
func TestConvertHTTPRoutes_SupportedFilter_NoDiagnostic(t *testing.T) {
	t.Parallel()

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
								RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
									Set: []gatewayv1.HTTPHeader{{Name: "X-Test", Value: "1"}},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("web-svc", 80, 1)},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Zero(t, cfg.Rules[0].UnavailableStatus)
	assert.Empty(t, cfg.Diagnostics)
}

// TestConvertHTTPRoutes_UnsupportedBackendFilter_FailsBackendClosed pins that
// an unsupported per-backend filter fails that backend closed (its traffic
// fraction returns HTTP 500) and records a diagnostic, while the rule itself
// stays servable for any sibling backends.
func TestConvertHTTPRoutes_UnsupportedBackendFilter_FailsBackendClosed(t *testing.T) {
	t.Parallel()

	extName := gatewayv1.ObjectName("myfilter")
	backend := backendRef("web-svc", 80, 1)
	backend.Filters = []gatewayv1.HTTPRouteFilter{
		{
			Type: gatewayv1.HTTPRouteFilterExtensionRef,
			ExtensionRef: &gatewayv1.LocalObjectReference{
				Group: "networking.example.net",
				Kind:  "MyFilter",
				Name:  extName,
			},
		},
	}

	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules:     []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{backend}}},
			},
		},
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[0].UnavailableStatus,
		"backend with an unsupported filter must fail that backend closed")

	require.Len(t, cfg.Diagnostics, 1)
	assert.Equal(t, proxy.DiagnosticAccepted, cfg.Diagnostics[0].Target)
	assert.False(t, cfg.Diagnostics[0].WholeRule,
		"a backend-level filter affects only the backend fraction, not the whole rule")
	assert.Zero(t, cfg.Rules[0].UnavailableStatus,
		"the rule itself stays servable — only the backend fraction fails closed")
}

// TestConvertGRPCRoutes_FiltersFailClosedWithDiagnostic pins that a GRPCRoute
// rule carrying an unsupported filter (RequestMirror — extended, not served yet)
// fails closed and records an Accepted-target diagnostic, rather than the rule
// serving silently without the declared filter. The core header-modifier filters
// are served (see TestConvertGRPCRoutes_RequestHeaderModifierServed).
func TestConvertGRPCRoutes_FiltersFailClosedWithDiagnostic(t *testing.T) {
	t.Parallel()

	svc := gatewayv1.ObjectName("grpc-svc")
	port := gatewayv1.PortNumber(9000)
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Filters: []gatewayv1.GRPCRouteFilter{
							{Type: gatewayv1.GRPCRouteFilterRequestMirror},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{Name: svc, Port: &port},
								},
							},
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].UnavailableStatus)

	require.Len(t, cfg.Diagnostics, 1)
	diag := cfg.Diagnostics[0]
	assert.Equal(t, "grpc", diag.Name)
	assert.Equal(t, proxy.DiagnosticAccepted, diag.Target)
	assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedValue), diag.Reason)
	assert.True(t, diag.WholeRule, "gRPC filters fail the whole rule closed")
	assert.True(t, strings.Contains(diag.Message, "filter"), "message must mention filters")
}

// TestConvertGRPCRoutes_BackendFiltersFailClosedWithDiagnostic pins that a
// GRPCRoute *backend*-scoped unsupported filter (RequestMirror) fails that
// backend closed — its traffic fraction returns HTTP 500 — and records a
// backend-scope diagnostic, rather than being silently dropped while the rule
// serves on. This is the gRPC analogue of the HTTP per-backend filter
// fail-closed; the core header modifiers serve instead of failing closed.
func TestConvertGRPCRoutes_BackendFiltersFailClosedWithDiagnostic(t *testing.T) {
	t.Parallel()

	svc := gatewayv1.ObjectName("grpc-svc")
	port := gatewayv1.PortNumber(9000)
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{Name: svc, Port: &port},
								},
								Filters: []gatewayv1.GRPCRouteFilter{
									{Type: gatewayv1.GRPCRouteFilterRequestMirror},
								},
							},
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Zero(t, cfg.Rules[0].UnavailableStatus, "a backend-scoped filter must not fail the whole rule closed")
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[0].UnavailableStatus,
		"the backend carrying an unsupported filter must fail that backend closed")

	require.Len(t, cfg.Diagnostics, 1)
	diag := cfg.Diagnostics[0]
	assert.Equal(t, proxy.DiagnosticAccepted, diag.Target)
	assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedValue), diag.Reason)
	assert.False(t, diag.WholeRule, "a backend-scoped filter affects only the backend fraction")
	assert.Contains(t, diag.Message, "filter", "message must mention filters")
}

// TestConvertGRPCRoutes_RequestHeaderModifierServed pins the Gateway API core
// requirement (grpcroute_types.go:229,510): the core RequestHeaderModifier and
// the extended ResponseHeaderModifier filters MUST be served for GRPCRoute. gRPC
// metadata is carried as HTTP/2 headers, so they route through the same
// header-modifier pipeline as HTTPRoute. The rule must serve (no 500) and carry
// the converted filters.
func TestConvertGRPCRoutes_RequestHeaderModifierServed(t *testing.T) {
	t.Parallel()

	svc := gatewayv1.ObjectName("grpc-svc")
	port := gatewayv1.PortNumber(9000)
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						Filters: []gatewayv1.GRPCRouteFilter{
							{
								Type: gatewayv1.GRPCRouteFilterRequestHeaderModifier,
								RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
									Set: []gatewayv1.HTTPHeader{{Name: "X-Tenant", Value: "acme"}},
								},
							},
							{
								Type: gatewayv1.GRPCRouteFilterResponseHeaderModifier,
								ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
									Add: []gatewayv1.HTTPHeader{{Name: "X-Served-By", Value: "proxy"}},
								},
							},
						},
						BackendRefs: []gatewayv1.GRPCBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{Name: svc, Port: &port},
								},
							},
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Zero(t, cfg.Rules[0].UnavailableStatus, "core header-modifier filters must serve, not fail closed")
	assert.Empty(t, cfg.Diagnostics, "serving a supported filter must not record a fail-closed diagnostic")

	require.Len(t, cfg.Rules[0].Filters, 2)
	assert.Equal(t, proxy.FilterRequestHeaderModifier, cfg.Rules[0].Filters[0].Type)
	require.NotNil(t, cfg.Rules[0].Filters[0].RequestHeaderModifier)
	require.Len(t, cfg.Rules[0].Filters[0].RequestHeaderModifier.Set, 1)
	assert.Equal(t, "X-Tenant", cfg.Rules[0].Filters[0].RequestHeaderModifier.Set[0].Name)
	assert.Equal(t, proxy.FilterResponseHeaderModifier, cfg.Rules[0].Filters[1].Type)
	require.NotNil(t, cfg.Rules[0].Filters[1].ResponseHeaderModifier)
	require.Len(t, cfg.Rules[0].Filters[1].ResponseHeaderModifier.Add, 1)
	assert.Equal(t, "X-Served-By", cfg.Rules[0].Filters[1].ResponseHeaderModifier.Add[0].Name)
}

// TestConvertGRPCRoutes_BackendHeaderModifierServed pins that a backend-scoped
// gRPC RequestHeaderModifier is served on that backend (not failed closed),
// mirroring the HTTPRoute per-backend header-modifier support.
func TestConvertGRPCRoutes_BackendHeaderModifierServed(t *testing.T) {
	t.Parallel()

	svc := gatewayv1.ObjectName("grpc-svc")
	port := gatewayv1.PortNumber(9000)
	routes := []*gatewayv1.GRPCRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "default"},
			Spec: gatewayv1.GRPCRouteSpec{
				Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
				Rules: []gatewayv1.GRPCRouteRule{
					{
						BackendRefs: []gatewayv1.GRPCBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{Name: svc, Port: &port},
								},
								Filters: []gatewayv1.GRPCRouteFilter{
									{
										Type: gatewayv1.GRPCRouteFilterRequestHeaderModifier,
										RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
											Set: []gatewayv1.HTTPHeader{{Name: "X-Backend", Value: "v1"}},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, nil, nil)

	require.Len(t, cfg.Rules, 1)
	assert.Zero(t, cfg.Rules[0].UnavailableStatus)
	assert.Empty(t, cfg.Diagnostics)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus, "a supported backend filter must serve")
	require.Len(t, cfg.Rules[0].Backends[0].Filters, 1)
	assert.Equal(t, proxy.FilterRequestHeaderModifier, cfg.Rules[0].Backends[0].Filters[0].Type)
	require.NotNil(t, cfg.Rules[0].Backends[0].Filters[0].RequestHeaderModifier)
	assert.Equal(t, "X-Backend", cfg.Rules[0].Backends[0].Filters[0].RequestHeaderModifier.Set[0].Name)
}
