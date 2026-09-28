package controller

// Error-path pins for the read-modify-write route status update (the spec
// requires Get + Update under conflict retry): a failing fresh Get must
// surface as an error so the reconcile requeues, and a failing GatewayClass
// list must propagate the same way -- with a nil managed-class set every
// parentRef would look foreign and the write would wipe our own
// RouteParentStatus entries while reporting success.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

var errStatusGetBoom = errors.New("simulated route get failure")

func TestUpdateRouteStatusGeneric_FreshGetErrorPropagates(t *testing.T) {
	t.Parallel()

	scheme := newListenerSetScheme(t)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns"},
	}

	failingClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(route).
		WithStatusSubresource(route).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				if _, isRoute := obj.(*gatewayv1.HTTPRoute); isRoute {
					return errStatusGetBoom
				}

				return nil
			},
		}).
		Build()

	err := updateRouteStatusGeneric(
		context.Background(),
		&routeStatusUpdateParams{k8sClient: failingClient, controllerName: "test"},
		types.NamespacedName{Name: "r", Namespace: "ns"},
		newHTTPRouteAccessor,
		routeBindingInfo{},
		nil,
		nil,
	)

	require.Error(t, err, "a failing fresh Get must propagate so the reconcile requeues")
	assert.ErrorIs(t, err, errStatusGetBoom)
}

func TestUpdateRouteStatusGeneric_ClassListFailurePropagatesAndPreservesStatus(t *testing.T) {
	t.Parallel()

	scheme := newListenerSetScheme(t)

	// The route already carries one of OUR parent status entries; a transient
	// GatewayClass list failure must not wipe it (a nil managed-class set
	// would make every parentRef look foreign and the write would erase our
	// entries while reporting success).
	gwName := gatewayv1.ObjectName("gw")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: gwName}},
			},
		},
		Status: gatewayv1.HTTPRouteStatus{
			RouteStatus: gatewayv1.RouteStatus{
				Parents: []gatewayv1.RouteParentStatus{
					{
						ParentRef:      gatewayv1.ParentReference{Name: gwName},
						ControllerName: "test",
						Conditions: []metav1.Condition{
							{
								Type:               string(gatewayv1.RouteConditionAccepted),
								Status:             metav1.ConditionTrue,
								Reason:             string(gatewayv1.RouteReasonAccepted),
								Message:            "ok",
								LastTransitionTime: metav1.Now(),
							},
						},
					},
				},
			},
		},
	}

	listFails := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(route).
		WithStatusSubresource(route).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				if _, isClasses := list.(*gatewayv1.GatewayClassList); isClasses {
					return errStatusGetBoom
				}

				return nil
			},
		}).
		Build()

	err := updateRouteStatusGeneric(
		context.Background(),
		&routeStatusUpdateParams{k8sClient: listFails, controllerName: "test", reconciledGeneration: 1},
		types.NamespacedName{Name: "r", Namespace: "ns"},
		newHTTPRouteAccessor,
		routeBindingInfo{},
		nil,
		nil,
	)

	require.Error(t, err,
		"a failing GatewayClass list must propagate so the reconcile requeues -- proceeding would wipe our parent entries")
	assert.ErrorIs(t, err, errStatusGetBoom)

	var refreshed gatewayv1.HTTPRoute
	require.NoError(t, listFails.Get(context.Background(), types.NamespacedName{Name: "r", Namespace: "ns"}, &refreshed))
	require.Len(t, refreshed.Status.Parents, 1,
		"existing parent status entries must survive a transient class-list failure untouched")
	assert.Equal(t, gatewayv1.GatewayController("test"), refreshed.Status.Parents[0].ControllerName)
}

// TestUpdateRouteStatusGeneric_UnreadableGatewayKeepsEntry pins that a status
// pass that cannot read a parent's Gateway leaves that parent's existing entry
// alone. Dropping it would delete the status external-dns reads for a route
// whose Gateway is only briefly unreadable.
func TestUpdateRouteStatusGeneric_UnreadableGatewayKeepsEntry(t *testing.T) {
	t.Parallel()

	scheme := newListenerSetScheme(t)

	gwName := gatewayv1.ObjectName("gw")
	gwNamespace := gatewayv1.Namespace("ns")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: gwName}},
			},
		},
		Status: gatewayv1.HTTPRouteStatus{
			RouteStatus: gatewayv1.RouteStatus{
				Parents: []gatewayv1.RouteParentStatus{
					{
						ParentRef:      gatewayv1.ParentReference{Name: gwName, Namespace: &gwNamespace},
						ControllerName: "test",
						Conditions: []metav1.Condition{
							{
								Type:               string(gatewayv1.RouteConditionAccepted),
								Status:             metav1.ConditionTrue,
								Reason:             string(gatewayv1.RouteReasonAccepted),
								Message:            "ok",
								ObservedGeneration: 1,
								LastTransitionTime: metav1.Now(),
							},
						},
					},
				},
			},
		},
	}

	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test"},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cls"},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(route, gatewayClass, gateway).
		WithStatusSubresource(route).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isGateway := obj.(*gatewayv1.Gateway); isGateway {
					return errStatusGetBoom
				}

				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	err := updateRouteStatusGeneric(
		context.Background(),
		&routeStatusUpdateParams{k8sClient: cli, controllerName: "test", reconciledGeneration: 1},
		types.NamespacedName{Name: "r", Namespace: "ns"},
		newHTTPRouteAccessor,
		routeBindingInfo{bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: false, Reason: gatewayv1.RouteReasonPending},
		}},
		nil,
		nil,
	)
	require.NoError(t, err)

	var refreshed gatewayv1.HTTPRoute
	require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "r", Namespace: "ns"}, &refreshed))
	require.Len(t, refreshed.Status.Parents, 1, "the entry of a parent whose Gateway cannot be read must survive")
	assert.Equal(t, "ok", refreshed.Status.Parents[0].Conditions[0].Message)
}

// TestUpdateRouteStatusGeneric_MissingParentDropsEntry pins the other side: a
// parent that does not exist is gone, not unreadable, so its entry is removed.
func TestUpdateRouteStatusGeneric_MissingParentDropsEntry(t *testing.T) {
	t.Parallel()

	listenerSetKind := gatewayv1.Kind(kindListenerSet)

	tests := []struct {
		name    string
		ref     gatewayv1.ParentReference
		objects []client.Object
	}{
		{name: "gateway does not exist", ref: gatewayv1.ParentReference{Name: "gw"}},
		{name: "listenerset does not exist", ref: gatewayv1.ParentReference{Name: "ls", Kind: &listenerSetKind}},
		{
			name: "listenerset parent gateway does not exist",
			ref:  gatewayv1.ParentReference{Name: "ls", Kind: &listenerSetKind},
			objects: []client.Object{&gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "ns"},
				Spec:       gatewayv1.ListenerSetSpec{ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"}},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gwNamespace := gatewayv1.Namespace("ns")
			priorRef := tt.ref
			priorRef.Namespace = &gwNamespace

			route := &gatewayv1.HTTPRoute{
				ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", Generation: 1},
				Spec: gatewayv1.HTTPRouteSpec{
					CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{tt.ref}},
				},
				Status: gatewayv1.HTTPRouteStatus{
					RouteStatus: gatewayv1.RouteStatus{
						Parents: []gatewayv1.RouteParentStatus{{
							ParentRef:      priorRef,
							ControllerName: "test",
							Conditions: []metav1.Condition{{
								Type:               string(gatewayv1.RouteConditionAccepted),
								Status:             metav1.ConditionTrue,
								Reason:             string(gatewayv1.RouteReasonAccepted),
								Message:            "ok",
								ObservedGeneration: 1,
								LastTransitionTime: metav1.Now(),
							}},
						}},
					},
				},
			}

			cli := fake.NewClientBuilder().
				WithScheme(newListenerSetScheme(t)).
				WithObjects(append([]client.Object{route, &gatewayv1.GatewayClass{
					ObjectMeta: metav1.ObjectMeta{Name: "cls"},
					Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test"},
				}}, tt.objects...)...).
				WithStatusSubresource(route).
				Build()

			err := updateRouteStatusGeneric(
				context.Background(),
				&routeStatusUpdateParams{k8sClient: cli, controllerName: "test", reconciledGeneration: 1},
				types.NamespacedName{Name: "r", Namespace: "ns"},
				newHTTPRouteAccessor,
				routeBindingInfo{},
				nil,
				nil,
			)
			require.NoError(t, err)

			var refreshed gatewayv1.HTTPRoute
			require.NoError(t, cli.Get(context.Background(), types.NamespacedName{Name: "r", Namespace: "ns"}, &refreshed))
			assert.Empty(t, refreshed.Status.Parents, "the entry of a parent that does not exist is removed")
		})
	}
}
