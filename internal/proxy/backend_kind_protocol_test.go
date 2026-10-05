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

func serviceImportRef(name string, port gatewayv1.PortNumber) gatewayv1.BackendRef {
	group := gatewayv1.Group(proxy.ServiceImportGroup)
	kind := gatewayv1.Kind(proxy.ServiceImportKind)
	weight := int32(1)

	return gatewayv1.BackendRef{
		BackendObjectReference: gatewayv1.BackendObjectReference{
			Group: &group, Kind: &kind, Name: gatewayv1.ObjectName(name), Port: &port,
		},
		Weight: &weight,
	}
}

// A Service port's appProtocol describes that Service only, so a ServiceImport
// backend sharing its name must not inherit it.
func TestConvertHTTPRoutes_ServiceImportIgnoresLocalServiceAppProtocol(t *testing.T) {
	t.Parallel()

	for _, appProtocol := range []string{"kubernetes.io/h2c", "https", "kubernetes.io/wss"} {
		t.Run(appProtocol, func(t *testing.T) {
			t.Parallel()

			routes := []*gatewayv1.HTTPRoute{{
				ObjectMeta: metav1.ObjectMeta{Name: "import", Namespace: "default"},
				Spec: gatewayv1.HTTPRouteSpec{
					Rules: []gatewayv1.HTTPRouteRule{{
						BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: serviceImportRef("shared", 8080)}},
					}},
				},
			}}

			resolver := func(context.Context, string, string, int32) string { return appProtocol }

			cfg := proxy.ConvertHTTPRoutes(context.Background(), routes, "cluster.local", nil, resolver, nil, nil)

			require.Len(t, cfg.Rules, 1)
			require.Len(t, cfg.Rules[0].Backends, 1)

			backend := cfg.Rules[0].Backends[0]
			assert.Equal(t, proxy.BackendProtocolHTTP, backend.Protocol)
			assert.False(t, backend.WebSocket)
			assert.Zero(t, backend.UnavailableStatus)
		})
	}
}

func TestConvertGRPCRoutes_ServiceImportIgnoresLocalServiceAppProtocol(t *testing.T) {
	t.Parallel()

	svc := "grpc.examples.echo.Echo"
	routes := []*gatewayv1.GRPCRoute{{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{
			Rules: []gatewayv1.GRPCRouteRule{{
				Matches: []gatewayv1.GRPCRouteMatch{
					{Method: &gatewayv1.GRPCMethodMatch{Type: grpcExact(), Service: &svc}},
				},
				BackendRefs: []gatewayv1.GRPCBackendRef{{BackendRef: serviceImportRef("shared", 9000)}},
			}},
		},
	}}

	resolver := func(context.Context, string, string, int32) string { return "https" }

	cfg := proxy.ConvertGRPCRoutes(context.Background(), routes, "cluster.local", nil, resolver, nil, nil)

	require.Len(t, cfg.Rules, 1)
	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.NotEqual(t, http.StatusInternalServerError, cfg.Rules[0].Backends[0].UnavailableStatus)
	assert.Equal(t, proxy.BackendProtocolH2C, cfg.Rules[0].Backends[0].Protocol)
}
