package proxy_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// appProtoTestRoute builds a one-backend HTTPRoute for the appProtocol tests.
func appProtoTestRoute() *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"example.com"},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches:     []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}}},
					BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("web-svc", 80, 1)},
				},
			},
		},
	}
}

// TestConvertHTTPRoutes_AppProtocolHTTPS_WithoutPolicy_FailsClosed pins the
// spec-mandated behaviour: a backend declaring appProtocol https (or wss)
// without a BackendTLSPolicy cannot be served — the proxy has no trust anchor,
// so dialing plaintext to a TLS backend would silently fail. Per the Gateway
// API spec this is an unsupported app protocol: the backend fails closed (its
// traffic fraction returns HTTP 502) and a ResolvedRefs-target diagnostic with
// reason UnsupportedProtocol is recorded with an actionable message naming the
// fix (attach a BackendTLSPolicy).
func TestConvertHTTPRoutes_AppProtocolHTTPS_WithoutPolicy_FailsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		appProtocol string
	}{
		{"lowercase https", "https"},
		{"uppercase HTTPS", "HTTPS"},
		{"wss", "kubernetes.io/wss"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resolver := func(_ context.Context, _, _ string, _ int32) string { return tc.appProtocol }

			// tlsResolver = nil → no BackendTLSPolicy applies.
			cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{appProtoTestRoute()},
				"cluster.local", nil, resolver, nil, nil)

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Backends, 1)
			assert.Equal(t, http.StatusBadGateway, cfg.Rules[0].Backends[0].UnavailableStatus,
				"a TLS appProtocol without a BackendTLSPolicy must fail the backend closed")

			require.Len(t, cfg.Diagnostics, 1)
			diag := cfg.Diagnostics[0]
			assert.Equal(t, "web", diag.Name)
			assert.Equal(t, proxy.DiagnosticResolvedRefs, diag.Target)
			assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedProtocol), diag.Reason)
			assert.False(t, diag.WholeRule, "only this backend fraction fails, the rule still serves other backends")
			assert.Contains(t, diag.Message, "BackendTLSPolicy", "message must name the fix")
		})
	}
}

// TestConvertHTTPRoutes_AppProtocolHTTPS_WithPolicy_NoDiagnostic confirms the
// happy path: with a BackendTLSPolicy attached, appProtocol https is served
// over TLS — no fail-closed, no diagnostic.
func TestConvertHTTPRoutes_AppProtocolHTTPS_WithPolicy_NoDiagnostic(t *testing.T) {
	t.Parallel()

	resolver := func(_ context.Context, _, _ string, _ int32) string { return "https" }
	tlsResolver := func(_ context.Context, _, _ string, _ int32) *proxy.BackendTLSConfig {
		return &proxy.BackendTLSConfig{CABundlePEM: "ca", ServerName: "web-svc"}
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{appProtoTestRoute()},
		"cluster.local", nil, resolver, tlsResolver, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus, "a policed TLS backend must serve normally")
	assert.NotNil(t, cfg.Rules[0].Backends[0].TLS)
	assert.Empty(t, cfg.Diagnostics)
}

// TestConvertHTTPRoutes_AppProtocolUnknown_ReportsButServes pins the
// report-only treatment for an unrecognised appProtocol: the proxy keeps
// serving over HTTP/1.1 (a safe default the backend may well speak), but
// records a ResolvedRefs-target UnsupportedProtocol diagnostic so the operator
// sees the hint was not honoured. The backend is NOT failed closed.
func TestConvertHTTPRoutes_AppProtocolUnknown_ReportsButServes(t *testing.T) {
	t.Parallel()

	resolver := func(_ context.Context, _, _ string, _ int32) string { return "my-custom-proto" }

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{appProtoTestRoute()},
		"cluster.local", nil, resolver, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Equal(t, proxy.BackendProtocolHTTP, cfg.Rules[0].Backends[0].Protocol, "unknown appProtocol falls back to HTTP/1.1")
	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus, "unknown appProtocol is report-only, not fail-closed")

	require.Len(t, cfg.Diagnostics, 1)
	diag := cfg.Diagnostics[0]
	assert.Equal(t, proxy.DiagnosticResolvedRefs, diag.Target)
	assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedProtocol), diag.Reason)
	assert.Contains(t, diag.Message, "my-custom-proto", "message must name the unrecognised value")
}
