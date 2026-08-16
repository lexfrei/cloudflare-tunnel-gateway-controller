package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// errCloudflareSync stands in for a transient Cloudflare Tunnel API failure
// returned by RouteSyncer.SyncAllRoutes. Defined as a package-level sentinel so
// route-status pin tests can wrap it instead of allocating ad-hoc dynamic
// errors (err113).
var errCloudflareSync = errors.New("cloudflare API 500")

func TestBuildParentStatus_IncludesPort(t *testing.T) {
	t.Parallel()

	port := gatewayv1.PortNumber(8080)
	ref := gatewayv1.ParentReference{
		Group:       new(gatewayv1.Group),
		Kind:        new(gatewayv1.Kind),
		Name:        "test-gateway",
		Port:        &port,
		SectionName: new(gatewayv1.SectionName),
	}

	status := buildParentStatus(
		ref, "default", "test-controller", 1,
		metav1.Now(), routeBindingInfo{}, 0, nil, nil, nil,
		nil, 0,
	)

	require.NotNil(t, status.ParentRef.Port)
	assert.Equal(t, port, *status.ParentRef.Port)
}

func TestBuildParentStatus_NilPort(t *testing.T) {
	t.Parallel()

	ref := gatewayv1.ParentReference{
		Name: "test-gateway",
	}

	status := buildParentStatus(
		ref, "default", "test-controller", 1,
		metav1.Now(), routeBindingInfo{}, 0, nil, nil, nil,
		nil, 0,
	)

	assert.Nil(t, status.ParentRef.Port)
}

// TestBuildParentStatus_NoMatchingParent_ConformancePin pins the parent-status
// surface required by the upstream conformance test
// HTTPRouteInvalidParentRefNotMatchingListenerPort: when the binding validator
// rejects a parentRef because no listener matched the requested port, the
// Accepted condition on the resulting parent status must be False with
// Reason=NoMatchingParent and ObservedGeneration mirroring the route. The
// ParentRef block must still echo the rejected Port so observers can correlate
// the failure with the offending parentRef.
func TestBuildParentStatus_NoMatchingParent_ConformancePin(t *testing.T) {
	t.Parallel()

	port := gatewayv1.PortNumber(81)
	ref := gatewayv1.ParentReference{
		Name: "same-namespace",
		Port: &port,
	}

	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {
				Accepted: false,
				Reason:   gatewayv1.RouteReasonNoMatchingParent,
				Message:  "No matching listener found",
			},
		},
	}

	status := buildParentStatus(
		ref, "gateway-conformance-infra", "test-controller", 7,
		metav1.Now(), bindingInfo, 0, nil, nil, nil,
		nil, 0,
	)

	require.NotNil(t, status.ParentRef.Port)
	assert.Equal(t, port, *status.ParentRef.Port)

	var accepted *metav1.Condition

	for i := range status.Conditions {
		if status.Conditions[i].Type == string(gatewayv1.RouteConditionAccepted) {
			accepted = &status.Conditions[i]

			break
		}
	}

	require.NotNil(t, accepted, "Accepted condition must be present")
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonNoMatchingParent), accepted.Reason)
	assert.Equal(t, int64(7), accepted.ObservedGeneration)
}

// TestBuildAcceptedCondition_OnlySyncErrorTriggersPending pins the contract
// between buildAcceptedCondition and the syncer: only failures from
// RouteSyncer.SyncAllRoutes (Cloudflare Tunnel API errors) are propagated
// into syncErr and demote Accepted to Reason=Pending. Proxy-push failures
// are best-effort — syncAndUpdateStatusCommon logs them and bumps the
// proxy_push sync-error metric but never wires them into syncErr. The test
// guards the docs claim in docs/reference/crd-reference.md that Accepted
// stays True when only the proxy push fails.
func TestBuildAcceptedCondition_OnlySyncErrorTriggersPending(t *testing.T) {
	t.Parallel()

	now := metav1.Now()

	// Healthy sync, healthy binding → Accepted=True.
	cond := buildAcceptedCondition(1, now, routeBindingInfo{}, 0, nil, nil)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonAccepted), cond.Reason)

	// Cloudflare sync failure → Accepted=False, Reason=Pending.
	cond = buildAcceptedCondition(1, now, routeBindingInfo{}, 0, errCloudflareSync, nil)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonPending), cond.Reason)
	assert.Equal(t, errCloudflareSync.Error(), cond.Message)

	// Binding rejection without sync error → Accepted=False, Reason from
	// the binding result (NoMatchingParent / NoMatchingListenerHostname / etc.).
	bindingInfo := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {
				Accepted: false,
				Reason:   gatewayv1.RouteReasonNoMatchingParent,
				Message:  "No matching listener found",
			},
		},
	}
	cond = buildAcceptedCondition(1, now, bindingInfo, 0, nil, nil)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonNoMatchingParent), cond.Reason)

	// A binding rejection wins over a sync error: a route that does not bind is
	// never programmed regardless of tunnel health, so its specific reason is
	// the actionable cause — a transient Pending would mask the permanent
	// problem (the route's parentRef/hostname is wrong, not the tunnel).
	cond = buildAcceptedCondition(1, now, bindingInfo, 0, errCloudflareSync, nil)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonNoMatchingParent), cond.Reason)
}

// TestBuildAcceptedCondition_BrokenDataPlaneAttributesReasonToGateway pins that
// a route blocked by a broken per-Gateway data plane reports Reason=Pending
// (InvalidParameters is a Gateway-level reason, absent from the route reason
// enum) while its message attributes InvalidParameters to the GATEWAY — so the
// route's reason and message do not contradict each other.
func TestBuildAcceptedCondition_BrokenDataPlaneAttributesReasonToGateway(t *testing.T) {
	t.Parallel()

	cond := buildAcceptedCondition(1, metav1.Now(), routeBindingInfo{}, 0, errBrokenDataPlane, nil)

	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonPending), cond.Reason,
		"a broken data plane demotes the route to Pending; InvalidParameters is not a route reason")
	assert.NotEqual(t, "InvalidParameters", cond.Reason,
		"the route must not claim a Gateway-level reason as its own")
	assert.Contains(t, cond.Message, "Gateway's Accepted condition",
		"the message must point at the Gateway, not read as the route's own InvalidParameters reason")
	assert.Contains(t, cond.Message, "InvalidParameters")
}

// routeStatusTransitionFixtures builds a managed GatewayClass and Gateway that
// route-status transition tests attach their HTTPRoute to.
func routeStatusTransitionFixtures() (*gatewayv1.GatewayClass, *gatewayv1.Gateway) {
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gwclass"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test"},
	}
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "gwclass"},
	}

	return gatewayClass, gateway
}

// TestUpdateRouteStatusGeneric_SecondSyncWithNoChangeSkipsWrite pins that a
// sync reaching the same verdict as the prior one leaves the route's
// resourceVersion and each condition's LastTransitionTime untouched -- the
// fake client bumps resourceVersion on every Status().Update, so an unchanged
// resourceVersion after a second sync proves the write itself was skipped, not
// merely that it wrote the same values again.
func TestUpdateRouteStatusGeneric_SecondSyncWithNoChangeSkipsWrite(t *testing.T) {
	t.Parallel()

	scheme := newListenerSetScheme(t)
	gatewayClass, gateway := routeStatusTransitionFixtures()

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: "gw"}},
			},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route).
		WithStatusSubresource(route).
		Build()

	params := &routeStatusUpdateParams{k8sClient: cli, controllerName: "test", reconciledGeneration: 1}
	ctx := context.Background()
	routeKey := types.NamespacedName{Name: "r", Namespace: "ns"}

	require.NoError(t, updateRouteStatusGeneric(ctx, params, routeKey, newHTTPRouteAccessor, routeBindingInfo{}, nil, nil))

	var first gatewayv1.HTTPRoute
	require.NoError(t, cli.Get(ctx, routeKey, &first))
	require.Len(t, first.Status.Parents, 1)
	require.NotEmpty(t, first.Status.Parents[0].Conditions)

	firstResourceVersion := first.ResourceVersion
	firstLastTransitionTimes := make(map[string]metav1.Time, len(first.Status.Parents[0].Conditions))

	for _, cond := range first.Status.Parents[0].Conditions {
		firstLastTransitionTimes[cond.Type] = cond.LastTransitionTime
	}

	require.NoError(t, updateRouteStatusGeneric(ctx, params, routeKey, newHTTPRouteAccessor, routeBindingInfo{}, nil, nil))

	var second gatewayv1.HTTPRoute
	require.NoError(t, cli.Get(ctx, routeKey, &second))

	assert.Equal(t, firstResourceVersion, second.ResourceVersion,
		"a no-op sync must skip the Status().Update entirely")
	require.Len(t, second.Status.Parents, 1)

	for _, cond := range second.Status.Parents[0].Conditions {
		seededAt, ok := firstLastTransitionTimes[cond.Type]
		require.True(t, ok, "condition %s must still be present", cond.Type)
		assert.True(t, cond.LastTransitionTime.Equal(&seededAt),
			"condition %s did not transition, so LastTransitionTime must be preserved", cond.Type)
	}
}

// TestUpdateRouteStatusGeneric_ForeignParentEntrySurvivesRepeatedSync pins
// that a RouteParentStatus entry owned by another controller is carried
// through untouched across repeated syncs of our own entry -- the LTT-merge
// logic must only ever look at entries whose controllerName matches ours.
func TestUpdateRouteStatusGeneric_ForeignParentEntrySurvivesRepeatedSync(t *testing.T) {
	t.Parallel()

	scheme := newListenerSetScheme(t)
	gatewayClass, gateway := routeStatusTransitionFixtures()

	foreignCondition := metav1.Condition{
		Type:               "foreign.io/Something",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second)),
		Reason:             "ForeignReason",
		Message:            "set by a foreign controller",
	}

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: "gw"}},
			},
		},
		Status: gatewayv1.HTTPRouteStatus{
			RouteStatus: gatewayv1.RouteStatus{
				Parents: []gatewayv1.RouteParentStatus{
					{
						ParentRef:      gatewayv1.ParentReference{Name: "gw"},
						ControllerName: "foreign-controller",
						Conditions:     []metav1.Condition{foreignCondition},
					},
				},
			},
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(gatewayClass, gateway, route).
		WithStatusSubresource(route).
		Build()

	params := &routeStatusUpdateParams{k8sClient: cli, controllerName: "test", reconciledGeneration: 1}
	ctx := context.Background()
	routeKey := types.NamespacedName{Name: "r", Namespace: "ns"}

	require.NoError(t, updateRouteStatusGeneric(ctx, params, routeKey, newHTTPRouteAccessor, routeBindingInfo{}, nil, nil))
	require.NoError(t, updateRouteStatusGeneric(ctx, params, routeKey, newHTTPRouteAccessor, routeBindingInfo{}, nil, nil))

	var updated gatewayv1.HTTPRoute
	require.NoError(t, cli.Get(ctx, routeKey, &updated))
	require.Len(t, updated.Status.Parents, 2, "our own entry plus the foreign entry")

	var foreignEntry *gatewayv1.RouteParentStatus

	for i := range updated.Status.Parents {
		if updated.Status.Parents[i].ControllerName == "foreign-controller" {
			foreignEntry = &updated.Status.Parents[i]
		}
	}

	require.NotNil(t, foreignEntry, "the foreign entry must survive repeated syncs of our own entry")
	require.Len(t, foreignEntry.Conditions, 1)

	got := foreignEntry.Conditions[0]
	assert.Equal(t, foreignCondition.Type, got.Type, "a foreign entry must be carried forward, never merged")
	assert.Equal(t, foreignCondition.Status, got.Status)
	assert.Equal(t, foreignCondition.ObservedGeneration, got.ObservedGeneration)
	assert.Equal(t, foreignCondition.Reason, got.Reason)
	assert.Equal(t, foreignCondition.Message, got.Message)
	assert.True(t, got.LastTransitionTime.Equal(&foreignCondition.LastTransitionTime))
}
