package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// headerTimeoutRoundTripper wraps an http.RoundTripper with a
// per-call header-only deadline. It exists so the h2c backend path can
// honour the same "bound time-to-first-response-byte but stream the
// body freely" contract that *http.Transport.ResponseHeaderTimeout
// gives the HTTP/1.1 and TLS paths -- golang.org/x/net/http2.Transport
// has no ResponseHeaderTimeout-equivalent knob, so we synthesize one.
//
// Streaming-response contract (mirrors the HTTP/1.1 contract pinned by
// runStreamingTimeoutTest in handler_test.go):
//
//   - timeout bounds the time the wrapped RoundTrip takes to return,
//     i.e. how long it waits for response headers from the backend.
//   - once RoundTrip returns successfully, the deadline is cleared --
//     subsequent body reads are NOT bounded by timeout. SSE / chunked /
//     gRPC server-streaming bodies stream for as long as the backend
//     keeps producing bytes.
//
// Mechanism: time.AfterFunc starts a timer that fires the
// cancel-the-wrapped-ctx callback after h.timeout. Before consulting
// the inner result we always wait for the timer's eventual disposition
// (callback finished, or Stop() raced with us). That synchronisation
// closes the race between "inner.RoundTrip returned success at
// T = timeout - 1ms" and "timer goroutine ran at T = timeout, set
// timerFired, and cancelled ctx" -- without it the success path could
// hand the caller a response body bound to a cancelled context, which
// would surface as "context canceled" mid-stream on the first body
// read (relocating exactly the truncation bug this PR fixes from "all
// slow h2c" to "h2c whose first-byte latency lands near the
// deadline").
//
// Parent ctx cancellation (e.g. client disconnect) flows through the
// derived context.WithCancel chain unchanged -- it cancels both the
// wrapped RoundTrip and the body stream, regardless of the header
// timeout's state.
type headerTimeoutRoundTripper struct {
	inner   http.RoundTripper
	timeout time.Duration
}

// newHeaderTimeoutRoundTripper wraps inner with a header-only deadline
// of timeout. Zero timeout returns inner unchanged so callers don't
// pay the wrapper's allocation or goroutine cost when the knob is
// unset.
func newHeaderTimeoutRoundTripper(inner http.RoundTripper, timeout time.Duration) http.RoundTripper {
	if timeout <= 0 {
		return inner
	}

	return &headerTimeoutRoundTripper{inner: inner, timeout: timeout}
}

// RoundTrip implements http.RoundTripper. See the type comment for
// the full streaming-response contract.
func (h *headerTimeoutRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Derive a cancellable child of the request context. The timer
	// callback cancels this child if the header timeout fires before
	// RoundTrip returns; the body wrapper cancels it on Close().
	// Parent ctx cancellation flows through naturally because
	// WithCancel chains.
	ctx, cancel := context.WithCancel(req.Context())

	// timerFired distinguishes "inner returned an error because of OUR
	// header-timeout cancellation" from "inner returned an error for
	// any other reason" (connect refused, protocol error, parent ctx
	// cancellation). When timerFired is true on the failure path we
	// surface context.DeadlineExceeded so errorHandler maps it to 504
	// (mirroring how *http.Transport.ResponseHeaderTimeout surfaces on
	// the H/1.1 path); otherwise the inner error propagates unchanged.
	var timerFired atomic.Bool

	// timerDone signals the moment the AfterFunc callback has run to
	// completion (timerFired has been written + cancel has been
	// called). It's the synchronisation hook the post-RoundTrip code
	// uses to settle the success-vs-late-timer race.
	timerDone := make(chan struct{})
	settleOnce := sync.Once{}
	settle := func() { close(timerDone) }

	timer := time.AfterFunc(h.timeout, func() {
		timerFired.Store(true)
		cancel()
		settleOnce.Do(settle)
	})

	// Panic safety: if the inner panics we must still stop the timer,
	// release the ctx, and ensure timerDone closes (otherwise a future
	// `<-timerDone` from a sibling code path would block forever).
	// transferred flips to true only after we've successfully wrapped
	// the body and confirmed the timer DIDN'T fire concurrently --
	// that's the single point where the body wrapper assumes
	// responsibility for cancel().
	var transferred bool

	defer func() {
		if !transferred {
			cancel()
		}

		// Stop the timer and force timerDone to close so any waiter
		// (here or downstream) is unblocked even on the panic path.
		// timer.Stop() returns true if it stopped the timer before it
		// fired; false means the callback either already ran or is
		// running. In both cases settleOnce ensures timerDone closes
		// exactly once.
		if timer.Stop() {
			settleOnce.Do(settle)
		}
	}()

	resp, err := h.inner.RoundTrip(req.Clone(ctx))

	// Settle the timer's disposition BEFORE consulting err / timerFired.
	// Two possibilities for the timer:
	//   (a) Stop() returns true: the callback never fired. Close
	//       timerDone ourselves; timerFired is guaranteed false.
	//   (b) Stop() returns false: the callback already fired OR is
	//       executing right now. Wait for timerDone so timerFired and
	//       cancel() have visibly completed before we read them.
	if timer.Stop() {
		settleOnce.Do(settle)
	}

	<-timerDone

	// Late-timer-but-success race: inner returned nil err, but the
	// timer fired before we could observe its disposition. The
	// response body is bound to the now-cancelled ctx, so first read
	// would surface "context canceled" mid-stream -- discard the
	// response and surface as a clean header timeout instead.
	if err == nil && timerFired.Load() {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("h2c header timeout after %s: %w", h.timeout, context.DeadlineExceeded)
	}

	if err != nil {
		if timerFired.Load() {
			return nil, fmt.Errorf("h2c header timeout after %s: %w", h.timeout, context.DeadlineExceeded)
		}

		return nil, fmt.Errorf("h2c header-timeout transport: %w", err)
	}

	// Headers arrived and the timer did NOT fire concurrently. Hand
	// cancel() to the body wrapper so the stream can run past
	// h.timeout without being torn down -- the panic-safety defer
	// above now sees transferred=true and skips its cancel().
	resp.Body = &headerTimeoutBody{ReadCloser: resp.Body, cancel: cancel}
	transferred = true

	return resp, nil
}

// CloseIdleConnections delegates to the inner RoundTripper. PruneTransports
// in handler.go uses an interface assertion to evict idle connections from
// transports that fall out of the pool (e.g. when an operator flips a
// per-rule timeout and transportKey produces a fresh entry). Without this
// method the assertion misses the wrapped path entirely -- the wrapper is
// removed from the pool but the underlying *http2.Transport's multiplexed
// conn never gets told to close idle conns, leaking until OS keepalive
// reaps it.
func (h *headerTimeoutRoundTripper) CloseIdleConnections() {
	closer, ok := h.inner.(interface{ CloseIdleConnections() })
	if ok {
		closer.CloseIdleConnections()
	}
}

// headerTimeoutBody wraps the response body so closing it cancels the
// per-request context the wrapper created. Without this hook the
// context would only be reclaimed when the parent ctx fires, which on
// long-lived streams could mean the context lives effectively
// forever and the wrapper leaks one goroutine-context per request.
type headerTimeoutBody struct {
	io.ReadCloser

	cancel context.CancelFunc
}

// Close releases the wrapped body and the per-request context.
func (b *headerTimeoutBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()

	if err != nil {
		return fmt.Errorf("h2c header-timeout transport body close: %w", err)
	}

	return nil
}
