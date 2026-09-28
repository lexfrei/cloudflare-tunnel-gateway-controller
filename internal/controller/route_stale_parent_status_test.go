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
