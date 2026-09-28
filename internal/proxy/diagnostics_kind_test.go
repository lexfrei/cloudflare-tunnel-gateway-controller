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

func extensionRef() *gatewayv1.LocalObjectReference {
	return &gatewayv1.LocalObjectReference{Group: "networking.example.net", Kind: "MyFilter", Name: "myfilter"}
}

// TestConvertRoutes_DiagnosticsCarryRouteKind pins that the converter stamps
// each diagnostic with the kind of the route it came from, so an HTTPRoute and
// a GRPCRoute sharing a namespace and name do not receive each other's.
func TestConvertRoutes_DiagnosticsCarryRouteKind(t *testing.T) {
	t.Parallel()

	meta := metav1.ObjectMeta{Name: "same", Namespace: "default"}

	httpCfg := proxy.ConvertHTTPRoutes(context.Background(), []*gatewayv1.HTTPRoute{{
		ObjectMeta: meta,
		Spec: gatewayv1.HTTPRouteSpec{Rules: []gatewayv1.HTTPRouteRule{{
			Filters: []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterExtensionRef, ExtensionRef: extensionRef()}},
		}}},
	}}, "cluster.local", nil, nil, nil, nil)

	grpcCfg := proxy.ConvertGRPCRoutes(context.Background(), []*gatewayv1.GRPCRoute{{
		ObjectMeta: meta,
		Spec: gatewayv1.GRPCRouteSpec{Rules: []gatewayv1.GRPCRouteRule{{
			Filters: []gatewayv1.GRPCRouteFilter{{Type: gatewayv1.GRPCRouteFilterExtensionRef, ExtensionRef: extensionRef()}},
		}}},
	}}, "cluster.local", nil, nil, nil, nil)

	require.NotEmpty(t, httpCfg.Diagnostics)
	require.NotEmpty(t, grpcCfg.Diagnostics)

	for _, diag := range httpCfg.Diagnostics {
		assert.Equal(t, "HTTPRoute", diag.Kind)
	}

	for _, diag := range grpcCfg.Diagnostics {
		assert.Equal(t, "GRPCRoute", diag.Kind)
	}
}

// TestDetectShadowedRules_CarriesRouteKind pins that a shadowed diagnostic
// names the losing route's kind.
func TestDetectShadowedRules_CarriesRouteKind(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Hostnames: []string{"app.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
			{Hostnames: []string{"app.example.com"}, Matches: []proxy.RouteMatch{pathPrefixMatch("/")}},
		},
		Provenance: []proxy.RuleProvenance{
			prov("HTTPRoute", "team-a", "app", shadowT0, 0),
			prov("GRPCRoute", "team-b", "intruder", shadowT1, 0),
		},
	}

	diags := proxy.DetectShadowedRules(cfg)
	require.Len(t, diags, 1)
	assert.Equal(t, "GRPCRoute", diags[0].Kind)
}
