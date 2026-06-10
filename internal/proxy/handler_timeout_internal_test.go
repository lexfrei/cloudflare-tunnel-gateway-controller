package proxy

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestRuleHeaderTimeout_ExplicitZeroDisables pins the spec's explicit-zero
// semantic (HTTPRouteTimeouts: "0s" SHOULD disable the timeout entirely): a
// zero Request/Backend timeout yields no ResponseHeaderTimeout bound at all,
// and a zero value never participates in the min() with a non-zero sibling.
func TestRuleHeaderTimeout_ExplicitZeroDisables(t *testing.T) {
	t.Parallel()

	assert.Equal(t, time.Duration(0), ruleHeaderTimeout(&RouteTimeouts{Request: 0, Backend: 0}),
		"explicit zero on both knobs must mean unbounded, not instant-timeout")
	assert.Equal(t, 5*time.Second, ruleHeaderTimeout(&RouteTimeouts{Request: 0, Backend: 5 * time.Second}),
		"zero request timeout must not shadow a live backend timeout")
	assert.Equal(t, 7*time.Second, ruleHeaderTimeout(&RouteTimeouts{Request: 7 * time.Second, Backend: 0}),
		"zero backend timeout must not shadow a live request timeout")
}
