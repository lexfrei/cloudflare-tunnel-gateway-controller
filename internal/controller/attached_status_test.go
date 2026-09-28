package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/hostnameownership"
)

// parentStatusFor is this controller's status entry for ref, as the route
// status writer records it, with the given Accepted verdict.
func parentStatusFor(
	ref gatewayv1.ParentReference,
	routeNamespace, controllerName string,
	accepted metav1.ConditionStatus,
	reason string,
) gatewayv1.RouteParentStatus {
	return gatewayv1.RouteParentStatus{
		ParentRef:      statusParentRef(ref, routeNamespace),
		ControllerName: gatewayv1.GatewayController(controllerName),
		Conditions: []metav1.Condition{{
			Type: string(gatewayv1.RouteConditionAccepted), Status: accepted, Reason: reason,
			LastTransitionTime: metav1.Now(),
		}},
	}
}

// TestGatewayAttachedRoutes_FollowsRouteAcceptedStatus pins attachedRoutes to
// the route's own Accepted verdict for this Gateway, which includes rejections
// made after binding: a route refused by the hostname-ownership policy and the
// losing route of a cross-type conflict both count 0.
func TestGatewayAttachedRoutes_FollowsRouteAcceptedStatus(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel", Listeners: httpListener()},
	}
	ref := gatewayv1.ParentReference{Name: "gw"}

	withStatus := func(name string, accepted metav1.ConditionStatus, reason string) *gatewayv1.HTTPRoute {
		route := attachedRoute(name, nil, ref)
		route.Status.Parents = []gatewayv1.RouteParentStatus{
			parentStatusFor(ref, "default", "test-controller", accepted, reason),
		}

		return route
	}

	loser := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "loser", Namespace: "default"},
		Spec:       gatewayv1.GRPCRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{ref}}},
		Status: gatewayv1.GRPCRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{
			parentStatusFor(ref, "default", "test-controller", metav1.ConditionFalse, string(routeReasonConflicted)),
		}}},
	}

	cli := setupGatewayFakeClient(gateway,
		withStatus("accepted", metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted)),
		withStatus("forbidden", metav1.ConditionFalse, string(hostnameownership.RouteReasonHostnameNotPermitted)),
		loser,
	)

	reconciler := &GatewayReconciler{Client: cli, Scheme: cli.Scheme(), ControllerName: "test-controller"}

	assert.Equal(t, map[gatewayv1.SectionName]int32{"http": 1}, reconciler.countAttachedRoutes(context.Background(), gateway, nil))
}

// TestListenerSetAttachedRoutes_FollowsRouteAcceptedStatus pins the same rule
// on a ListenerSet entry.
func TestListenerSetAttachedRoutes_FollowsRouteAcceptedStatus(t *testing.T) {
	t.Parallel()

	fromSame := gatewayv1.NamespacesFromSame
	lsKind := gatewayv1.Kind(kindListenerSet)
	ref := gatewayv1.ParentReference{Kind: &lsKind, Name: "ls"}

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{Namespaces: &gatewayv1.ListenerNamespaces{From: &fromSame}},
			Listeners:        []gatewayv1.Listener{{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{{
				Name: "entry", Port: 8081, Protocol: gatewayv1.HTTPProtocolType,
				AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()}},
			}},
		},
	}

	route := func(name string, accepted metav1.ConditionStatus, reason string) *gatewayv1.HTTPRoute {
		return &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "infra"},
			Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{ref}}},
			Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{
				parentStatusFor(ref, "infra", testListenerSetController, accepted, reason),
			}}},
		}
	}

	r, cli := newListenerSetReconcilerWithObjects(t, newListenerSetScheme(t), gc, gw, ls,
		route("accepted", metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted)),
		route("forbidden", metav1.ConditionFalse, string(hostnameownership.RouteReasonHostnameNotPermitted)),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "ls", Namespace: "infra"}})
	require.NoError(t, err)

	updated := getListenerSet(t, cli, "ls", "infra")
	require.Len(t, updated.Status.Listeners, 1)
	assert.Equal(t, int32(1), updated.Status.Listeners[0].AttachedRoutes)
}
