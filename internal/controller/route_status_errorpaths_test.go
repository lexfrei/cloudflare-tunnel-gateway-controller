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
