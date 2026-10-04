package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"
)

// maxRetryBodyBytes bounds the request body recorded for replay. A larger
// body is not resent once the transport has started reading it.
const maxRetryBodyBytes = 64 << 10

// errAttemptAbandoned is what an abandoned attempt reads from the request
// body once a later attempt owns it.
var errAttemptAbandoned = errors.New("request body handed to a later retry attempt")

// errRequestBudget is returned when timeouts.request expires across retry
// attempts. It wraps context.DeadlineExceeded so errorHandler answers 504.
var errRequestBudget = fmt.Errorf("request timeout reached during retries: %w", context.DeadlineExceeded)

// retryTransport retries a backend round trip per a rule's RouteRetry. It
// also owns the rule's request timeout: the budget covers every attempt and
// backoff, and stops once the final response headers arrive so a streaming
// body is not cut.
type retryTransport struct {
	next           http.RoundTripper
	policy         *RouteRetry
	requestTimeout time.Duration
}

func (rt *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder, body, err := newReplayBody(req)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancelCause(req.Context())
	budgetSpent := func() bool { return false }

	if rt.requestTimeout > 0 {
		timer := time.AfterFunc(rt.requestTimeout, func() { cancel(errRequestBudget) })
		// Stop reports false once the timer has fired, and keeps doing so.
		budgetSpent = func() bool { return !timer.Stop() }
	}

	for attempt := 0; ; attempt++ {
		out := req.WithContext(ctx)
		out.Body = body
		// Replays come only from the recorder, so the transport must not
		// rewind a body on its own as it could with a client-built request.
		out.GetBody = nil

		resp, err := rt.next.RoundTrip(out)
		if attempt >= min(rt.policy.Attempts, MaxRetryAttempts) || !rt.shouldRetry(resp, err) {
			return finishAttempt(resp, err, budgetSpent, cancel)
		}

		if recorder != nil {
			next, ok := recorder.next()
			if !ok {
				return finishAttempt(resp, err, budgetSpent, cancel)
			}

			body = next
		}

		discardResponse(resp)

		err = sleepCtx(ctx, max(rt.policy.Backoff, MinRetryBackoff))
		if err != nil {
			return finishAttempt(nil, err, budgetSpent, cancel)
		}
	}
}

// finishAttempt settles the final attempt. A spent request budget overrides
// whatever the attempt returned; otherwise the response is handed on with the
// attempt context tied to its body.
func finishAttempt(resp *http.Response, err error, budgetSpent func() bool, cancel context.CancelCauseFunc) (*http.Response, error) {
	if budgetSpent() {
		cancel(errRequestBudget)
		discardResponse(resp)

		return nil, errRequestBudget
	}

	if err != nil {
		cancel(nil)

		return nil, fmt.Errorf("backend round trip: %w", err)
	}

	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}

	return resp, nil
}

// shouldRetry reports whether an attempt's outcome warrants another one: any
// transport error, or a listed status code. A client that went away also
// surfaces as an error, but its cancelled context ends the backoff wait.
func (rt *retryTransport) shouldRetry(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}

	return slices.Contains(rt.policy.Codes, resp.StatusCode)
}

// replayBody records a request body as an attempt sends it, so a retry can
// resend it without holding up the first attempt: a streaming upload reaches
// the backend as it arrives. Only the attempt that currently owns the body
// reads the source, because a transport may keep reading an abandoned
// attempt's body after its RoundTrip has returned.
type replayBody struct {
	src io.ReadCloser

	mu       sync.Mutex
	owner    *attemptBody
	recorded []byte
	overflow bool
	eof      bool
	consumed bool
	reading  int
}

// newReplayBody prepares req's body for retries and returns the body for the
// first attempt. A body whose declared length fits maxRetryBodyBytes is read
// up front, so a connection that fails mid-upload can still be retried; any
// other body is recorded as the first attempt sends it. recorder is nil when
// there is no body.
func newReplayBody(req *http.Request) (*replayBody, io.ReadCloser, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, req.Body, nil
	}

	if req.ContentLength > 0 && req.ContentLength <= maxRetryBodyBytes {
		// The extra byte lets the transport reject a body that overruns its
		// declared length, as it would have without buffering.
		buf, err := io.ReadAll(io.LimitReader(req.Body, req.ContentLength+1))
		if err != nil {
			return nil, nil, fmt.Errorf("reading request body: %w", err)
		}

		recorder := &replayBody{recorded: buf, eof: true}

		return recorder, io.NopCloser(bytes.NewReader(buf)), nil
	}

	recorder := &replayBody{src: req.Body}
	first := recorder.attempt()

	return recorder, first, nil
}

// attempt hands the source to a new attempt. It writes owner without taking
// mu: callers either hold it or run before the body is shared.
func (b *replayBody) attempt() *attemptBody {
	owner := &attemptBody{body: b}
	b.owner = owner

	return owner
}

// next returns the body for the following attempt and whether there is one.
// A body read to its end within maxRetryBodyBytes is replayed from the
// recording; a source nobody has read from yet is handed on as is; anything
// else, an upload the backend stopped reading part-way included, cannot be
// resent.
func (b *replayBody) next() (io.ReadCloser, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// The owner changes only when a body is handed on: a refused hand-off
	// leaves the attempt that is still sending free to finish.
	switch {
	case b.reading > 0 || b.overflow:
		return nil, false
	case b.eof:
		b.owner = nil

		return io.NopCloser(bytes.NewReader(b.recorded)), true
	case !b.consumed:
		return b.attempt(), true
	default:
		return nil, false
	}
}

// attemptBody is one attempt's view of a replayBody. Close is a no-op: the
// source stays open for a later attempt, and the server closes it when the
// request ends.
type attemptBody struct {
	body *replayBody
}

func (a *attemptBody) Read(buf []byte) (int, error) {
	b := a.body

	b.mu.Lock()
	if b.owner != a {
		b.mu.Unlock()

		return 0, errAttemptAbandoned
	}

	b.reading++
	b.mu.Unlock()

	n, err := b.src.Read(buf)

	b.mu.Lock()
	defer b.mu.Unlock()

	b.reading--
	b.consumed = b.consumed || n > 0
	b.eof = b.eof || errors.Is(err, io.EOF)

	if !b.overflow && len(b.recorded)+n > maxRetryBodyBytes {
		b.overflow = true
		b.recorded = nil
	}

	if !b.overflow {
		b.recorded = append(b.recorded, buf[:n]...)
	}

	return n, err //nolint:wrapcheck // io.EOF and friends must pass through by identity
}

func (a *attemptBody) Close() error {
	return nil
}

func discardResponse(resp *http.Response) {
	if resp == nil {
		return
	}

	// Closed unread: draining would let a backend that stalls mid-body hold
	// up the retry, and a failed attempt's keep-alive is not worth that.
	_ = resp.Body.Close()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return fmt.Errorf("retry backoff: %w", context.Cause(ctx))
	case <-timer.C:
		return nil
	}
}

// cancelOnClose releases the attempt context once the response body is done.
type cancelOnClose struct {
	io.ReadCloser

	cancel context.CancelCauseFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel(nil)

	if err != nil {
		return fmt.Errorf("closing response body: %w", err)
	}

	return nil
}

func ruleRequestTimeout(rule *RouteRule) time.Duration {
	if rule.Timeouts == nil {
		return 0
	}

	return rule.Timeouts.Request
}
