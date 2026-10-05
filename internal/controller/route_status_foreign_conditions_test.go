package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestUpdateRouteStatusGeneric_ForeignConditionInOwnEntrySurvives pins the
// RouteParentStatus.Conditions contract: a condition type this controller is
// not responsible for (the godoc's `special.io/SomeField`) MUST NOT be
// removed, changed or updated, even inside the entry carrying our
// controllerName. A foreign observedGeneration ahead of the route's is
// unrelated to ours and must not trip the stale-status guard either.
func TestUpdateRouteStatusGeneric_ForeignConditionInOwnEntrySurvives(t *testing.T) {
	t.Parallel()

	for _, foreignGeneration := range []int64{1, 5} {
		foreign := metav1.Condition{
			Type:               "special.io/SomeField",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: foreignGeneration,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second)),
			Reason:             "SomeReason",
			Message:            "set by a foreign controller",
		}

		for _, kind := range []string{"HTTPRoute", "GRPCRoute"} {
			t.Run(fmt.Sprintf("%s/foreignGeneration=%d", kind, foreignGeneration), func(t *testing.T) {
				t.Parallel()
				assertForeignConditionSurvives(t, kind, foreign)
			})
		}
	}
}

func assertForeignConditionSurvives(t *testing.T, kind string, foreign metav1.Condition) {
	t.Helper()

	namespace := gatewayv1.Namespace("ns")
	ownEntry := []gatewayv1.RouteParentStatus{{
		ParentRef:      gatewayv1.ParentReference{Name: "gw", Namespace: &namespace},
		ControllerName: "test",
		Conditions:     []metav1.Condition{foreign},
	}}
	parentRefs := []gatewayv1.ParentReference{{Name: "gw"}}
	meta := metav1.ObjectMeta{Name: "r", Namespace: "ns", Generation: 1}

	route, accessor := client.Object(&gatewayv1.HTTPRoute{
		ObjectMeta: meta,
		Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefs}},
		Status:     gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: ownEntry}},
	}), newHTTPRouteAccessor
	if kind == "GRPCRoute" {
		route, accessor = &gatewayv1.GRPCRoute{
			ObjectMeta: meta,
			Spec:       gatewayv1.GRPCRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefs}},
			Status:     gatewayv1.GRPCRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: ownEntry}},
		}, newGRPCRouteAccessor
	}

	gatewayClass, gateway := routeStatusTransitionFixtures()
	cli := fake.NewClientBuilder().
		WithScheme(newListenerSetScheme(t)).
		WithObjects(gatewayClass, gateway, route).
		WithStatusSubresource(route).
		Build()

	params := &routeStatusUpdateParams{k8sClient: cli, controllerName: "test", reconciledGeneration: 1}
	ctx := context.Background()
	routeKey := types.NamespacedName{Name: "r", Namespace: "ns"}

	require.NoError(t, updateRouteStatusGeneric(ctx, params, routeKey, accessor, routeBindingInfo{}, nil, nil))

	stored := accessor()
	require.NoError(t, cli.Get(ctx, routeKey, stored.obj))

	parents := stored.routeStatus().Parents
	require.Len(t, parents, 1)

	got := findCondition(parents[0].Conditions, foreign.Type)
	require.NotNil(t, got, "a foreign condition type in our entry must not be removed")
	assert.Equal(t, foreign, *got, "a foreign condition must be kept verbatim")
	assert.NotNil(t, findCondition(parents[0].Conditions, string(gatewayv1.RouteConditionAccepted)),
		"the foreign condition must not block our own write")
}

func TestIsControllerOwnedRouteParentConditionType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		condType string
		owned    bool
	}{
		{condType: string(gatewayv1.RouteConditionAccepted), owned: true},
		{condType: string(gatewayv1.RouteConditionResolvedRefs), owned: true},
		{condType: string(gatewayv1.RouteConditionPartiallyInvalid), owned: true},
		{condType: routeConditionShadowed, owned: true},
		{condType: routeConditionProxyConfigPushed, owned: true},
		{condType: routeConditionTunnelShared, owned: true},
		{condType: "special.io/SomeField", owned: false},
		{condType: "Programmed", owned: false},
	}

	for _, tt := range tests {
		t.Run(tt.condType, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.owned, isControllerOwnedRouteParentConditionType(tt.condType))
		})
	}
}
