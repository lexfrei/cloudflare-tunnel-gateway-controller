//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// connKillingServer returns a test server that drops the connection without
// writing a response for the first failures requests, then serves 200. The
// returned counter reports how many requests reached the server.
func connKillingServer(t *testing.T, failures int64) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var seen atomic.Int64

	// The handler runs on a server goroutine, so it must not call FailNow
	// (require) -- t.Error marks the test failed without terminating the
	// wrong goroutine.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if seen.Add(1) <= failures {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server response writer must support Hijack")

				return
			}

			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijacking connection: %v", err)

				return
			}

			closeErr := conn.Close()
			if closeErr != nil {
				t.Errorf("closing hijacked connection: %v", closeErr)
			}

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return server, &seen
}

func TestDoWithTransportRetry_RecoversFromTransientFailures(t *testing.T) {
	t.Parallel()

	server, seen := connKillingServer(t, transportAttempts-1)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)

	resp, err := doWithTransportRetry(server.Client(), req)
	require.NoError(t, err)

	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int64(transportAttempts), seen.Load(), "every attempt should reach the server")
}

func TestDoWithTransportRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	server, seen := connKillingServer(t, transportAttempts)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)

	start := time.Now()

	resp, err := doWithTransportRetry(server.Client(), req) //nolint:bodyclose // no response on error
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, int64(transportAttempts), seen.Load(), "must stop after transportAttempts tries")

	wantDelay := time.Duration(transportAttempts-1) * transportRetryDelay
	assert.GreaterOrEqual(t, time.Since(start), wantDelay,
		"attempts must be spaced by transportRetryDelay, not fired back-to-back")
}

func TestDoWithTransportRetry_NeverRetriesReceivedResponse(t *testing.T) {
	t.Parallel()

	// Error statuses ARE routing verdicts the e2e tests assert on (fail-closed
	// tests expect 503s); a received response must never trigger a retry.
	var seen atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)

	resp, err := doWithTransportRetry(server.Client(), req)
	require.NoError(t, err)

	defer resp.Body.Close()

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, int64(1), seen.Load(), "a received response must be returned, not retried")
}

func TestDoWithTransportRetry_NeverRetriesRequestWithBody(t *testing.T) {
	t.Parallel()

	// A body is consumed by the first attempt; a re-send would go out empty.
	// Such requests must get exactly one try.
	server, seen := connKillingServer(t, transportAttempts)

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, server.URL, strings.NewReader("payload"))
	require.NoError(t, err)

	resp, err := doWithTransportRetry(server.Client(), req) //nolint:bodyclose // no response on error
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, int64(1), seen.Load(), "a request with a body must get exactly one attempt")
}

func TestDoWithTransportRetry_StopsOnContextCancellation(t *testing.T) {
	t.Parallel()

	server, seen := connKillingServer(t, transportAttempts)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)

	resp, err := doWithTransportRetry(server.Client(), req) //nolint:bodyclose // no response on error
	require.Error(t, err)
	assert.Nil(t, resp)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int64(0), seen.Load(), "a cancelled context must not burn retry attempts")
}
