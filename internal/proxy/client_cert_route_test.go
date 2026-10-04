package proxy_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestConvertRoutes_ClientCertResolverSeesTheRoute pins that the converter
// asks for a parent's client certificate on behalf of a specific route, so the
// caller can decide per route which parents may supply one. A parentRef
// without a namespace resolves to the route's own namespace, and a ListenerSet
// parent is passed on with its kind.
func TestConvertRoutes_ClientCertResolverSeesTheRoute(t *testing.T) {
	t.Parallel()

	type lookup struct {
		route, parent types.NamespacedName
		kind          gatewayv1.Kind
	}

	parentRefs := []gatewayv1.ParentReference{
		{Namespace: new(gatewayv1.Namespace("other")), Name: "gw-other"},
		{Name: "gw-local"},
		{Kind: new(gatewayv1.Kind("ListenerSet")), Name: "ls"},
		{Kind: new(gatewayv1.Kind("Service")), Name: "not-a-parent-kind"},
	}
	backend := gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
		Name: "svc", Port: new(gatewayv1.PortNumber(443)),
	}}
	tlsResolver := func(context.Context, string, string, int32) *proxy.BackendTLSConfig {
		return &proxy.BackendTLSConfig{ServerName: "svc"}
	}
	routeNN := types.NamespacedName{Namespace: "team", Name: "r"}
	wantLookups := []lookup{
		{route: routeNN, parent: types.NamespacedName{Namespace: "other", Name: "gw-other"}, kind: "Gateway"},
		{route: routeNN, parent: types.NamespacedName{Namespace: "team", Name: "gw-local"}, kind: "Gateway"},
		{route: routeNN, parent: types.NamespacedName{Namespace: "team", Name: "ls"}, kind: "ListenerSet"},
	}

	recorder := func(lookups *[]lookup) proxy.GatewayClientCertResolver {
		return func(_ context.Context, route, parent types.NamespacedName, kind gatewayv1.Kind) *proxy.ClientCertConfig {
			*lookups = append(*lookups, lookup{route: route, parent: parent, kind: kind})

			return nil
		}
	}

	t.Run("HTTPRoute", func(t *testing.T) {
		t.Parallel()

		var lookups []lookup

		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "r"},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefs},
				Rules:           []gatewayv1.HTTPRouteRule{{BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: backend}}}},
			},
		}

		cfg := proxy.ConvertHTTPRoutes(t.Context(), []*gatewayv1.HTTPRoute{route}, "cluster.local",
			nil, nil, tlsResolver, recorder(&lookups))

		require.Len(t, cfg.Rules, 1)
		assert.Equal(t, wantLookups, lookups)
	})

	t.Run("GRPCRoute", func(t *testing.T) {
		t.Parallel()

		var lookups []lookup

		route := &gatewayv1.GRPCRoute{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "r"},
			Spec: gatewayv1.GRPCRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefs},
				Rules:           []gatewayv1.GRPCRouteRule{{BackendRefs: []gatewayv1.GRPCBackendRef{{BackendRef: backend}}}},
			},
		}

		cfg := proxy.ConvertGRPCRoutes(t.Context(), []*gatewayv1.GRPCRoute{route}, "cluster.local",
			nil, nil, tlsResolver, recorder(&lookups))

		require.Len(t, cfg.Rules, 1)
		assert.Equal(t, wantLookups, lookups)
	})
}
