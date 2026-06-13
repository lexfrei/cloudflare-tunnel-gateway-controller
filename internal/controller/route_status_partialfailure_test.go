package controller

import (
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

var errTunnelGroupFailed = errors.New("tunnel group sync failed")

// TestBuildAcceptedCondition_PerParentSyncError pins per-PARENT status
// precision: a multi-parent route whose parent A binds to a Gateway on a
// FAILED tunnel and parent B to a Gateway on a HEALTHY tunnel must report
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

	// Parent 0 → failed tunnel: Pending. No global syncErr.
	failed := buildAcceptedCondition(1, now, binding, 0, nil, nil)
	assert.Equal(t, metav1.ConditionFalse, failed.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonPending), failed.Reason)

	// Parent 1 → healthy tunnel: Accepted, despite parent 0's failure.
	healthy := buildAcceptedCondition(1, now, binding, 1, nil, nil)
	assert.Equal(t, metav1.ConditionTrue, healthy.Status,
		"a parent on a healthy tunnel must stay Accepted when a sibling parent's tunnel failed")
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

// TestInjectPartitionSyncErrors_AttributesPerGateway pins the partition →
// per-Gateway error mapping: a failed infra-Gateway partition records its
// error only on bindings accepted on THAT Gateway; a healthy partition records
// nothing; the shared partition's failure maps onto every shared (non-infra)
// parent.
func TestInjectPartitionSyncErrors_AttributesPerGateway(t *testing.T) {
	t.Parallel()

	bindings := map[string]routeBindingInfo{
		"team-a/multi": {acceptedGateways: map[string]bool{
			"team-a/gw-failed": true, "team-a/gw-healthy": true,
		}},
		"team-b/shared-only": {acceptedGateways: map[string]bool{"sys/shared-gw": true}},
	}

	infra := &infraGateways{
		resolved: map[string]*infraGateway{
			"team-a/gw-failed":  {},
			"team-a/gw-healthy": {},
		},
		broken: map[string]bool{},
	}

	// gw-failed's own partition failed; the shared partition failed too.
	failed := map[string]error{
		"team-a/gw-failed": errTunnelGroupFailed,
		sharedPartitionKey: errTunnelGroupFailed,
	}

	injectPartitionSyncErrors(bindings, failed, infra)

	multi := bindings["team-a/multi"].syncErrByGateway
	require.ErrorIs(t, multi["team-a/gw-failed"], errTunnelGroupFailed, "the failed Gateway must carry the error")
	_, healthyHasErr := multi["team-a/gw-healthy"]
	assert.False(t, healthyHasErr, "the healthy Gateway must carry no error")

	shared := bindings["team-b/shared-only"].syncErrByGateway
	assert.ErrorIs(t, shared["sys/shared-gw"], errTunnelGroupFailed,
		"a shared-partition failure maps onto the route's shared (non-infra) parent")
}
