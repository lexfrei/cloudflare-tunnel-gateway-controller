package proxy_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// findEventDiag returns the first Event-target diagnostic, or a zero value and
// false when none is present.
func findEventDiag(diags []proxy.RouteDiagnostic) (proxy.RouteDiagnostic, bool) {
	for _, d := range diags {
		if d.Target == proxy.DiagnosticEvent {
			return d, true
		}
	}

	return proxy.RouteDiagnostic{}, false
}

// TestConvertHTTPRoutes_H2CSuppressedByTLS_NormalEvent pins that a backend
// declaring appProtocol kubernetes.io/h2c while also targeted by a
// BackendTLSPolicy records a benign-override Event diagnostic (Normal): TLS
// wins, the proxy does the right thing, and the operator is told the cleartext
// hint was superseded. No condition is raised — config applied successfully.
func TestConvertHTTPRoutes_H2CSuppressedByTLS_NormalEvent(t *testing.T) {
	t.Parallel()

	resolver := func(_ context.Context, _, _ string, _ int32) string { return "kubernetes.io/h2c" }
	tlsResolver := func(_ context.Context, _, _ string, _ int32) *proxy.BackendTLSConfig {
		return &proxy.BackendTLSConfig{CABundlePEM: "ca", ServerName: "web-svc"}
	}

	cfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{appProtoTestRoute()},
		"cluster.local", nil, resolver, tlsResolver, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus, "TLS-wins is a benign override, not a fail-closed")
	assert.NotNil(t, cfg.Rules[0].Backends[0].TLS, "the policy still applies — TLS wins")

	diag, ok := findEventDiag(cfg.Diagnostics)
	require.True(t, ok, "h2c suppressed by a BackendTLSPolicy must record an Event diagnostic")
	assert.Equal(t, "web", diag.Name)
	assert.Equal(t, proxy.EventTypeNormal, diag.EventType, "a TLS-wins override is a Normal event")
	assert.Contains(t, diag.Message, "h2c", "message must name the suppressed hint")
}

// TestConvertHTTPRoutes_WSHandshakeStrip_WarningEvent pins that a
// ResponseHeaderModifier removing a WebSocket handshake header on a WS-marked
// backend records a Warning Event diagnostic — the filter is honored as written
// (per spec the pipeline runs unconditionally) but the operator is warned that
// it breaks the upgrade.
func TestConvertHTTPRoutes_WSHandshakeStrip_WarningEvent(t *testing.T) {
	t.Parallel()

	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)
	wsBackend := gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{Name: "ws-svc", Port: &port},
		},
	}
	routes := []*gatewayv1.HTTPRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}}},
						Filters: []gatewayv1.HTTPRouteFilter{
							{
								Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
								ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
									Remove: []string{"Sec-WebSocket-Accept"},
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{wsBackend},
					},
				},
			},
		},
	}

	// Resolver marks the backend as WebSocket-capable.
	resolver := func(_ context.Context, _, _ string, _ int32) string { return "kubernetes.io/ws" }

	cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, resolver, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Filters, 1, "the filter is still applied as written")

	diag, ok := findEventDiag(cfg.Diagnostics)
	require.True(t, ok, "stripping a WS handshake header must record an Event diagnostic")
	assert.Equal(t, proxy.EventTypeWarning, diag.EventType, "breaking the WS upgrade is a Warning event")
	assert.Contains(t, diag.Message, "Sec-WebSocket-Accept", "message must name the stripped header")
}
