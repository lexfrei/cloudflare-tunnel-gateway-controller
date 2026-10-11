//go:build conformance

package conformance

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/gateway-api/conformance/tests"
	"sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/gateway-api/pkg/features"
)

func TestApplyRunMode(t *testing.T) {
	t.Parallel()

	t.Run("shared data plane", func(t *testing.T) {
		t.Parallel()

		var opts suite.ConformanceOptions
		applyRunMode(&opts, false)

		assert.Empty(t, opts.Mode, "the default mode is left for the suite to name")
		assert.Contains(t, opts.ExemptFeatures, features.SupportGatewayInfrastructure)
		assert.NotContains(t, opts.SupportedFeatures, features.SupportGatewayInfrastructure)
	})

	t.Run("per-Gateway data planes", func(t *testing.T) {
		t.Parallel()

		var opts suite.ConformanceOptions
		applyRunMode(&opts, true)

		assert.Equal(t, perGatewayPlanesMode, opts.Mode, "the report must say which mode earned the claim")
		assert.Contains(t, opts.SupportedFeatures, features.SupportGatewayInfrastructure)
		assert.NotContains(t, opts.ExemptFeatures, features.SupportGatewayInfrastructure)
	})
}

// A claimed feature whose tests are skipped is a claim nothing checked.
func TestGatewayInfrastructureTestsAreNotSkipped(t *testing.T) {
	t.Parallel()

	gated := 0

	for _, ct := range tests.ConformanceTests {
		if !slices.Contains(ct.Features, features.SupportGatewayInfrastructure) {
			continue
		}

		gated++

		assert.NotContains(t, conformanceSkipTests(), ct.ShortName)
	}

	assert.Positive(t, gated, "expected the vendored suite to gate tests on GatewayInfrastructure")
}

// The suite already reports a test whose features are not all claimed as
// unsupported, so a skip entry for it changes nothing and hides the real
// reason. The one exception is the upstream -run-test flag, which bypasses
// feature gating for the named test. Only a test that some run mode would
// actually run belongs in the skip list.
func TestSkipListNamesOnlyRunnableTests(t *testing.T) {
	t.Parallel()

	runModes := []bool{false, true}
	modes := make([]sets.Set[features.FeatureName], 0, len(runModes))

	for _, perGatewayPlanes := range runModes {
		opts := suite.ConformanceOptions{
			SupportedFeatures: append(claimedFeatures(), experimentalChannelFeatures()...),
			ExemptFeatures:    exemptFeatures(),
		}
		applyRunMode(&opts, perGatewayPlanes)

		modes = append(modes, sets.New(opts.SupportedFeatures...).Difference(sets.New(opts.ExemptFeatures...)))
	}

	byName := make(map[string]suite.ConformanceTest, len(tests.ConformanceTests))
	for _, ct := range tests.ConformanceTests {
		byName[ct.ShortName] = ct
	}

	for _, name := range conformanceSkipTests() {
		ct, ok := byName[name]
		if !assert.Truef(t, ok, "skip entry %q names no test in the vendored suite", name) {
			continue
		}

		runsInSomeMode := slices.ContainsFunc(modes, func(claimed sets.Set[features.FeatureName]) bool {
			return claimed.HasAll(ct.Features...)
		})
		assert.Truef(t, runsInSomeMode,
			"skip entry %q is redundant: no run mode claims all of %v, so the suite never runs it", name, ct.Features)
	}
}
