//go:build conformance

package conformance

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
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
