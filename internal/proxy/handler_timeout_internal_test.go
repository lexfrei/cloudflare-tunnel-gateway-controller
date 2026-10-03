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

	assert.Equal(t, time.Duration(0), ruleHeaderTimeout(&RouteRule{Timeouts: &RouteTimeouts{Request: 0, Backend: 0}}),
		"explicit zero on both knobs must mean unbounded, not instant-timeout")
	assert.Equal(t, 5*time.Second, ruleHeaderTimeout(&RouteRule{Timeouts: &RouteTimeouts{Request: 0, Backend: 5 * time.Second}}),
		"zero request timeout must not shadow a live backend timeout")
	assert.Equal(t, 7*time.Second, ruleHeaderTimeout(&RouteRule{Timeouts: &RouteTimeouts{Request: 7 * time.Second, Backend: 0}}),
		"zero backend timeout must not shadow a live request timeout")
}

// TestRuleHeaderTimeout_RetryKeepsOnlyBackend pins the split a retry policy
// forces: the transport's header deadline bounds one attempt (backendRequest),
// and the request timeout moves to retryTransport's budget across attempts.
func TestRuleHeaderTimeout_RetryKeepsOnlyBackend(t *testing.T) {
	t.Parallel()

	retry := &RouteRetry{Attempts: 2}

	assert.Equal(t, 2*time.Second, ruleHeaderTimeout(&RouteRule{
		Timeouts: &RouteTimeouts{Request: 10 * time.Second, Backend: 2 * time.Second},
		Retry:    retry,
	}))
	assert.Equal(t, time.Duration(0), ruleHeaderTimeout(&RouteRule{
		Timeouts: &RouteTimeouts{Request: 10 * time.Second},
		Retry:    retry,
	}))
}
