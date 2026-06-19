package controller

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

func shadowedDiag(message string) proxy.RouteDiagnostic {
	return proxy.RouteDiagnostic{
		Namespace: "default",
		Name:      "loser",
		RuleIndex: 0,
		Target:    proxy.DiagnosticShadowed,
		Reason:    "HostnameMatchShadowed",
		Message:   message,
	}
}

// TestBuildParentStatus_ShadowedConditionPresent pins the #474 contract: a
// shadowed route carries the dedicated condition with the diagnostic message,
// while Accepted REMAINS True — per the Gateway API spec, same-hostname routes
// merge legally and a successfully-bound route must stay Accepted.
func TestBuildParentStatus_ShadowedConditionPresent(t *testing.T) {
	t.Parallel()

	status := buildParentStatusForDiag([]proxy.RouteDiagnostic{
		shadowedDiag(`rule 0 match (host "app.example.com") is shadowed by HTTPRoute team-a/app rule 0 (older creationTimestamp)`),
	}, 1)

	accepted := findCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status,
		"Accepted MUST stay True for a shadowed-but-bound route (spec: same-hostname routes merge)")

	shadowed := findCondition(status.Conditions, routeConditionShadowed)
	require.NotNil(t, shadowed, "the dedicated shadowed condition must be present")
	assert.Equal(t, metav1.ConditionTrue, shadowed.Status)
	assert.Equal(t, routeReasonShadowed, shadowed.Reason)
	assert.Contains(t, shadowed.Message, "HTTPRoute team-a/app")
}

// TestBuildParentStatus_ShadowedConditionAbsentWhenNoDiagnostics pins the
// clearing contract: with no shadow diagnostics the condition is simply not
// written (parent status entries are fully rebuilt on every sync).
func TestBuildParentStatus_ShadowedConditionAbsentWhenNoDiagnostics(t *testing.T) {
	t.Parallel()

	status := buildParentStatusForDiag(nil, 1)

	assert.Nil(t, findCondition(status.Conditions, routeConditionShadowed))
}

// TestBuildParentStatus_ShadowedOmittedWhenRejected pins that a rejected
// route does not also carry the shadowed condition — the rejection already
// tells the story, mirroring the PartiallyInvalid gating.
func TestBuildParentStatus_ShadowedOmittedWhenRejected(t *testing.T) {
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
		[]proxy.RouteDiagnostic{shadowedDiag("shadowed")}, 1,
	)

	accepted := findCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, accepted)
	require.Equal(t, metav1.ConditionFalse, accepted.Status)

	assert.Nil(t, findCondition(status.Conditions, routeConditionShadowed))
}

// TestBuildShadowedCondition_TruncatesOverlongMessage pins the CRD-validation
// guard: metav1.Condition caps Message at 32768, and a route with very many
// shadowed pairs would otherwise join past the cap and fail the WHOLE status
// update — losing Accepted/ResolvedRefs along with the diagnostic.
func TestBuildShadowedCondition_TruncatesOverlongMessage(t *testing.T) {
	t.Parallel()

	diagnostics := make([]proxy.RouteDiagnostic, 0, 400)
	for i := range 400 {
		diagnostics = append(diagnostics, shadowedDiag(
			strings.Repeat("x", 100)+" pair "+strconv.Itoa(i)+" shadowed"))
	}

	shadowed := buildShadowedCondition(diagnostics, 1, metav1.Now())
	require.NotNil(t, shadowed)
	assert.LessOrEqual(t, len(shadowed.Message), 32768,
		"the joined message must fit metav1.Condition's MaxLength or the status update fails validation")
	assert.True(t, strings.HasSuffix(shadowed.Message, "..."),
		"a truncated message must signal the cut")
}

// TestBuildParentStatus_ShadowedAggregatesMessages pins that multiple shadowed
// pairs collapse into ONE condition with deduplicated, joined detail.
func TestBuildParentStatus_ShadowedAggregatesMessages(t *testing.T) {
	t.Parallel()

	status := buildParentStatusForDiag([]proxy.RouteDiagnostic{
		shadowedDiag("pair one shadowed"),
		shadowedDiag("pair two shadowed"),
		shadowedDiag("pair one shadowed"), // duplicate must not repeat
	}, 2)

	shadowed := findCondition(status.Conditions, routeConditionShadowed)
	require.NotNil(t, shadowed)
	assert.Contains(t, shadowed.Message, "pair one shadowed")
	assert.Contains(t, shadowed.Message, "pair two shadowed")
	assert.Equal(t, 1, strings.Count(shadowed.Message, "pair one shadowed"))
}
