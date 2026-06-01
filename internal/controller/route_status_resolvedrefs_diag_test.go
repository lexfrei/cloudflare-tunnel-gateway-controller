package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestDiagnostics_ResolvedRefsTarget_SetsResolvedRefsFalse pins that a
// ResolvedRefs-target diagnostic (e.g. a backend declaring a TLS appProtocol
// with no BackendTLSPolicy, or an unrecognised appProtocol) sets the
// ResolvedRefs condition to False with the diagnostic's reason and message —
// the converter's per-backend protocol findings reach the route status, not
// just the ingress builder's backend-ref failures.
func TestDiagnostics_ResolvedRefsTarget_SetsResolvedRefsFalse(t *testing.T) {
	t.Parallel()

	diagnostics := []proxy.RouteDiagnostic{
		{
			Namespace: "default", Name: "web", RuleIndex: 0,
			Target:  proxy.DiagnosticResolvedRefs,
			Reason:  string(gatewayv1.RouteReasonUnsupportedProtocol),
			Message: "Service \"web-svc\" declares appProtocol \"https\" but no BackendTLSPolicy targets it",
		},
	}

	status := buildParentStatusForDiag(diagnostics, 1)

	resolved := findCondition(status.Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	require.NotNil(t, resolved)
	assert.Equal(t, metav1.ConditionFalse, resolved.Status,
		"a ResolvedRefs-target diagnostic must drive ResolvedRefs=False")
	assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedProtocol), resolved.Reason)
	assert.Contains(t, resolved.Message, "BackendTLSPolicy", "message must carry the actionable detail")

	// A ResolvedRefs diagnostic alone must not reject the whole route — the
	// route is still Accepted, the unresolved ref is the specific problem.
	accepted := findCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status)
}

// TestDiagnostics_ResolvedRefsTarget_BackendRefErrorTakesPrecedence pins that an
// ingress-builder BackendRefError (a missing Service, etc.) still drives the
// ResolvedRefs condition when both it and a converter ResolvedRefs diagnostic
// are present — the hard unresolved reference is the more fundamental problem.
func TestDiagnostics_ResolvedRefsTarget_BackendRefErrorTakesPrecedence(t *testing.T) {
	t.Parallel()

	ref := gatewayv1.ParentReference{Name: "test-gateway"}
	diagnostics := []proxy.RouteDiagnostic{
		{
			Namespace: "default", Name: "web", RuleIndex: 0,
			Target:  proxy.DiagnosticResolvedRefs,
			Reason:  string(gatewayv1.RouteReasonUnsupportedProtocol),
			Message: "appProtocol https without policy",
		},
	}
	failedRefs := []ingress.BackendRefError{
		{
			RouteNamespace: "default", RouteName: "web",
			BackendName: "missing-svc", BackendNS: "default",
			Reason: string(gatewayv1.RouteReasonBackendNotFound), Message: "Service not found",
		},
	}

	status := buildParentStatus(
		ref, "default", "example.com/controller",
		1, metav1.Now(),
		routeBindingInfo{}, 0,
		failedRefs, nil,
		nil,
		diagnostics, 1,
	)

	resolved := findCondition(status.Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	require.NotNil(t, resolved)
	assert.Equal(t, metav1.ConditionFalse, resolved.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonBackendNotFound), resolved.Reason,
		"a hard unresolved backend ref outranks a softer protocol diagnostic")
}
