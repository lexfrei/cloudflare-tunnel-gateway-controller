package proxy_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errConnectionRefused is the static-error sentinel returned by erroringRT
// in the propagates-inner-error test. err113 forbids defining one-off
// errors.New() values inside test bodies, so we hoist it to package scope.
var errConnectionRefused = errors.New("connection refused")

// blockingRT is a fake RoundTripper whose RoundTrip blocks until either
// ctx is cancelled or release is closed -- whichever happens first. It
// lets the headerTimeoutRoundTripper tests assert on cancellation
// behaviour without spinning up a real backend server.
//
// On ctx cancellation it returns ctx.Err() (mimicking what http2.Transport
// does when the underlying stream is cancelled mid-headers). On release
// it returns a 200 OK with body bound to the same ctx so body reads can
// be exercised across the timeout boundary.
type blockingRT struct {
	release   <-chan struct{}
	bodyDelay time.Duration
}

func (b *blockingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case <-req.Context().Done():
		return nil, fmt.Errorf("blockingRT ctx: %w", req.Context().Err())
	case <-b.release:
		// "headers arrived"
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       &delayedReader{ctx: req.Context(), delay: b.bodyDelay, payload: "ok"},
		Request:    req,
	}

	return resp, nil
}

// delayedReader emits payload after delay, simulating a streaming body
// that takes a while to produce bytes. Read returns ctx.Err() if the
// surrounding context fires before delay elapses.
type delayedReader struct {
	ctx context.Context //nolint:containedctx // test fake: ctx-on-struct is the simplest way to make Read honour cancellation

	delay   time.Duration
	payload string
	read    bool
}

func (d *delayedReader) Read(p []byte) (int, error) {
	if d.read {
		return 0, io.EOF
	}

	if d.delay > 0 {
		select {
		case <-d.ctx.Done():
			return 0, fmt.Errorf("delayedReader ctx: %w", d.ctx.Err())
		case <-time.After(d.delay):
		}
	}

	n := copy(p, d.payload)
	d.read = true

	return n, nil
}

func (d *delayedReader) Close() error { return nil }

func TestHeaderTimeoutRoundTripper_ZeroTimeoutPassesThrough(t *testing.T) {
	t.Parallel()

	released := make(chan struct{})
	close(released)

	inner := &blockingRT{release: released}
	wrapped := proxy.NewHeaderTimeoutRoundTripperForTest(inner, 0)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
	require.NoError(t, err)

	resp, err := wrapped.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestHeaderTimeoutRoundTripper_FiresWhenHeadersDoNotArriveInTime(t *testing.T) {
	t.Parallel()

	// release never fires -- inner.RoundTrip will block until ctx is cancelled.
	released := make(chan struct{})
	inner := &blockingRT{release: released}

	wrapped := proxy.NewHeaderTimeoutRoundTripperForTest(inner, 50*time.Millisecond)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
	require.NoError(t, err)

	start := time.Now()

	resp, err := wrapped.RoundTrip(req)
	elapsed := time.Since(start)

	require.Error(t, err, "header timeout must surface an error when backend stalls")

	if resp != nil {
		resp.Body.Close()
	}

	assert.ErrorIs(t, err, context.DeadlineExceeded,
		"header timeout must surface canonical DeadlineExceeded so errorHandler maps to 504 -- not raw context.Canceled, "+
			"which the handler treats as client-disconnect-no-response; got: %v", err)
	assert.Less(t, elapsed, 500*time.Millisecond,
		"timeout must fire near the 50ms deadline, not block until test timeout")
}

func TestHeaderTimeoutRoundTripper_BodyStreamSurvivesPastTimeout(t *testing.T) {
	t.Parallel()

	// Headers arrive immediately; body takes 200ms to produce. With a
	// 50ms header timeout, the body MUST still stream (the goroutine
	// that cancels on header-timeout must NOT fire once RoundTrip has
	// returned successfully).
	released := make(chan struct{})
	close(released)

	inner := &blockingRT{release: released, bodyDelay: 200 * time.Millisecond}
	wrapped := proxy.NewHeaderTimeoutRoundTripperForTest(inner, 50*time.Millisecond)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
	require.NoError(t, err)

	resp, err := wrapped.RoundTrip(req)
	require.NoError(t, err, "RoundTrip must succeed when headers arrive in time")
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "body read must complete even though the read takes longer than the header timeout")

	assert.Equal(t, "ok", string(body))
}

func TestHeaderTimeoutRoundTripper_PropagatesInnerError(t *testing.T) {
	t.Parallel()

	inner := &erroringRT{err: errConnectionRefused}
	wrapped := proxy.NewHeaderTimeoutRoundTripperForTest(inner, 100*time.Millisecond)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
	require.NoError(t, err)

	//nolint:bodyclose // erroringRT always returns nil resp; resp.Body cannot be dereferenced
	resp, err := wrapped.RoundTrip(req)
	require.Error(t, err)
	require.Nil(t, resp)
	assert.ErrorIs(t, err, errConnectionRefused, "inner errors must propagate verbatim through the wrapper")
}

type erroringRT struct{ err error }

func (e *erroringRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, e.err
}

// TestHeaderTimeoutRoundTripper_RespectsParentContextCancellation pins
// that a parent ctx cancellation (e.g. client disconnect) propagates
// regardless of the header timeout. Without correct context chaining
// the wrapper would shield the inner RoundTripper from the parent
// cancellation signal.
func TestHeaderTimeoutRoundTripper_RespectsParentContextCancellation(t *testing.T) {
	t.Parallel()

	released := make(chan struct{}) // never fires
	inner := &blockingRT{release: released}
	wrapped := proxy.NewHeaderTimeoutRoundTripperForTest(inner, 5*time.Second)

	parentCtx, cancel := context.WithCancel(t.Context())

	req, err := http.NewRequestWithContext(parentCtx, http.MethodGet, "http://example.com/", nil)
	require.NoError(t, err)

	// Cancel parent in 50ms; the wrapper's own 5s timeout would otherwise
	// dominate the test.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()

	resp, err := wrapped.RoundTrip(req)
	elapsed := time.Since(start)

	require.Error(t, err)

	if resp != nil {
		resp.Body.Close()
	}

	assert.Less(t, elapsed, 500*time.Millisecond,
		"parent ctx cancel must propagate -- wrapper must NOT swallow it under its own deadline")
	assert.True(t, errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled"),
		"error must surface the parent cancellation, got: %v", err)
}

// TestHeaderTimeoutRoundTripper_CancellationIsSingleShot pins that
// the per-request timer goroutine fires AT MOST once -- not on every
// body read, and not on RoundTrip retries. Catches a regression where
// the cancellation goroutine is re-spawned per inner call.
func TestHeaderTimeoutRoundTripper_CancellationIsSingleShot(t *testing.T) {
	t.Parallel()

	var cancelCount atomic.Int32

	inner := &countingRT{cancelCount: &cancelCount}
	wrapped := proxy.NewHeaderTimeoutRoundTripperForTest(inner, 30*time.Millisecond)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
	require.NoError(t, err)

	//nolint:bodyclose // countingRT always returns nil resp; resp.Body cannot be dereferenced
	_, err = wrapped.RoundTrip(req)
	require.Error(t, err)

	// Wait for the cancellation to settle, then verify it didn't fire
	// again. Eventually with a tight tick keeps the test from being
	// flake-prone on slow CI runners (vs a fixed time.Sleep that has
	// to over-allocate slack to be reliable).
	require.Eventually(t, func() bool {
		return cancelCount.Load() == 1
	}, 500*time.Millisecond, 5*time.Millisecond, "cancellation must register")

	// Hold the assertion for a second cycle so a delayed re-fire would
	// be caught (a re-spawned goroutine would bump the counter above 1).
	assert.Never(t, func() bool {
		return cancelCount.Load() > 1
	}, 200*time.Millisecond, 20*time.Millisecond,
		"cancellation must fire exactly once per RoundTrip call -- a re-spawned goroutine would push it above 1")
}

// countingRT increments cancelCount whenever its request ctx is
// cancelled. Single-use: blocks on ctx.Done() and returns the ctx
// error.
type countingRT struct{ cancelCount *atomic.Int32 }

func (c *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	c.cancelCount.Add(1)

	return nil, fmt.Errorf("countingRT ctx: %w", req.Context().Err())
}

// TestHeaderTimeoutRoundTripper_CloseIdleConnectionsDelegates pins the
// PruneTransports contract: the cached transport pool evicts entries
// via an `interface{ CloseIdleConnections() }` assertion. Without the
// wrapper's own CloseIdleConnections method, the inner *http2.Transport
// would never get its idle conns reaped when the operator flips a
// per-rule timeout (transportKey re-keys the pool entry, old wrapper
// is evicted, h2 multiplexed conn lingers until OS keepalive).
func TestHeaderTimeoutRoundTripper_CloseIdleConnectionsDelegates(t *testing.T) {
	t.Parallel()

	var closed atomic.Int32

	inner := &closableRT{closed: &closed}
	wrapped := proxy.NewHeaderTimeoutRoundTripperForTest(inner, 100*time.Millisecond)

	closer, ok := wrapped.(interface{ CloseIdleConnections() })
	require.True(t, ok, "wrapper must satisfy CloseIdleConnections so PruneTransports can evict the underlying transport")

	closer.CloseIdleConnections()

	assert.Equal(t, int32(1), closed.Load(),
		"wrapper must delegate CloseIdleConnections to inner -- otherwise the h2 multiplexed conn leaks on pool eviction")
}

// closableRT records each CloseIdleConnections call on an atomic
// counter; RoundTrip is a no-op (returns an error) since the eviction
// test never drives a request.
type closableRT struct{ closed *atomic.Int32 }

func (c *closableRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errConnectionRefused
}

func (c *closableRT) CloseIdleConnections() {
	c.closed.Add(1)
}

// TestHeaderTimeoutRoundTripper_PanicInInnerDoesNotLeak pins the
// goroutine + context cleanup contract for the case where the inner
// RoundTripper panics. Without the panic-safety defer, the wrapper's
// derived WithCancel ctx would leak (waiting for parent ctx) and
// the AfterFunc timer would keep its callback live until h.timeout
// fired (multiple seconds of operator-configurable delay).
//
// Test mechanism: panickingRT spawns a goroutine that watches its
// request ctx for Done(), then panics. The watcher closing the
// ctxDone channel is the deterministic signal that the wrapper's
// panic-safety defer cancelled the derived ctx. No runtime.NumGoroutine
// involved -- the parallel test runner makes that count fluctuate
// unpredictably and the assertion would flake on shared CI runners.
func TestHeaderTimeoutRoundTripper_PanicInInnerDoesNotLeak(t *testing.T) {
	t.Parallel()

	ctxCancelled := make(chan struct{})
	inner := &panickingRT{ctxCancelled: ctxCancelled}
	wrapped := proxy.NewHeaderTimeoutRoundTripperForTest(inner, 10*time.Second)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
	require.NoError(t, err)

	assert.Panics(t, func() {
		//nolint:bodyclose // panickingRT never returns; assert.Panics catches the panic
		_, _ = wrapped.RoundTrip(req)
	}, "panic from inner must propagate to the caller -- the wrapper must not swallow it")

	// After the panic, the wrapper's panic-safety defer must cancel
	// the derived ctx so the panickingRT's watcher goroutine sees
	// Done() and closes ctxCancelled. A missing defer leaves the
	// ctx waiting for the 10s wrapper timeout (or the request ctx,
	// which the test never cancels), well past this assertion's
	// 500ms window.
	select {
	case <-ctxCancelled:
		// Pass: wrapper cancelled the derived ctx after panic.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("wrapper did not cancel the derived ctx within 500ms after panic; " +
			"panic-safety defer missing or broken (timer's AfterFunc / WithCancel watcher would leak)")
	}
}

// panickingRT spawns a ctx-watcher goroutine (closes ctxCancelled
// when the request ctx fires) and then panics in RoundTrip. The
// goroutine starts BEFORE the panic so the cancel observation is
// deterministic regardless of scheduler ordering.
type panickingRT struct {
	ctxCancelled chan struct{}
}

func (p *panickingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	go func() {
		<-req.Context().Done()
		close(p.ctxCancelled)
	}()

	// Give the watcher a moment to register before panicking. Without
	// this the panic can unwind faster than the goroutine starts and
	// the test races on the observation -- but Go's goroutine spawn
	// is fast enough that runtime.Gosched is sufficient.
	runtime.Gosched()

	panic("simulated transport panic")
}

// TestHeaderTimeoutRoundTripper_LateTimerSuccessIsTreatedAsTimeout
// pins the round-2 race-condition fix: if inner.RoundTrip returns
// nil (success) at T = timeout - 1ms but the wrapper goroutine is
// descheduled until T = timeout + 1ms, the timer fires concurrently
// with the success return. Without the timer-disposition wait, the
// success path would hand the caller a response body bound to a
// now-cancelled context (first body read would surface "context
// canceled" mid-stream).
//
// Mechanism: 1000 iterations of "inner returns success right around
// the deadline" -- if the success-but-timer-fired branch ever leaks
// a half-cancelled response through, ReadAll on that body fails
// with a non-EOF error and the assertion catches it.
func TestHeaderTimeoutRoundTripper_LateTimerSuccessIsTreatedAsTimeout(t *testing.T) {
	t.Parallel()

	const iterations = 1000

	const timeout = 5 * time.Millisecond

	for range iterations {
		released := make(chan struct{})

		// Release at exactly h.timeout to maximise the chance of
		// landing in the success-vs-late-timer race window.
		go func() {
			time.Sleep(timeout)
			close(released)
		}()

		inner := &blockingRT{release: released}
		wrapped := proxy.NewHeaderTimeoutRoundTripperForTest(inner, timeout)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
		require.NoError(t, err)

		resp, err := wrapped.RoundTrip(req)
		// Two outcomes are acceptable:
		//   (a) err is non-nil -- timer fired before inner returned,
		//       wrapper surfaced DeadlineExceeded.
		//   (b) err is nil AND resp.Body reads to EOF without a
		//       cancellation error -- inner won the race AND the
		//       wrapper correctly observed timer did NOT fire.
		// The forbidden outcome is "err nil + body read returns
		// context.Canceled" -- that's the half-cancelled-response leak.
		if err != nil {
			require.ErrorIs(t, err, context.DeadlineExceeded, "non-nil err must be the canonical header-timeout error")

			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, readErr,
			"success-path response body must read cleanly -- a context.Canceled here is the half-cancelled "+
				"response leak the late-timer-disposition wait is supposed to catch")
		require.Equal(t, "ok", string(body))
	}
}
