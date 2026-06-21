package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

func proxyPushDiag(message string) proxy.RouteDiagnostic {
	return proxy.RouteDiagnostic{
		Namespace: "default",
		Name:      "web",
		Target:    proxy.DiagnosticProxyConfigPush,
		Reason:    routeReasonProxyConfigPushFailed,
		Message:   message,
	}
}

// TestBuildParentStatus_ProxyConfigPushedConditionPresent pins #487: a sustained
// proxy-push failure surfaces a dedicated ProxyConfigPushed=False condition while
// Accepted REMAINS True — the spec is valid and the tunnel document was written;
// only the in-cluster push failed.
func TestBuildParentStatus_ProxyConfigPushedConditionPresent(t *testing.T) {
	t.Parallel()

	status := buildParentStatusForDiag([]proxy.RouteDiagnostic{
		proxyPushDiag("could not push config to the data plane after sustained retries"),
	}, 1)

	accepted := findCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status,
		"Accepted MUST stay True — a push failure is a data-plane problem, not a spec problem")

	pushed := findCondition(status.Conditions, routeConditionProxyConfigPushed)
	require.NotNil(t, pushed, "the dedicated ProxyConfigPushed condition must be present")
	assert.Equal(t, metav1.ConditionFalse, pushed.Status)
	assert.Equal(t, routeReasonProxyConfigPushFailed, pushed.Reason)
}

// TestBuildParentStatus_ProxyConfigPushedAbsentWhenNoDiagnostics pins the
// clearing contract: with no push-failure diagnostic the condition is simply not
// written (parent status is fully rebuilt every sync, so recovery clears it).
func TestBuildParentStatus_ProxyConfigPushedAbsentWhenNoDiagnostics(t *testing.T) {
	t.Parallel()

	status := buildParentStatusForDiag(nil, 1)

	assert.Nil(t, findCondition(status.Conditions, routeConditionProxyConfigPushed))
}

// TestBuildParentStatus_ProxyConfigPushedOmittedWhenRejected pins that a rejected
// route does not also carry the push-failure condition — the rejection already
// tells the story.
func TestBuildParentStatus_ProxyConfigPushedOmittedWhenRejected(t *testing.T) {
	t.Parallel()

	ref := gatewayv1.ParentReference{Name: "test-gateway"}
	binding := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: false, Reason: gatewayv1.RouteReasonNoMatchingParent, Message: "no parent"},
		},
	}

	status := buildParentStatus(
		ref, "default", "example.com/controller",
		1, metav1.Now(),
		binding, 0,
		nil, nil,
		nil,
		[]proxy.RouteDiagnostic{proxyPushDiag("push failed")}, 1,
	)

	accepted := findCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, accepted)
	require.Equal(t, metav1.ConditionFalse, accepted.Status)

	assert.Nil(t, findCondition(status.Conditions, routeConditionProxyConfigPushed))
}
