package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
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

// errBodyLength rejects a body whose length differs from its declared
// Content-Length. The transport would fail every attempt on it, so no attempt
// is made.
var errBodyLength = errors.New("request body length does not match its Content-Length")

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
	metrics        *Metrics
	hostname       string
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

	parentTrace := httptrace.ContextClientTrace(req.Context())
	maxAttempts := min(rt.policy.Attempts, MaxRetryAttempts)

	for attempt := 0; ; attempt++ {
		attemptCtx, held := ctx, (*heldInformational)(nil)
		// The last allowed attempt answers whatever it gets, so its 1xx
		// responses can go out as they arrive.
		if attempt < maxAttempts {
			attemptCtx, held = holdInformational(ctx, parentTrace)
		}

		out := req.WithContext(attemptCtx)
		out.Body = body
		// Replays come only from the recorder, so the transport must not
		// rewind a body on its own as it could with a client-built request.
		out.GetBody = nil

		resp, err := rt.next.RoundTrip(out)
		if attempt >= maxAttempts || !rt.shouldRetry(resp, err) {
			return finishAttempt(resp, err, budgetSpent, cancel, held)
		}

		if recorder != nil {
			next, ok := recorder.next(ctx)
			if !ok {
				return finishAttempt(resp, err, budgetSpent, cancel, held)
			}

			body = next
		}

		reason := retryReason(err)

		discardResponse(resp)

		err = sleepCtx(ctx, max(rt.policy.Backoff, MinRetryBackoff))
		if err != nil {
			return finishAttempt(nil, err, budgetSpent, cancel, nil)
		}

		rt.metrics.backendRetried(rt.hostname, reason)
	}
}

// finishAttempt settles the final attempt. A spent request budget overrides
// whatever the attempt returned; otherwise the attempt's held 1xx responses
// are forwarded and the response is handed on with the attempt context tied
// to its body.
func finishAttempt(resp *http.Response, err error, budgetSpent func() bool, cancel context.CancelCauseFunc, held *heldInformational) (*http.Response, error) {
	if budgetSpent() {
		cancel(errRequestBudget)
		discardResponse(resp)

		return nil, errRequestBudget
	}

	if err == nil {
		err = held.forward()
		if err != nil {
			discardResponse(resp)
		}
	}

	if err != nil {
		cancel(nil)

		return nil, fmt.Errorf("backend round trip: %w", err)
	}

	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}

	return resp, nil
}

// heldInformational keeps an attempt's 1xx responses until the attempt is
// known to answer the request. ReverseProxy forwards them through a trace hook
// as they arrive, which would let a retried attempt's 103 reach the client.
type heldInformational struct {
	parent *httptrace.ClientTrace

	mu        sync.Mutex
	responses []informationalResponse
}

type informationalResponse struct {
	code   int
	header textproto.MIMEHeader
}

// holdInformational returns a context for one attempt whose trace holds 1xx
// responses instead of passing them to parent's hook. held is nil when there
// is no hook to hold for.
func holdInformational(ctx context.Context, parent *httptrace.ClientTrace) (context.Context, *heldInformational) {
	if parent == nil || parent.Got1xxResponse == nil {
		return ctx, nil
	}

	held := &heldInformational{parent: parent}
	replacement := *parent
	replacement.Got1xxResponse = held.hold

	return traceOverride{Context: ctx, parent: parent, replacement: &replacement}, held
}

func (h *heldInformational) hold(code int, header textproto.MIMEHeader) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.responses = append(h.responses, informationalResponse{code: code, header: header})

	return nil
}

// forward passes the held responses to the parent hook. An abandoned
// attempt's are never forwarded. Nil-safe.
func (h *heldInformational) forward() error {
	if h == nil {
		return nil
	}

	h.mu.Lock()
	responses := h.responses
	h.mu.Unlock()

	for _, resp := range responses {
		err := h.parent.Got1xxResponse(resp.code, resp.header)
		if err != nil {
			return fmt.Errorf("forwarding %d response: %w", resp.code, err)
		}
	}

	return nil
}

// traceOverride swaps one ClientTrace for another in a context. httptrace
// can only add a trace on top of an existing one, and the existing hooks then
// still run.
type traceOverride struct {
	context.Context //nolint:containedctx // a derived context wraps its parent, as the stdlib ones do; only Value is overridden

	parent      *httptrace.ClientTrace
	replacement *httptrace.ClientTrace
}

func (c traceOverride) Value(key any) any {
	value := c.Context.Value(key)
	if trace, ok := value.(*httptrace.ClientTrace); ok && trace == c.parent {
		return c.replacement
	}

	return value
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

// retryReason labels a retried attempt: "status" for a listed status code,
// otherwise the backend error reason.
func retryReason(err error) string {
	if err == nil {
		return "status"
	}

	return classifyBackendError(err)
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
	// idle is closed when reading drops to zero, for a hand-off waiting on it.
	idle chan struct{}
	// probed is set for a method the HTTP/1 transport probes for a body.
	probed bool
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
		// The extra byte detects a body that overruns its declared length.
		buf, err := io.ReadAll(io.LimitReader(req.Body, req.ContentLength+1))
		if err != nil {
			return nil, nil, fmt.Errorf("reading request body: %w", err)
		}

		if int64(len(buf)) != req.ContentLength {
			return nil, nil, fmt.Errorf("%w (%d declared)", errBodyLength, req.ContentLength)
		}

		recorder := &replayBody{recorded: buf, eof: true}

		return recorder, io.NopCloser(bytes.NewReader(buf)), nil
	}

	recorder := &replayBody{src: req.Body, probed: bodyProbed(req.Method)}
	first := recorder.attempt()

	return recorder, first, nil
}

// bodyProbed reports whether the HTTP/1 transport probes a request of this
// method whose body length is unknown, mirroring net/http's
// requestMethodUsuallyLacksBody.
func bodyProbed(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodOptions, "PROPFIND", "SEARCH":
		return true
	}

	return false
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
func (b *replayBody) next(ctx context.Context) (io.ReadCloser, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// A read in progress on a source nothing has been read from may still end
	// empty: an HTTP/1 transport probing a bodyless-method request leaves one
	// behind when the connection fails. Its outcome decides the hand-off, so
	// wait for it, bounded by the request. Any other request is refused at
	// once: its backend may have answered without reading the body, and a
	// full-duplex client may send nothing until it sees that answer.
	for b.probed && b.reading > 0 && !b.consumed && !b.overflow {
		if b.idle == nil {
			b.idle = make(chan struct{})
		}

		idle := b.idle

		b.mu.Unlock()

		select {
		case <-ctx.Done():
			b.mu.Lock()

			return nil, false
		case <-idle:
		}

		b.mu.Lock()
	}

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
	if b.reading == 0 && b.idle != nil {
		close(b.idle)
		b.idle = nil
	}

	// A failed read leaves the source in an unknown state, so it is not
	// handed on.
	b.consumed = b.consumed || n > 0 || (err != nil && !errors.Is(err, io.EOF))
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
