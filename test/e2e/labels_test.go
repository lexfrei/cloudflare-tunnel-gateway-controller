//go:build e2e

package e2e

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kubernetesLabelValueRE matches the Kubernetes label-value rules
// per https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/:
// up to 63 chars, must start AND end with [a-zA-Z0-9], inner chars may
// include [-_.]. Empty value also valid but we never emit empty.
var kubernetesLabelValueRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// TestSubtestLabelValue_Deterministic pins the contract that a given
// t.Name() input always maps to the same label value. Cleanup helpers
// rely on this -- the defer at the end of a subtest must filter by
// the SAME value the createHTTPRoute call inside the subtest stamped,
// or the route survives and pollutes the next run.
func TestSubtestLabelValue_Deterministic(t *testing.T) {
	t.Parallel()

	const name = "TestHTTPRouteConformance/Core/HTTPRouteSimpleSameNamespace"

	first := subtestLabelValue(name)
	second := subtestLabelValue(name)

	assert.Equal(t, first, second,
		"subtestLabelValue must be deterministic; same input must yield the same output")
}

// TestSubtestLabelValue_DistinguishesSubtests pins that semantically
// different subtest paths produce different label values. If two
// siblings collided, one subtest's defer would wipe the other's
// routes -- exactly the bug #265 is fixing -- so this guard fails
// loudly the moment a future implementation introduces a collision.
func TestSubtestLabelValue_DistinguishesSubtests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		nameA, nameB string
	}{
		{"TestHTTPRouteConformance/Core/A", "TestHTTPRouteConformance/Core/B"},
		{"TestHTTPRouteConformance/Core/A", "TestHTTPRouteConformance/Extended/A"},
		{"TestHTTPRouteConformance", "TestHTTPRouteConformance/Core"},
		{"TestA/B/C", "TestA/B/D"},
		{"", "TestA"},
	}

	for _, tc := range tests {
		a := subtestLabelValue(tc.nameA)
		b := subtestLabelValue(tc.nameB)
		assert.NotEqual(t, a, b,
			"different subtest names must hash to different label values: %q vs %q both produced %q",
			tc.nameA, tc.nameB, a)
	}
}

// TestSubtestLabelValue_KubernetesLabelFormat pins that the emitted
// value always parses as a valid Kubernetes label value -- 63 chars
// or fewer, regex match -- regardless of input shape. Catches a
// future implementation that drops the hash and emits the raw
// t.Name() (which would contain "/" -- illegal in label values).
func TestSubtestLabelValue_KubernetesLabelFormat(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"",
		"TestA",
		"TestA/B",
		"TestA/B/C",
		"TestHTTPRouteConformance/Extended/HTTPRouteRegexQueryParamMatching",
		"TestVeryLongName" + string(make([]byte, 200)), // pad with NULs to stress-test length capping
	}

	for _, in := range inputs {
		got := subtestLabelValue(in)

		require.LessOrEqual(t, len(got), 63,
			"subtestLabelValue(%q) = %q must be at most 63 chars to fit Kubernetes label rules", in, got)
		require.True(t, kubernetesLabelValueRE.MatchString(got),
			"subtestLabelValue(%q) = %q must match the Kubernetes label-value regex", in, got)
	}
}

// TestSubtestLabels_HasOwnerKey pins that subtestLabels returns the
// owner key with the value computed by subtestLabelValue. This is the
// contract createHTTPRoute / deleteAllRoutes depend on; if the key
// drifts (typo, accidental rename), cleanup stops scoping and we
// regress to the namespace-wide wipe #265 is fixing.
func TestSubtestLabels_HasOwnerKey(t *testing.T) {
	t.Parallel()

	// Use a sub-test so t.Name() includes a "/" path -- exercises the
	// realistic shape that prod call sites pass in.
	t.Run("nested", func(t *testing.T) {
		t.Parallel()

		labels := subtestLabels(t)
		require.Contains(t, labels, ownerLabelKey,
			"subtestLabels must include the owner key %q", ownerLabelKey)

		want := subtestLabelValue(t.Name())
		assert.Equal(t, want, labels[ownerLabelKey],
			"subtestLabels[owner] must equal subtestLabelValue(t.Name())")
	})
}
