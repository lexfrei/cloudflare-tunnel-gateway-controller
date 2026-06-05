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

// grpcAppProtoRoute builds a one-backend GRPCRoute for the appProtocol tests:
// an Exact service+method match routed to echo-svc:8443.
func grpcAppProtoRoute() *gatewayv1.GRPCRoute {
	svc := "grpc.examples.echo.Echo"
	method := "UnaryEcho"

	return &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{
			Rules: []gatewayv1.GRPCRouteRule{
				{
					Matches: []gatewayv1.GRPCRouteMatch{
						{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc, Method: &method}},
					},
					BackendRefs: []gatewayv1.GRPCBackendRef{grpcBackendRef("echo-svc", 8443, 1)},
				},
			},
		},
	}
}

// TestConvertGRPCRoutes_AppProtocolTLS_WithoutPolicy_FailsClosed pins the
// HTTP-vs-gRPC consistency fix (issue #438): a gRPC backend whose Service port
// declares a TLS appProtocol (https / HTTPS / kubernetes.io/wss) with no
// BackendTLSPolicy cannot be served — the proxy has no trust anchor, so dialing
// cleartext h2c to a TLS backend would silently defeat the operator's stated TLS
// intent. The backend fails closed (HTTP 502 for its traffic fraction) and a
// ResolvedRefs-target diagnostic with reason UnsupportedProtocol is recorded,
// mirroring the HTTP path (appprotocol_failclosed_test.go).
func TestConvertGRPCRoutes_AppProtocolTLS_WithoutPolicy_FailsClosed(t *testing.T) {
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

			protocolResolver := func(_ context.Context, _, _ string, _ int32) string { return tc.appProtocol }

			// tlsResolver = nil → no BackendTLSPolicy applies.
			cfg := proxy.ConvertGRPCRoutes(context.Background(), []*gatewayv1.GRPCRoute{grpcAppProtoRoute()},
				"cluster.local", nil, protocolResolver, nil, nil)

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Backends, 1)
			assert.Equal(t, http.StatusBadGateway, cfg.Rules[0].Backends[0].UnavailableStatus,
				"a TLS appProtocol without a BackendTLSPolicy must fail the gRPC backend closed")

			require.Len(t, cfg.Diagnostics, 1)
			diag := cfg.Diagnostics[0]
			assert.Equal(t, "echo", diag.Name)
			assert.Equal(t, proxy.DiagnosticResolvedRefs, diag.Target)
			assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedProtocol), diag.Reason)
			assert.False(t, diag.WholeRule, "only this backend fraction fails, the rule still serves other backends")
			assert.Contains(t, diag.Message, "BackendTLSPolicy", "message must name the fix")
		})
	}
}

// TestConvertGRPCRoutes_AppProtocolTLS_WithPolicy_NoDiagnostic confirms the
// happy path: a TLS appProtocol WITH a BackendTLSPolicy attached is served over
// TLS (ALPN negotiates HTTP/2), the h2c marker is dropped, and no diagnostic is
// recorded — the policy supplies the missing trust anchor.
func TestConvertGRPCRoutes_AppProtocolTLS_WithPolicy_NoDiagnostic(t *testing.T) {
	t.Parallel()

	protocolResolver := func(_ context.Context, _, _ string, _ int32) string { return "https" }
	tlsResolver := func(_ context.Context, _, _ string, _ int32) *proxy.BackendTLSConfig {
		return &proxy.BackendTLSConfig{CABundlePEM: "ca", ServerName: "echo-svc"}
	}

	cfg := proxy.ConvertGRPCRoutes(context.Background(), []*gatewayv1.GRPCRoute{grpcAppProtoRoute()},
		"cluster.local", nil, protocolResolver, tlsResolver, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	backend := cfg.Rules[0].Backends[0]
	assert.Zero(t, backend.UnavailableStatus, "a policed TLS gRPC backend must serve normally")
	require.NotNil(t, backend.TLS)
	assert.True(t, strings.HasPrefix(backend.URL, "https://"), "policed TLS backend keeps the https scheme, got %q", backend.URL)
	assert.Equal(t, proxy.BackendProtocolHTTP, backend.Protocol, "the h2c marker is dropped — ALPN negotiates HTTP/2 over TLS")
	assert.Empty(t, cfg.Diagnostics)
}

// TestConvertGRPCRoutes_AppProtocolCleartext_StaysH2C pins that every non-TLS
// appProtocol hint keeps the gRPC default: cleartext h2c. gRPC is HTTP/2 by
// definition, so unset, kubernetes.io/h2c, and even an unrecognised value all
// dial h2c (the correct transport regardless) with no diagnostic — only the
// TLS-vs-cleartext decision is honoured on the gRPC path, not a full HTTP/1.1
// fallback.
func TestConvertGRPCRoutes_AppProtocolCleartext_StaysH2C(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		appProtocol string
	}{
		{"unset", ""},
		{"h2c", "kubernetes.io/h2c"},
		{"http", "http"},
		{"unknown", "my-custom-proto"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			protocolResolver := func(_ context.Context, _, _ string, _ int32) string { return tc.appProtocol }

			cfg := proxy.ConvertGRPCRoutes(context.Background(), []*gatewayv1.GRPCRoute{grpcAppProtoRoute()},
				"cluster.local", nil, protocolResolver, nil, nil)

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Backends, 1)
			backend := cfg.Rules[0].Backends[0]
			assert.Equal(t, proxy.BackendProtocolH2C, backend.Protocol, "a non-TLS appProtocol keeps the gRPC h2c default")
			assert.True(t, strings.HasPrefix(backend.URL, "http://"), "h2c is cleartext, URL must be http://, got %q", backend.URL)
			assert.Zero(t, backend.UnavailableStatus, "a non-TLS appProtocol does not fail the gRPC backend closed")
			assert.Empty(t, cfg.Diagnostics, "a non-TLS appProtocol records no diagnostic on the gRPC path")
		})
	}
}
