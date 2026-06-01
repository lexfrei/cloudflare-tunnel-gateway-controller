package controller

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// Confirms the function takes the binding result into account only when
	// the syncer succeeded, matching the precedence in route_status.go.
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

	// syncErr wins over a rejected binding: when Cloudflare sync fails we
	// surface the sync error first so operators see the actionable cause.
	cond = buildAcceptedCondition(1, now, bindingInfo, 0, errCloudflareSync, nil)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonPending), cond.Reason)
}
