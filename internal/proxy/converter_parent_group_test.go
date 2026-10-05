package proxy_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestConvertHTTPRoutes_GatewayClientCert_ParentGroup pins the parent-cert
// lookup to the binding rule: only an omitted group or gateway.networking.k8s.io
// names a Gateway, so a parentRef with an explicit "" (the core group) or any
// other group must not pick up that Gateway's client certificate.
func TestConvertHTTPRoutes_GatewayClientCert_ParentGroup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		group    *gatewayv1.Group
		wantCert bool
	}{
		{name: "omitted group", wantCert: true},
		{name: "gateway api group", group: new(gatewayv1.Group(gatewayv1.GroupName)), wantCert: true},
		{name: "explicit empty group", group: new(gatewayv1.Group(""))},
		{name: "foreign group", group: new(gatewayv1.Group("example.com"))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pathPrefix := gatewayv1.PathMatchPathPrefix
			kind := gatewayv1.Kind("Gateway")
			cert := &proxy.ClientCertConfig{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")}

			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "default"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{
						ParentRefs: []gatewayv1.ParentReference{{Group: tt.group, Kind: &kind, Name: "gw"}},
					},
					Rules: []gatewayv1.HTTPRouteRule{{
						Matches:     []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new("/")}}},
						BackendRefs: []gatewayv1.HTTPBackendRef{backendRef("svc", 8443, 1)},
					}},
				},
			}

			tlsResolver := func(_ context.Context, _, _ string, _ int32, _ bool) *proxy.BackendTLSConfig {
				return &proxy.BackendTLSConfig{CABundlePEM: "CA", ServerName: "svc"}
			}
			certResolver := func(_ context.Context, _, _ ktypes.NamespacedName, _ gatewayv1.Kind) *proxy.ClientCertConfig {
				return cert
			}

			cfg := proxy.ConvertHTTPRoutes(t.Context(), []*gatewayv1.HTTPRoute{route}, "cluster.local",
				nil, nil, tlsResolver, certResolver)

			require.Len(t, cfg.Rules, 1)
			require.NotNil(t, cfg.Rules[0].Backends[0].TLS)
			assert.Equal(t, tt.wantCert, len(cfg.Rules[0].Backends[0].TLS.ClientCertPEM) > 0)
		})
	}
}
