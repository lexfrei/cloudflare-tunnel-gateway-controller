package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func attachedRoute(name string, hostnames []gatewayv1.Hostname, refs ...gatewayv1.ParentReference) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: refs},
			Hostnames:       hostnames,
		},
	}
}

// TestGatewayAttachedRoutes_CountsOnlyAcceptedRoutes pins the vendored
// AttachedRoutes contract on the Gateway: attachment follows allowedRoutes and
// parentRefs whatever the listener's own status, but only a route Accepted for
// the Gateway is counted, and each route counts once per listener.
//
//   - A route matching only the conflicting pair is not accepted, so it is
//     counted nowhere.
//   - A route that also matches the distinct listener is accepted, so it is
//     counted on every listener it attaches to, the conflicted ones included.
//   - A route naming the Gateway twice counts once.
func TestGatewayAttachedRoutes_CountsOnlyAcceptedRoutes(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel", Listeners: ownConflictListeners()},
	}
	ok := gatewayv1.SectionName("ok")

	onlyA := attachedRoute("only-a", []gatewayv1.Hostname{"a.example.com"}, gatewayv1.ParentReference{Name: "gw"})
	anyHost := attachedRoute("any", nil, gatewayv1.ParentReference{Name: "gw"})
	twice := attachedRoute("twice", []gatewayv1.Hostname{"b.example.com"},
		gatewayv1.ParentReference{Name: "gw", SectionName: &ok}, gatewayv1.ParentReference{Name: "gw"})

	// Binding rejects only-a, whose every match conflicts; the others bind.
	stampAccepted("test-controller", metav1.ConditionFalse, onlyA)
	stampAccepted("test-controller", metav1.ConditionTrue, anyHost, twice)

	cli := setupGatewayFakeClient(gateway, onlyA, anyHost, twice)

	reconciler := &GatewayReconciler{Client: cli, Scheme: cli.Scheme(), ControllerName: "test-controller"}

	assert.Equal(t, map[gatewayv1.SectionName]int32{"c1": 1, "c2": 1, "ok": 2},
		reconciler.countAttachedRoutes(context.Background(), gateway))
}
