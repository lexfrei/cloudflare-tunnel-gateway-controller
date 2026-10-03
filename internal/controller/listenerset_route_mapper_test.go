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

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
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
// mapper enqueues the routes whose parentRef targets the given ListenerSet
// when its parent Gateway is one of ours, and not a route on another Gateway.
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
	routeOnOtherGateway := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "on-other-gateway", Namespace: "team-a"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &gwKind, Name: "other", Namespace: &ns},
				},
			},
		},
	}

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc, gw, ls).Build()

	routes := []Route{HTTPRouteWrapper{routeAttached}, HTTPRouteWrapper{routeOnOtherGateway}}
	got := findRoutesAttachedToListenerSet(context.Background(), cli, ls, testListenerSetController, routes)

	require.Len(t, got, 1, "a route on an unrelated Gateway must not be enqueued")
	assert.Equal(t, "attached", got[0].Name)
}

// TestFindRoutesAttachedToListenerSet_GatewayBoundRouteEnqueued pins that a
// route attached directly to the parent Gateway is enqueued on a ListenerSet
// event: the ListenerSet's entries take part in listener isolation, so adding
// or removing one changes which hosts that route answers, even when no route
// is attached to the ListenerSet itself.
func TestFindRoutesAttachedToListenerSet_GatewayBoundRouteEnqueued(t *testing.T) {
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
	gwKind := gatewayv1.Kind(kindGateway)
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

	got := findRoutesAttachedToListenerSet(context.Background(), cli, ls, testListenerSetController,
		[]Route{HTTPRouteWrapper{routeOnGateway}})

	require.Len(t, got, 1, "a Gateway-bound route must be enqueued on a ListenerSet event")
	assert.Equal(t, "on-gateway", got[0].Name)
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

// TestIncrementListenerSetAttachedRoutes_DeduplicatesDuplicateParentRefs pins
// that a single route listing the same ListenerSet entry twice in its
// parentRefs is counted ONCE in AttachedRoutes — duplicate parentRefs must
// not inflate the per-entry count.
func TestIncrementListenerSetAttachedRoutes_DeduplicatesDuplicateParentRefs(t *testing.T) {
	t.Parallel()

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "entry", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, ls)
	validator := routebinding.NewValidator(cli)

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	// Same ListenerSet listed twice — degenerate but legal parentRefs.
	dupRefs := []gatewayv1.ParentReference{
		{Kind: &lsKind, Name: "ls", Namespace: &lsNS},
		{Kind: &lsKind, Name: "ls", Namespace: &lsNS},
	}

	counts := map[gatewayv1.SectionName]int32{"entry": 0}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team-a"},
		Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: dupRefs}},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{
			parentStatusFor(dupRefs[0], "team-a", testListenerSetController, metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted)),
		}}},
	}

	incrementListenerSetAttachedRoutes(context.Background(), validator, testListenerSetController, ls,
		HTTPRouteWrapper{route}, counts)

	assert.Equal(t, int32(1), counts["entry"], "duplicate parentRefs to the same ListenerSet must count the route once")
}

// TestListenerSetParentRefGroup pins every ListenerSet-side parentRef reader
// to the binding rule: an omitted group or gateway.networking.k8s.io names a
// ListenerSet, while an explicit "" (the core group) or any other group names
// some other resource.
func TestListenerSetParentRefGroup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		group *gatewayv1.Group
		ours  bool
	}{
		{name: "omitted group", ours: true},
		{name: "gateway api group", group: new(gatewayv1.Group(gatewayv1.GroupName)), ours: true},
		{name: "explicit empty group", group: new(gatewayv1.Group(""))},
		{name: "foreign group", group: new(gatewayv1.Group("other.example.com"))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			kind := gatewayv1.Kind(kindListenerSet)
			ns := gatewayv1.Namespace("infra")
			ref := gatewayv1.ParentReference{Group: tt.group, Kind: &kind, Name: "ls", Namespace: &ns}
			ls := &gatewayv1.ListenerSet{ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"}}
			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team-a"},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{ref}},
				},
			}

			scheme := runtime.NewScheme()
			require.NoError(t, gatewayv1.Install(scheme))

			reconciler := &ListenerSetReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(ls).Build()}

			assert.Equal(t, tt.ours, parentRefSelectsListenerSet(ref, "team-a", ls), "attachedRoutes counting")
			assert.Equal(t, tt.ours, routeTargetsListenerSet(HTTPRouteWrapper{route}, ls), "ListenerSet to route mapper")
			assert.Equal(t, tt.ours, len(reconciler.routeToListenerSets(context.Background(), route)) > 0,
				"route to ListenerSet mapper")
		})
	}
}

// TestFindRoutesAttachedToListenerSet_SiblingListenerSetRouteEnqueued pins
// that an event on one ListenerSet enqueues a route attached to another
// ListenerSet of the same Gateway: listener isolation lets the entries of
// either one take hosts from the other.
func TestFindRoutesAttachedToListenerSet_SiblingListenerSetRouteEnqueued(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(gc.Name)},
	}
	listenerSet := func(name string) *gatewayv1.ListenerSet {
		return &gatewayv1.ListenerSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "infra"},
			Spec: gatewayv1.ListenerSetSpec{
				ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw.Name)},
			},
		}
	}
	sibling, changed := listenerSet("x"), listenerSet("y")

	ns := gatewayv1.Namespace("infra")
	lsKind := gatewayv1.Kind(kindListenerSet)
	routeOnSibling := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "on-x", Namespace: "team-a"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: gatewayv1.ObjectName(sibling.Name), Namespace: &ns},
				},
			},
		},
	}

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gc, gw, sibling, changed).Build()

	got := findRoutesAttachedToListenerSet(context.Background(), cli, changed, testListenerSetController,
		[]Route{HTTPRouteWrapper{routeOnSibling}})

	require.Len(t, got, 1, "a route on a sibling ListenerSet must be enqueued")
	assert.Equal(t, "on-x", got[0].Name)
}
