package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const staleStatusController = "test-controller"

// staleParentStatus is a route status holding this controller's Accepted entry
// for a Gateway the route no longer names, plus another controller's entry.
func staleParentStatus() gatewayv1.RouteStatus {
	accepted := []metav1.Condition{{
		Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue,
		Reason: string(gatewayv1.RouteReasonAccepted), LastTransitionTime: metav1.Now(),
	}}

	return gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{
		{ParentRef: gatewayv1.ParentReference{Name: "old-gw"}, ControllerName: staleStatusController, Conditions: accepted},
		{ParentRef: gatewayv1.ParentReference{Name: "other-gw"}, ControllerName: "other.example.com/ctrl", Conditions: accepted},
	}}
}

// staleStatusObjects returns a Gateway of another controller that the route
// now names, with its GatewayClass.
func staleStatusObjects() []client.Object {
	return []client.Object{
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "other-class"},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: "other.example.com/ctrl"},
		},
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "other-gw", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "other-class"},
		},
	}
}

func staleStatusClient(t *testing.T, funcs interceptor.Funcs, route client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(append(staleStatusObjects(), route)...).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}, &gatewayv1.GRPCRoute{}).
		WithInterceptorFuncs(funcs).
		Build()
}

func ownParentEntries(parents []gatewayv1.RouteParentStatus) []gatewayv1.ParentReference {
	var refs []gatewayv1.ParentReference

	for _, parent := range parents {
		if parent.ControllerName == staleStatusController {
			refs = append(refs, parent.ParentRef)
		}
	}

	return refs
}

// TestRouteReconcile_ReleasesStaleParentStatus pins that a route which stops
// naming a Gateway this controller manages loses the status.parents entries
// this controller wrote, for both route kinds, while another controller's
// entry stays.
func TestRouteReconcile_ReleasesStaleParentStatus(t *testing.T) {
	t.Parallel()

	parentRefs := []gatewayv1.ParentReference{{Name: "other-gw"}}

	t.Run("HTTPRoute", func(t *testing.T) {
		t.Parallel()

		route := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
			Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefs}},
			Status:     gatewayv1.HTTPRouteStatus{RouteStatus: staleParentStatus()},
		}
		cli := staleStatusClient(t, interceptor.Funcs{}, route)

		reconciler := &HTTPRouteReconciler{Client: cli, ControllerName: staleStatusController}
		reconciler.startupComplete.Store(true)

		_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "r", Namespace: "default"}})
		require.NoError(t, err)

		var updated gatewayv1.HTTPRoute
		require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "r", Namespace: "default"}, &updated))
		assert.Empty(t, ownParentEntries(updated.Status.Parents), "this controller's entry is gone")
		assert.Len(t, updated.Status.Parents, 1, "the other controller's entry stays")
	})

	t.Run("GRPCRoute", func(t *testing.T) {
		t.Parallel()

		route := &gatewayv1.GRPCRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
			Spec:       gatewayv1.GRPCRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefs}},
			Status:     gatewayv1.GRPCRouteStatus{RouteStatus: staleParentStatus()},
		}
		cli := staleStatusClient(t, interceptor.Funcs{}, route)

		reconciler := &GRPCRouteReconciler{Client: cli, ControllerName: staleStatusController}
		reconciler.startupComplete.Store(true)

		_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "r", Namespace: "default"}})
		require.NoError(t, err)

		var updated gatewayv1.GRPCRoute
		require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "r", Namespace: "default"}, &updated))
		assert.Empty(t, ownParentEntries(updated.Status.Parents), "this controller's entry is gone")
		assert.Len(t, updated.Status.Parents, 1, "the other controller's entry stays")
	})
}

// TestRouteReconcile_KeepsParentStatusOnReadError pins that a parent Gateway
// which cannot be read leaves the route's entries alone and retries: a read
// failure says nothing about whether the route is still ours.
func TestRouteReconcile_KeepsParentStatusOnReadError(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{{Name: "other-gw"}},
		}},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: staleParentStatus()},
	}
	cli := staleStatusClient(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*gatewayv1.Gateway); ok {
				return assert.AnError
			}

			return c.Get(ctx, key, obj, opts...)
		},
	}, route)

	reconciler := &HTTPRouteReconciler{Client: cli, ControllerName: staleStatusController}
	reconciler.startupComplete.Store(true)

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "r", Namespace: "default"}})
	require.Error(t, err)

	var updated gatewayv1.HTTPRoute
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "r", Namespace: "default"}, &updated))
	assert.Len(t, ownParentEntries(updated.Status.Parents), 1, "the entry stays until the parent can be read")
}

// TestFindRoutesForGateway_EnqueuesRouteWithOwnStatusOnForeignGateway pins the
// mapper side: a Gateway that stops being ours (its class belongs to another
// controller) still enqueues the routes that carry this controller's status,
// so their stale entries are released. Routes without such an entry stay out.
func TestFindRoutesForGateway_EnqueuesRouteWithOwnStatusOnForeignGateway(t *testing.T) {
	t.Parallel()

	parentRefs := []gatewayv1.ParentReference{{Name: "other-gw"}}
	withStatus := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "with-status", Namespace: "default"},
		Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefs}},
		Status:     gatewayv1.HTTPRouteStatus{RouteStatus: staleParentStatus()},
	}
	without := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "without", Namespace: "default"},
		Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefs}},
	}

	cli := staleStatusClient(t, interceptor.Funcs{}, without)

	var gateway gatewayv1.Gateway
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "other-gw", Namespace: "default"}, &gateway))

	requests := FindRoutesForGateway(context.Background(), cli, &gateway, staleStatusController,
		[]Route{HTTPRouteWrapper{withStatus}, HTTPRouteWrapper{without}})

	require.Len(t, requests, 1)
	assert.Equal(t, "with-status", requests[0].Name)
}

// TestReleaseOwnParentStatus_KeepsEntriesOfAManagedRoute pins the re-check the
// release does on the fresh route: a route that names a managed Gateway again
// by the time it is read keeps its entries.
func TestReleaseOwnParentStatus_KeepsEntriesOfAManagedRoute(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{{Name: "own-gw"}},
		}},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: staleParentStatus()},
	}
	ownClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "own-class"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: staleStatusController},
	}
	ownGateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "own-gw", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "own-class"},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ownClass, ownGateway, route).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()

	key := types.NamespacedName{Name: "r", Namespace: "default"}
	require.NoError(t, releaseOwnParentStatus(context.Background(), cli, staleStatusController, key, newHTTPRouteAccessor))

	var updated gatewayv1.HTTPRoute
	require.NoError(t, cli.Get(context.Background(), key, &updated))
	assert.Len(t, ownParentEntries(updated.Status.Parents), 1)
}

// listenerSetStaleRoutes returns two routes attached through ListenerSet ls in
// namespace default, one carrying this controller's status and one without.
func listenerSetStaleRoutes() []Route {
	lsKind := gatewayv1.Kind(kindListenerSet)
	refs := []gatewayv1.ParentReference{{Kind: &lsKind, Name: "ls"}}

	return []Route{
		HTTPRouteWrapper{&gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "with-status", Namespace: "default"},
			Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: refs}},
			Status:     gatewayv1.HTTPRouteStatus{RouteStatus: staleParentStatus()},
		}},
		HTTPRouteWrapper{&gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "without", Namespace: "default"},
			Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: refs}},
		}},
	}
}

// TestStaleStatusRoutesThroughListenerSetAreEnqueued pins both ListenerSet
// paths to a route whose Gateway stopped being ours: a ListenerSet repointed
// at another controller's Gateway, and a Gateway that is no longer ours
// holding a ListenerSet. Each enqueues the routes attached through the
// ListenerSet that carry this controller's status, and only those.
func TestStaleStatusRoutesThroughListenerSetAreEnqueued(t *testing.T) {
	t.Parallel()

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "default"},
		Spec:       gatewayv1.ListenerSetSpec{ParentRef: gatewayv1.ParentGatewayReference{Name: "other-gw"}},
	}
	cli := staleStatusClient(t, interceptor.Funcs{}, ls)

	t.Run("ListenerSet event", func(t *testing.T) {
		t.Parallel()

		requests := findRoutesAttachedToListenerSet(context.Background(), cli, ls, staleStatusController, listenerSetStaleRoutes())
		require.Len(t, requests, 1)
		assert.Equal(t, "with-status", requests[0].Name)
	})

	t.Run("Gateway event", func(t *testing.T) {
		t.Parallel()

		var gateway gatewayv1.Gateway
		require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "other-gw", Namespace: "default"}, &gateway))

		requests := FindRoutesForGateway(context.Background(), cli, &gateway, staleStatusController, listenerSetStaleRoutes())
		require.Len(t, requests, 1)
		assert.Equal(t, "with-status", requests[0].Name)
	})
}

// TestFindRoutesAttachedToListenerSet_DeletedParentEnqueuesOwnStatus pins the
// ListenerSet mapper when the parent Gateway no longer exists: the routes
// carrying this controller's status are still enqueued so their entries are
// released, and the others are not.
func TestFindRoutesAttachedToListenerSet_DeletedParentEnqueuesOwnStatus(t *testing.T) {
	t.Parallel()

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "default"},
		Spec:       gatewayv1.ListenerSetSpec{ParentRef: gatewayv1.ParentGatewayReference{Name: "gone-gw"}},
	}
	cli := staleStatusClient(t, interceptor.Funcs{}, ls)

	requests := findRoutesAttachedToListenerSet(context.Background(), cli, ls, staleStatusController, listenerSetStaleRoutes())
	require.Len(t, requests, 1)
	assert.Equal(t, "with-status", requests[0].Name)
}

// TestFindRoutesForGateway_ManagedGatewayEnqueuesListenerSetRoutes pins that a
// spec change on a managed Gateway, such as tightening allowedListeners,
// enqueues the routes attached only through its ListenerSets: whether those
// routes are still admitted depends on the Gateway, not only on the routes.
func TestFindRoutesForGateway_ManagedGatewayEnqueuesListenerSetRoutes(t *testing.T) {
	t.Parallel()

	fromSame := gatewayv1.NamespacesFromSame
	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{Namespaces: &gatewayv1.ListenerNamespaces{From: &fromSame}},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "default"},
		Spec:       gatewayv1.ListenerSetSpec{ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"}},
	}
	cli := buildGatewayFakeClient(t, gc, gw, ls)

	lsKind := gatewayv1.Kind(kindListenerSet)
	route := HTTPRouteWrapper{&gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "via-ls", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{{Kind: &lsKind, Name: "ls"}},
		}},
	}}

	requests := FindRoutesForGateway(context.Background(), cli, gw, testListenerSetController, []Route{route})
	require.Len(t, requests, 1)
	assert.Equal(t, "via-ls", requests[0].Name)
}

// TestFindRoutesForGateway_SkipsListenerSetsOfOtherGateways pins that a Gateway
// event enqueues only routes on its own ListenerSets: a route attached through
// a ListenerSet of another Gateway is left out, whether the Gateway is ours or
// not, even when it carries this controller's status.
func TestFindRoutesForGateway_SkipsListenerSetsOfOtherGateways(t *testing.T) {
	t.Parallel()

	lsKind := gatewayv1.Kind(kindListenerSet)
	refs := []gatewayv1.ParentReference{{Kind: &lsKind, Name: "elsewhere-ls"}}
	route := HTTPRouteWrapper{&gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "default"},
		Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: refs}},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{
			parentStatusFor(refs[0], "default", testListenerSetController, metav1.ConditionTrue, string(gatewayv1.RouteReasonAccepted)),
		}}},
	}}

	for _, className := range []string{managedGatewayClass().Name, "other-class"} {
		t.Run(className, func(t *testing.T) {
			t.Parallel()

			gc := managedGatewayClass()
			gw := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
				Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(className)},
			}
			own := &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "own-ls", Namespace: "default"},
				Spec:       gatewayv1.ListenerSetSpec{ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"}},
			}
			elsewhere := &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "elsewhere-ls", Namespace: "default"},
				Spec:       gatewayv1.ListenerSetSpec{ParentRef: gatewayv1.ParentGatewayReference{Name: "another-gw"}},
			}
			otherClass := &gatewayv1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{Name: "other-class"},
				Spec:       gatewayv1.GatewayClassSpec{ControllerName: "other.example.com/ctrl"},
			}
			cli := buildGatewayFakeClient(t, gc, otherClass, gw, own, elsewhere)

			assert.Empty(t, FindRoutesForGateway(context.Background(), cli, gw, testListenerSetController, []Route{route}))
		})
	}
}

// TestFindRoutesForGateway_ListenerSetListErrorKeepsDirectRoutes pins the
// failure path of the ListenerSet lookup: when ListenerSets cannot be listed,
// the routes naming the Gateway directly are still enqueued and the routes
// attached only through a ListenerSet are left for the next event.
func TestFindRoutesForGateway_ListenerSetListErrorKeepsDirectRoutes(t *testing.T) {
	t.Parallel()

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "default"},
		Spec:       gatewayv1.ListenerSetSpec{ParentRef: gatewayv1.ParentGatewayReference{Name: "other-gw"}},
	}
	cli := staleStatusClient(t, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*gatewayv1.ListenerSetList); ok {
				return assert.AnError
			}

			return c.List(ctx, list, opts...)
		},
	}, ls)

	var gateway gatewayv1.Gateway
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "other-gw", Namespace: "default"}, &gateway))

	direct := HTTPRouteWrapper{&gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "direct", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{{Name: "other-gw"}},
		}},
		Status: gatewayv1.HTTPRouteStatus{RouteStatus: staleParentStatus()},
	}}

	routes := append([]Route{direct}, listenerSetStaleRoutes()...)

	requests := FindRoutesForGateway(context.Background(), cli, &gateway, staleStatusController, routes)
	require.Len(t, requests, 1)
	assert.Equal(t, "direct", requests[0].Name)
}

// TestGatewayClassDelete_ReleasesOwnParentStatus pins that deleting a class of
// ours releases the status entries its routes carry: once the class is gone no
// route is accepted any more, so the class event has to reach the routes
// through their status. A route whose status this controller never wrote is
// not enqueued.
func TestGatewayClassDelete_ReleasesOwnParentStatus(t *testing.T) {
	t.Parallel()

	refs := []gatewayv1.ParentReference{{Name: "own-gw"}}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "default"},
		Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: refs}},
		Status:     gatewayv1.HTTPRouteStatus{RouteStatus: staleParentStatus()},
	}
	untouched := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "untouched", Namespace: "default"},
		Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: refs}},
	}
	ownGateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "own-gw", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "own-class"},
	}
	deletedClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "own-class"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: staleStatusController},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(ownGateway, route, untouched).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()

	reconciler := &HTTPRouteReconciler{Client: cli, ControllerName: staleStatusController}
	reconciler.startupComplete.Store(true)

	noneAccepted := func(context.Context) []reconcile.Request { return nil }
	requests := ownClassRoutes(staleStatusController, noneAccepted, reconciler.routesHoldingOwnStatus)(
		context.Background(), deletedClass)
	require.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "r", Namespace: "default"}}}, requests)

	_, err := reconciler.Reconcile(context.Background(), requests[0])
	require.NoError(t, err)

	var updated gatewayv1.HTTPRoute
	require.NoError(t, cli.Get(context.Background(), requests[0].NamespacedName, &updated))
	assert.Empty(t, ownParentEntries(updated.Status.Parents))
}

// TestGRPCRouteReconciler_RoutesHoldingOwnStatus pins the GRPCRoute side of
// the lookup the class watch uses.
func TestGRPCRouteReconciler_RoutesHoldingOwnStatus(t *testing.T) {
	t.Parallel()

	withStatus := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "with-status", Namespace: "default"},
		Status:     gatewayv1.GRPCRouteStatus{RouteStatus: staleParentStatus()},
	}
	without := &gatewayv1.GRPCRoute{ObjectMeta: metav1.ObjectMeta{Name: "without", Namespace: "default"}}

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(withStatus, without).Build()
	reconciler := &GRPCRouteReconciler{Client: cli, ControllerName: staleStatusController}

	assert.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "with-status", Namespace: "default"}}},
		reconciler.routesHoldingOwnStatus(context.Background()))
}
