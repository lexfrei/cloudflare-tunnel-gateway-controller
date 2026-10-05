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

const unenforceableCause = "BackendTLSPolicy default/web-tls cannot be enforced"

func unenforceableTLSResolver(_ context.Context, _, _ string, _ int32) *proxy.BackendTLSConfig {
	return &proxy.BackendTLSConfig{ServerName: "web-svc", Unenforceable: unenforceableCause}
}

// TestConvertRoutes_UnenforceableBackendTLSPolicyInvalidatesBackendRef pins
// the HTTPBackendRef godoc: a BackendTLSPolicy the implementation cannot meet
// makes the backendRef invalid, so the route MUST carry ResolvedRefs=False
// and the backend's traffic fraction gets the invalid-backendRef 500 without
// a dial. A mirror target is reported too, without failing the main request.
func TestConvertRoutes_UnenforceableBackendTLSPolicyInvalidatesBackendRef(t *testing.T) {
	t.Parallel()

	mirrorRoute := appProtoTestRoute()
	mirrorRoute.Spec.Rules[0].Filters = []gatewayv1.HTTPRouteFilter{{
		Type: gatewayv1.HTTPRouteFilterRequestMirror,
		RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
			BackendRef: gatewayv1.BackendObjectReference{Name: "mirror-svc", Port: new(gatewayv1.PortNumber(80))},
		},
	}}

	grpcRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{Rules: []gatewayv1.GRPCRouteRule{{
			BackendRefs: []gatewayv1.GRPCBackendRef{{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "web-svc", Port: new(gatewayv1.PortNumber(80))},
			}}},
		}}},
	}

	tests := []struct {
		name    string
		convert func() *proxy.Config
		minDiag int
	}{
		{
			name: "HTTPRoute backend",
			convert: func() *proxy.Config {
				return proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{appProtoTestRoute()},
					"cluster.local", nil, nil, unenforceableTLSResolver, nil)
			},
			minDiag: 1,
		},
		{
			name: "HTTPRoute backend and mirror",
			convert: func() *proxy.Config {
				return proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{mirrorRoute},
					"cluster.local", nil, nil, unenforceableTLSResolver, nil)
			},
			minDiag: 2,
		},
		{
			name: "GRPCRoute backend",
			convert: func() *proxy.Config {
				return proxy.ConvertGRPCRoutes(context.Background(), []*gatewayv1.GRPCRoute{grpcRoute},
					"cluster.local", nil, nil, unenforceableTLSResolver, nil)
			},
			minDiag: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := tt.convert()

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Backends, 1)
			assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[0].UnavailableStatus,
				"an invalid backendRef gets 500 for its traffic fraction")

			require.Len(t, cfg.Diagnostics, tt.minDiag)

			for _, diag := range cfg.Diagnostics {
				assert.Equal(t, proxy.DiagnosticResolvedRefs, diag.Target)
				assert.Equal(t, proxy.ReasonInvalidBackendTLSPolicy, diag.Reason)
				assert.False(t, diag.WholeRule)
				assert.Contains(t, diag.Message, unenforceableCause)
			}

			mirrorReports := 0

			for _, diag := range cfg.Diagnostics {
				if strings.Contains(diag.Message, "the main request is unaffected") {
					mirrorReports++
				}
			}

			assert.Equal(t, tt.minDiag-1, mirrorReports, "only the mirror leg says the main request is unaffected")
		})
	}
}
