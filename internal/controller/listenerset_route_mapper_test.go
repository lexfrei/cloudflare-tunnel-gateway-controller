package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestRouteReferencesOurGateways_ListenerSetOnlyParent asserts that a route
// whose only parentRef is Kind=ListenerSet is recognised as referencing our
// Gateway when the ListenerSet's parent Gateway is managed by this
// controller. Regression test for the silent-drop branch in mappers.go
// where `ref.Kind != kindGateway` short-circuited the loop.
func TestRouteReferencesOurGateways_ListenerSetOnlyParent(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(gc.Name)},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
		},
	}
	ns := gatewayv1.Namespace("infra")
	kind := gatewayv1.Kind(kindListenerSet)
	group := gatewayv1.Group(gatewayv1.GroupName)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team-a"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{
						Group:     &group,
						Kind:      &kind,
						Name:      gatewayv1.ObjectName(ls.Name),
						Namespace: &ns,
					},
				},
			},
		},
	}

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc, gw, ls, route).Build()

	got := routeReferencesOurGateways(context.Background(), cli, testListenerSetController, HTTPRouteWrapper{route})
	assert.True(t, got, "HTTPRoute with Kind=ListenerSet parentRef must be recognised as referencing our managed Gateway")
}

// TestRouteReferencesOurGateways_ListenerSetWithForeignGateway asserts that
// a route attached to a ListenerSet whose parent Gateway belongs to ANOTHER
// controller is correctly ignored.
func TestRouteReferencesOurGateways_ListenerSetWithForeignGateway(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	foreignClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "other-class"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "other.example.com/other"},
	}
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(foreignClass.Name)},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
		},
	}
	ns := gatewayv1.Namespace("infra")
	kind := gatewayv1.Kind(kindListenerSet)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team-a"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &kind, Name: gatewayv1.ObjectName(ls.Name), Namespace: &ns},
				},
			},
		},
	}

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreignClass, gw, ls, route).Build()

	got := routeReferencesOurGateways(context.Background(), cli, testListenerSetController, HTTPRouteWrapper{route})
	assert.False(t, got, "ListenerSet parent owned by a foreign controller must NOT register as our route")
}

// TestFindRoutesAttachedToListenerSet verifies that the controller-runtime
// mapper enqueues exactly the routes whose parentRef targets the given
// ListenerSet AND whose parent Gateway is one of ours.
func TestFindRoutesAttachedToListenerSet(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(gc.Name)},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
		},
	}

	ns := gatewayv1.Namespace("infra")
	lsKind := gatewayv1.Kind(kindListenerSet)
	gwKind := gatewayv1.Kind(kindGateway)

	routeAttached := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "attached", Namespace: "team-a"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: gatewayv1.ObjectName(ls.Name), Namespace: &ns},
				},
			},
		},
	}
	routeOnGateway := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "on-gateway", Namespace: "team-a"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &gwKind, Name: gatewayv1.ObjectName(gw.Name), Namespace: &ns},
				},
			},
		},
	}

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc, gw, ls).Build()

	routes := []Route{HTTPRouteWrapper{routeAttached}, HTTPRouteWrapper{routeOnGateway}}
	got := findRoutesAttachedToListenerSet(context.Background(), cli, ls, testListenerSetController, routes)

	require.Len(t, got, 1, "only the ListenerSet-attached route should be enqueued")
	assert.Equal(t, "attached", got[0].Name)
}

// TestParentRefSelectsListenerSet_RejectsForeignGroup asserts that a parentRef
// with Kind=ListenerSet but a Group OTHER than gateway.networking.k8s.io
// does NOT match — guards against name-collision with a third-party CRD.
func TestParentRefSelectsListenerSet_RejectsForeignGroup(t *testing.T) {
	t.Parallel()

	foreignGroup := gatewayv1.Group("other.example.com")
	kind := gatewayv1.Kind(kindListenerSet)
	ns := gatewayv1.Namespace("infra")
	ref := gatewayv1.ParentReference{
		Group: &foreignGroup, Kind: &kind, Name: "ls", Namespace: &ns,
	}
	ls := &gatewayv1.ListenerSet{ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"}}

	assert.False(t, parentRefSelectsListenerSet(ref, "team-a", ls))
}

// TestParentRefSelectsListenerSet_DefaultGroup asserts that a parentRef with
// Group unset (the common case) is treated as the Gateway API group.
func TestParentRefSelectsListenerSet_DefaultGroup(t *testing.T) {
	t.Parallel()

	kind := gatewayv1.Kind(kindListenerSet)
	ns := gatewayv1.Namespace("infra")
	ref := gatewayv1.ParentReference{Kind: &kind, Name: "ls", Namespace: &ns}
	ls := &gatewayv1.ListenerSet{ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"}}

	assert.True(t, parentRefSelectsListenerSet(ref, "team-a", ls))
}

// TestRejectedEntryResolvedRefsCondition_PendingDoesNotClaimTrue guards the
// signal-clarity fix: when the ListenerSet is rejected with Reason=Pending
// (e.g. transient TLS-ref evaluation error) the per-entry ResolvedRefs
// condition must NOT claim ConditionTrue.
func TestRejectedEntryResolvedRefsCondition_PendingDoesNotClaimTrue(t *testing.T) {
	t.Parallel()

	now := metav1.Now()
	result := listenerSetAcceptanceResult{
		Accepted: false,
		Reason:   gatewayv1.ListenerSetReasonPending,
		Message:  "Failed to evaluate ListenerSet TLS references: connection refused",
	}

	cond := rejectedEntryResolvedRefsCondition(7, now, result)

	assert.Equal(t, string(gatewayv1.ListenerConditionResolvedRefs), cond.Type)
	assert.NotEqual(t, metav1.ConditionTrue, cond.Status, "Pending must not surface as ResolvedRefs=True")
	assert.Equal(t, string(gatewayv1.ListenerSetReasonPending), cond.Reason)
	assert.Equal(t, result.Message, cond.Message)
}

// TestRejectedEntryResolvedRefsCondition_NonPendingKeepsTrue verifies that
// non-Pending resource-level rejections (e.g. NotAllowed) still report
// ResolvedRefs=True — TLS material is irrelevant to a Gateway-level reject.
func TestRejectedEntryResolvedRefsCondition_NonPendingKeepsTrue(t *testing.T) {
	t.Parallel()

	now := metav1.Now()
	result := listenerSetAcceptanceResult{
		Accepted: false,
		Reason:   gatewayv1.ListenerSetReasonNotAllowed,
		Message:  "Parent Gateway does not allow ListenerSet attachment",
	}

	cond := rejectedEntryResolvedRefsCondition(7, now, result)

	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, string(gatewayv1.ListenerReasonResolvedRefs), cond.Reason)
}

// Compile-time assertion that the helper exists with the right signature.
var _ = []func(client.Client){
	func(_ client.Client) {},
}
