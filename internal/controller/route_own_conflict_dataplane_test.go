package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestWithEffectiveHostnames_OwnConflictedListenerNotInherited pins that a
// route on a Gateway whose own listeners conflict inherits only the hostname
// of the distinct listener: neither listener of the conflicting pair is
// programmed, so the data plane must not serve its hostname.
func TestWithEffectiveHostnames_OwnConflictedListenerNotInherited(t *testing.T) {
	t.Parallel()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec:       gatewayv1.GatewaySpec{Listeners: ownConflictListeners()},
	}
	gwKind := gatewayv1.Kind(kindGateway)
	gwNS := gatewayv1.Namespace("infra")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{{Kind: &gwKind, Name: "gw", Namespace: &gwNS}},
		}},
	}

	out, _ := withEffectiveHostnames(context.Background(), buildGatewayFakeClient(t, gw), "", []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{"b.example.com"}, out[0].Spec.Hostnames)
}

// TestWithDefaultRedirectScheme_OwnConflictedListenerDoesNotSeedScheme pins
// the same rule for the redirect scheme: a conflicting pair of HTTPS listeners
// is not programmed, so only the distinct HTTP listener seeds the scheme.
func TestWithDefaultRedirectScheme_OwnConflictedListenerDoesNotSeedScheme(t *testing.T) {
	t.Parallel()

	fromAll := &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()}}
	hostA := gatewayv1.Hostname("a.example.com")
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{
			{Name: "c1", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: &hostA, AllowedRoutes: fromAll},
			{Name: "c2", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: &hostA, AllowedRoutes: fromAll},
			{Name: "ok", Port: 80, Protocol: gatewayv1.HTTPProtocolType, AllowedRoutes: fromAll},
		}},
	}

	out := withDefaultRedirectScheme(context.Background(), buildGatewayFakeClient(t, gw), "",
		[]*gatewayv1.HTTPRoute{redirectRoute(nil)}, nil)
	require.Len(t, out, 1)

	got := redirectFilterScheme(out[0])
	require.NotNil(t, got)
	assert.Equal(t, "http", *got)
}
