package hostnameownership_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/hostnameownership"
)

const testLabelKey = "cf.k8s.lex.la/hostname-suffix"

func toHostnames(values []string) []gatewayv1.Hostname {
	out := make([]gatewayv1.Hostname, 0, len(values))
	for _, value := range values {
		out = append(out, gatewayv1.Hostname(value))
	}

	return out
}

// TestPolicy_Evaluate_SharedVectors runs the shared semantic vectors — the
// SAME table the e2e suite drives through the CEL ValidatingAdmissionPolicy.
// Two implementations of one rule (admission fail-fast + controller-side
// authoritative enforcement); this shared table is the drift guard between
// them.
func TestPolicy_Evaluate_SharedVectors(t *testing.T) {
	t.Parallel()

	policy, err := hostnameownership.New(testLabelKey, "")
	require.NoError(t, err)

	for _, vector := range hostnameownership.Vectors() {
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()

			nsLabels := map[string]string{}
			if vector.Suffix != "" {
				nsLabels[testLabelKey] = vector.Suffix
			}

			verdict := policy.Evaluate(nsLabels, toHostnames(vector.Hostnames))

			assert.Equal(t, vector.WantAllowed, verdict.Allowed, "verdict mismatch: %s", verdict.Message)

			if !vector.WantAllowed {
				assert.NotEmpty(t, verdict.Message, "denials must carry an actionable message")
			}
		})
	}
}

// TestPolicy_NamespaceSelectorScopesEnforcement pins the policing scope: a
// namespace outside the selector is NOT policed (allowed regardless of
// labels), while a namespace inside the selector is held to the full
// fail-closed contract.
func TestPolicy_NamespaceSelectorScopesEnforcement(t *testing.T) {
	t.Parallel()

	policy, err := hostnameownership.New(testLabelKey, "tenancy=enforced")
	require.NoError(t, err)

	outside := policy.Evaluate(
		map[string]string{"team": "b"},
		toHostnames([]string{"anything.example.net"}),
	)
	assert.True(t, outside.Allowed, "namespaces outside the selector are not policed")

	insideNoLabel := policy.Evaluate(
		map[string]string{"tenancy": "enforced"},
		toHostnames([]string{"anything.example.net"}),
	)
	assert.False(t, insideNoLabel.Allowed, "a policed namespace without the suffix label fails closed")
}

// TestPolicy_EmptySelectorPolicesEverything pins the default: an empty
// selector string means every namespace is policed (fail-closed everywhere).
func TestPolicy_EmptySelectorPolicesEverything(t *testing.T) {
	t.Parallel()

	policy, err := hostnameownership.New(testLabelKey, "")
	require.NoError(t, err)

	verdict := policy.Evaluate(map[string]string{}, toHostnames([]string{"app.example.com"}))
	assert.False(t, verdict.Allowed)
}

// TestNew_InvalidSelectorErrors pins fail-loud construction: a malformed
// selector must error at startup, not silently police nothing.
func TestNew_InvalidSelectorErrors(t *testing.T) {
	t.Parallel()

	_, err := hostnameownership.New(testLabelKey, "!!!not-a-selector!!!")
	require.Error(t, err)
}

// TestNew_EmptyLabelKeyErrors pins that an empty label key is a
// misconfiguration, not an enforce-nothing no-op.
func TestNew_EmptyLabelKeyErrors(t *testing.T) {
	t.Parallel()

	_, err := hostnameownership.New("", "")
	require.Error(t, err)
}
