package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// The reason strings are public: operators alert on them.
const (
	reasonSharedAcrossNamespaces = "TunnelSharedAcrossNamespaces"
	reasonSharedWithinNamespace  = "TunnelSharedWithinNamespace"
)

func tunnelSharedDiag(message string) proxy.RouteDiagnostic {
	return tunnelSharedDiagWithReason(reasonSharedAcrossNamespaces, message)
}

func tunnelSharedDiagWithReason(reason, message string) proxy.RouteDiagnostic {
	return proxy.RouteDiagnostic{
		Namespace: "team-a",
		Name:      "a-route",
		Target:    proxy.DiagnosticTunnelShared,
		Reason:    reason,
		Message:   message,
	}
}

// TestBuildParentStatus_TunnelSharedConditionPresent pins #488: a route whose
// per-Gateway data plane shares a tunnel across namespaces carries a dedicated
// TunnelShared=True condition while Accepted REMAINS True — the sharing was
// permitted (opted into), it is just not isolation.
func TestBuildParentStatus_TunnelSharedConditionPresent(t *testing.T) {
	t.Parallel()

	status := buildParentStatusForDiag([]proxy.RouteDiagnostic{
		tunnelSharedDiag("this route's Gateway shares Cloudflare Tunnel with team-b/gw"),
	}, 1)

	accepted := findCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status,
		"Accepted MUST stay True — sharing a tunnel is supported, just not isolation")

	shared := findCondition(status.Conditions, routeConditionTunnelShared)
	require.NotNil(t, shared, "the dedicated TunnelShared condition must be present")
	assert.Equal(t, metav1.ConditionTrue, shared.Status)
	assert.Equal(t, reasonSharedAcrossNamespaces, shared.Reason)
}

// TestBuildParentStatus_TunnelSharedReasonFollowsDiagnostics pins the
// condition reason to what the diagnostics found: sharing only within one
// namespace says so, and any cross-namespace share on the route wins, since
// that is the case that crosses a tenant boundary.
func TestBuildParentStatus_TunnelSharedReasonFollowsDiagnostics(t *testing.T) {
	t.Parallel()

	t.Run("within one namespace", func(t *testing.T) {
		t.Parallel()

		status := buildParentStatusForDiag([]proxy.RouteDiagnostic{
			tunnelSharedDiagWithReason(reasonSharedWithinNamespace, "shares with team-a/gw-2"),
		}, 1)

		shared := findCondition(status.Conditions, routeConditionTunnelShared)
		require.NotNil(t, shared)
		assert.Equal(t, reasonSharedWithinNamespace, shared.Reason)
	})

	t.Run("across namespaces wins over within", func(t *testing.T) {
		t.Parallel()

		status := buildParentStatusForDiag([]proxy.RouteDiagnostic{
			tunnelSharedDiagWithReason(reasonSharedWithinNamespace, "shares with team-a/gw-2"),
			tunnelSharedDiagWithReason(reasonSharedAcrossNamespaces, "shares with team-b/gw"),
		}, 1)

		shared := findCondition(status.Conditions, routeConditionTunnelShared)
		require.NotNil(t, shared)
		assert.Equal(t, reasonSharedAcrossNamespaces, shared.Reason)
	})
}

// TestBuildParentStatus_TunnelSharedAbsentWhenNoDiagnostics pins the clearing
// contract: no collision diagnostic means no condition.
func TestBuildParentStatus_TunnelSharedAbsentWhenNoDiagnostics(t *testing.T) {
	t.Parallel()

	status := buildParentStatusForDiag(nil, 1)

	assert.Nil(t, findCondition(status.Conditions, routeConditionTunnelShared))
}
