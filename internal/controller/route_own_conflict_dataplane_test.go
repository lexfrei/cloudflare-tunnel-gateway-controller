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

	out, _, _ := withEffectiveHostnames(context.Background(), buildGatewayFakeClient(t, gw), "", []*gatewayv1.HTTPRoute{route}, nil, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{"b.example.com"}, out[0].Spec.Hostnames)
}
