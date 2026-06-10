package controller

// Error-path pins for the read-modify-write route status update (the spec
// requires Get + Update under conflict retry): a failing fresh Get must
// surface as an error so the reconcile requeues, and a failing GatewayClass
// list (used only to scope which parents are ours) must degrade gracefully
// instead of blocking the status write.

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

func TestUpdateRouteStatusGeneric_ClassListFailureStillWritesStatus(t *testing.T) {
	t.Parallel()

	scheme := newListenerSetScheme(t)

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", Generation: 1},
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

	assert.NoError(t, err,
		"a failing GatewayClass list (scoping only) must degrade gracefully, not block the status write")
}
