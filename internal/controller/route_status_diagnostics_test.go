package controller

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// buildParentStatusForDiag calls buildParentStatus with the defaults the
// diagnostic tests don't vary (no binding rejection, no failed refs, no sync
// error, no caller override). It reuses the package-level findCondition helper
// (gateway_controller_test.go) to assert on the resulting conditions.
func buildParentStatusForDiag(diagnostics []proxy.RouteDiagnostic, ruleCount int) gatewayv1.RouteParentStatus {
	ref := gatewayv1.ParentReference{Name: "test-gateway"}

	return buildParentStatus(
		ref, "default", "example.com/controller",
		1, metav1.Now(),
		routeBindingInfo{}, 0,
		nil, nil,
		nil,
		diagnostics, ruleCount,
	)
}

// TestDiagnostics_EveryRuleUnservable_AcceptedFalse pins that when every rule of
// the route is wholly unservable, the route is Accepted=False/UnsupportedValue
// and no PartiallyInvalid is set — the route serves nothing, so the Accepted
// rejection tells the whole story.
func TestDiagnostics_EveryRuleUnservable_AcceptedFalse(t *testing.T) {
	t.Parallel()

	diagnostics := []proxy.RouteDiagnostic{
		{Namespace: "default", Name: "web", RuleIndex: 0, Target: proxy.DiagnosticAccepted, Reason: string(gatewayv1.RouteReasonUnsupportedValue), Message: "rule 0 bad", WholeRule: true},
		{Namespace: "default", Name: "web", RuleIndex: 1, Target: proxy.DiagnosticAccepted, Reason: string(gatewayv1.RouteReasonUnsupportedValue), Message: "rule 1 bad", WholeRule: true},
	}

	status := buildParentStatusForDiag(diagnostics, 2)

	accepted := findCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status, "all-rules-unservable route must be Accepted=False")
	assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedValue), accepted.Reason)

	assert.Nil(t, findCondition(status.Conditions, string(gatewayv1.RouteConditionPartiallyInvalid)),
		"PartiallyInvalid must not be set when the whole route is rejected")
}

// TestDiagnostics_SomeRulesDropped_PartiallyInvalid pins that when only some
// rules are unservable, the route stays Accepted=True and gets a
// PartiallyInvalid=True condition whose message starts with the spec-mandated
// "Dropped Rule" prefix and names the affected rule index.
func TestDiagnostics_SomeRulesDropped_PartiallyInvalid(t *testing.T) {
	t.Parallel()

	diagnostics := []proxy.RouteDiagnostic{
		{Namespace: "default", Name: "web", RuleIndex: 1, Target: proxy.DiagnosticAccepted, Reason: string(gatewayv1.RouteReasonUnsupportedValue), Message: "rule 1 filter unsupported", WholeRule: true},
	}

	status := buildParentStatusForDiag(diagnostics, 3)

	accepted := findCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status, "route with surviving rules stays Accepted=True")

	partial := findCondition(status.Conditions, string(gatewayv1.RouteConditionPartiallyInvalid))
	require.NotNil(t, partial, "a partially-unservable route must carry PartiallyInvalid")
	assert.Equal(t, metav1.ConditionTrue, partial.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedValue), partial.Reason)
	assert.True(t, strings.HasPrefix(partial.Message, "Dropped Rule"),
		"PartiallyInvalid message must start with the spec-mandated 'Dropped Rule' prefix, got: %q", partial.Message)
	assert.Contains(t, partial.Message, "1", "message must name the dropped rule index")
}

// TestDiagnostics_BackendLevel_PartiallyInvalidNotAcceptedFalse pins that a
// backend-level diagnostic (WholeRule=false) on the route's only rule yields
// PartiallyInvalid — not Accepted=False — because the rule still serves its
// other backends; only one backend fraction fails closed.
func TestDiagnostics_BackendLevel_PartiallyInvalidNotAcceptedFalse(t *testing.T) {
	t.Parallel()

	diagnostics := []proxy.RouteDiagnostic{
		{Namespace: "default", Name: "web", RuleIndex: 0, Target: proxy.DiagnosticAccepted, Reason: string(gatewayv1.RouteReasonUnsupportedValue), Message: "backend filter unsupported", WholeRule: false},
	}

	status := buildParentStatusForDiag(diagnostics, 1)

	accepted := findCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status,
		"a backend-only fail-closed must not reject the whole route")

	partial := findCondition(status.Conditions, string(gatewayv1.RouteConditionPartiallyInvalid))
	require.NotNil(t, partial, "backend-level drop still warrants PartiallyInvalid")
	assert.Equal(t, metav1.ConditionTrue, partial.Status)
}

// TestDiagnostics_None_NoExtraConditions confirms the happy path: no
// diagnostics → Accepted=True and no PartiallyInvalid condition at all.
func TestDiagnostics_None_NoExtraConditions(t *testing.T) {
	t.Parallel()

	status := buildParentStatusForDiag(nil, 2)

	accepted := findCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status)

	assert.Nil(t, findCondition(status.Conditions, string(gatewayv1.RouteConditionPartiallyInvalid)),
		"a fully-valid route must not carry PartiallyInvalid")
}

// TestDiagnostics_CallerOverrideWins pins that an explicit caller override (the
// GRPCRoute reconciler's gRPC-over-quic UnsupportedProtocol) takes precedence
// over a diagnostic-derived whole-route override: the more specific reason is
// preserved on the Accepted condition.
func TestDiagnostics_CallerOverrideWins(t *testing.T) {
	t.Parallel()

	ref := gatewayv1.ParentReference{Name: "test-gateway"}
	diagnostics := []proxy.RouteDiagnostic{
		{Namespace: "default", Name: "grpc", RuleIndex: 0, Target: proxy.DiagnosticAccepted, Reason: string(gatewayv1.RouteReasonUnsupportedValue), Message: "filters unsupported", WholeRule: true},
	}

	status := buildParentStatus(
		ref, "default", "example.com/controller",
		1, metav1.Now(),
		routeBindingInfo{}, 0,
		nil, nil,
		&acceptedConditionOverride{
			reason:  string(gatewayv1.RouteReasonUnsupportedProtocol),
			message: "gRPC over quic unsupported",
		},
		diagnostics, 1,
	)

	accepted := findCondition(status.Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedProtocol), accepted.Reason,
		"the caller's more-specific override must win over the diagnostic-derived one")
}

// TestDroppedConfigMessage_TruncatesOverlongMessage pins the same
// CRD-validation guard buildShadowedCondition has: a route with very many
// dropped rules would otherwise join past metav1.Condition's 32768 Message cap
// and fail the WHOLE status update (losing Accepted/ResolvedRefs with it).
func TestDroppedConfigMessage_TruncatesOverlongMessage(t *testing.T) {
	t.Parallel()

	diagnostics := make([]proxy.RouteDiagnostic, 0, 400)
	for i := range 400 {
		diagnostics = append(diagnostics, proxy.RouteDiagnostic{
			Namespace: "default", Name: "web", RuleIndex: i, Target: proxy.DiagnosticAccepted,
			Reason:    string(gatewayv1.RouteReasonUnsupportedValue),
			Message:   strings.Repeat("x", 100) + " rule " + strconv.Itoa(i) + " dropped",
			WholeRule: true,
		})
	}

	for _, partial := range []bool{true, false} {
		msg := droppedConfigMessage(diagnostics, partial)
		assert.LessOrEqualf(t, len(msg), conditionMessageMaxLength,
			"partial=%v: the joined dropped-config message must fit metav1.Condition's MaxLength", partial)
		assert.Truef(t, strings.HasSuffix(msg, "..."), "partial=%v: a truncated message must signal the cut", partial)
	}
}

// TestTruncateConditionMessage_NeverProducesInvalidUTF8 pins that truncation
// cuts on a RUNE boundary. Shadow- and diagnostic-basis strings carry 3-byte
// em-dashes; a byte-offset cut landing inside one yields invalid UTF-8, which
// the apiserver rejects — failing the WHOLE status update (losing
// Accepted/ResolvedRefs), the exact outcome this guard exists to prevent.
func TestTruncateConditionMessage_NeverProducesInvalidUTF8(t *testing.T) {
	t.Parallel()

	// 11000 em-dashes = 33000 bytes, past the 32768 cap; the byte cut at
	// conditionMessageMaxLength-3 (32765 = 3*10921+2) lands mid-rune.
	msg := strings.Repeat("—", 11000)
	require.Greater(t, len(msg), conditionMessageMaxLength, "the fixture must actually trigger truncation")

	truncated := truncateConditionMessage(msg)

	assert.True(t, utf8.ValidString(truncated),
		"truncation must not split a multi-byte rune into invalid UTF-8")
	assert.LessOrEqual(t, len(truncated), conditionMessageMaxLength,
		"the truncated message must still fit metav1.Condition's MaxLength")
	assert.True(t, strings.HasSuffix(truncated, "..."), "a truncated message must signal the cut")
}

// TestTruncateUTF8_TinyCapIsTotal pins the totality guard: a cap at or below
// the marker length has no room for content and must not panic with a negative
// slice index. Unreachable with the real caps (256/32768); this keeps the
// helper total for any future caller.
func TestTruncateUTF8_TinyCapIsTotal(t *testing.T) {
	t.Parallel()

	for _, maxLen := range []int{0, 1, 3} {
		assert.NotPanics(t, func() { _ = truncateUTF8("a much longer message", maxLen) },
			"truncateUTF8 must not panic for maxLen=%d", maxLen)
	}
}
