package controller

import (
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/hostnameownership"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

var errTunnelGroupFailed = errors.New("data plane refused")

// TestBuildAcceptedCondition_PerParentSyncError pins per-PARENT status
// precision: a multi-parent route whose parent A binds to a Gateway with a
// refused data plane and parent B to a healthy Gateway must report
// Accepted=False on A's parentRef and Accepted=True on B's — RouteParentStatus
// is per-parent, so flipping the healthy parent to Pending is a status
// inaccuracy.
func TestBuildAcceptedCondition_PerParentSyncError(t *testing.T) {
	t.Parallel()

	binding := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: true},
			1: {Accepted: true},
		},
		parentGateways: map[int]string{
			0: "team-a/gw-failed",
			1: "team-a/gw-healthy",
		},
		syncErrByGateway: map[string]error{
			"team-a/gw-failed": errTunnelGroupFailed,
		},
	}

	now := metav1.Now()

	// Parent 0 → refused data plane: Pending. No global syncErr.
	failed := buildAcceptedCondition(1, now, binding, 0, nil, nil)
	assert.Equal(t, metav1.ConditionFalse, failed.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonPending), failed.Reason)

	// Parent 1 → healthy Gateway: Accepted, despite parent 0's refusal.
	healthy := buildAcceptedCondition(1, now, binding, 1, nil, nil)
	assert.Equal(t, metav1.ConditionTrue, healthy.Status,
		"a healthy parent must stay Accepted when a sibling parent is refused")
}

// TestBuildAcceptedCondition_GlobalSyncErrAppliesToAllParents pins the
// early-error path: when the whole sync failed before partitioning (global
// syncErr, no per-gateway map), every parent reports Pending.
func TestBuildAcceptedCondition_GlobalSyncErrAppliesToAllParents(t *testing.T) {
	t.Parallel()

	binding := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{0: {Accepted: true}},
		parentGateways: map[int]string{0: "team-a/gw"},
	}

	cond := buildAcceptedCondition(1, metav1.Now(), binding, 0, errTunnelGroupFailed, nil)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonPending), cond.Reason)
}

// TestBuildAcceptedCondition_BindingRejectionOutranksSyncError pins precedence:
// a binding rejection (e.g. HostnameNotPermitted) keeps its specific,
// actionable reason even during a TOTAL tunnel outage. The route is never
// programmed regardless of tunnel health, so the rejection — not a transient
// Pending — is the authoritative condition for the operator to act on.
func TestBuildAcceptedCondition_BindingRejectionOutranksSyncError(t *testing.T) {
	t.Parallel()

	binding := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: false, Reason: hostnameownership.RouteReasonHostnameNotPermitted, Message: "hostname outside the namespace suffix"},
		},
		parentGateways: map[int]string{0: "team-a/gw"},
	}

	cond := buildAcceptedCondition(1, metav1.Now(), binding, 0, errTunnelGroupFailed, nil)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, string(hostnameownership.RouteReasonHostnameNotPermitted), cond.Reason,
		"a binding rejection outranks a transient global sync error")
	assert.Contains(t, cond.Message, "hostname outside")
}

// TestInjectPlaneRefusals_BrokenGatewayFlagged pins that a route accepted
// only on an opted-in Gateway whose data plane did NOT resolve carries a
// per-parent error: the route is served nowhere, so its parent must not
// report Accepted=True.
func TestInjectPlaneRefusals_BrokenGatewayFlagged(t *testing.T) {
	t.Parallel()

	bindings := map[string]routeBindingInfo{
		"team-a/orphan": {acceptedGateways: map[string]bool{"team-a/gw-broken": true}},
	}

	infra := &infraGateways{
		resolved: map[string]*infraGateway{},
		broken:   map[string]bool{"team-a/gw-broken": true},
	}

	injectPlaneRefusals(bindings, infra)

	require.NotNil(t, bindings["team-a/orphan"].syncErrByGateway)
	assert.Error(t, bindings["team-a/orphan"].syncErrByGateway["team-a/gw-broken"],
		"a route on a broken (unresolvable) data plane must carry a per-parent error")
}
