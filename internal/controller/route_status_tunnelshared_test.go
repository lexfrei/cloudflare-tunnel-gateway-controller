package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func tunnelSharedDiag(message string) proxy.RouteDiagnostic {
	return proxy.RouteDiagnostic{
		Namespace: "team-a",
		Name:      "a-route",
		Target:    proxy.DiagnosticTunnelShared,
		Reason:    routeReasonTunnelShared,
		Message:   message,
	}
}

// TestBuildParentStatus_TunnelSharedConditionPresent pins #488: a route whose
// per-Gateway data plane shares a tunnel across namespaces carries a dedicated
// TunnelShared=True condition while Accepted REMAINS True — sharing is supported,
// it is just not isolation.
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
	assert.Equal(t, routeReasonTunnelShared, shared.Reason)
}

// TestBuildParentStatus_TunnelSharedAbsentWhenNoDiagnostics pins the clearing
// contract: no collision diagnostic means no condition.
func TestBuildParentStatus_TunnelSharedAbsentWhenNoDiagnostics(t *testing.T) {
	t.Parallel()

	status := buildParentStatusForDiag(nil, 1)

	assert.Nil(t, findCondition(status.Conditions, routeConditionTunnelShared))
}
