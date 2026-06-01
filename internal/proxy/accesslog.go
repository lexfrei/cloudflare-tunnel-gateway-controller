package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// errHijackNotSupported is returned by countingResponseWriter.Hijack
// when the wrapped writer does not implement http.Hijacker. The
// WebSocket upgrade path uses Hijack; surfacing a typed error keeps
// that fallback discoverable instead of panicking.
var errHijackNotSupported = errors.New("response writer does not support hijacking")

// accessLogMsg is the slog "msg" field for emitted access lines.
// Hoisted to a constant so downstream log filters / dashboards keyed
// on `msg:"access"` survive a typo in the call site, and so the
// string is searchable from one place when rename inevitably happens.
const accessLogMsg = "access"

// countingResponseWriter wraps an http.ResponseWriter and records the
// final status code + cumulative bytes written so the access-log
// emission at the end of ServeHTTP has the data without re-reading the
// underlying conn or growing the request context.
//
// Implements http.Flusher and http.Hijacker by delegating to the inner
// writer when it supports them (cloudflared's HTTP/2 response writer
// supports Flush; the H/1.x path supports Hijack for WebSocket). The
// type assertion + delegation pattern keeps the wrapper transparent
// to handlers that detect those interfaces.
type countingResponseWriter struct {
	http.ResponseWriter

	bytesWritten      atomic.Int64
	status            atomic.Int32
	writeHeaderCalled atomic.Bool
}

// newCountingResponseWriter wraps inner. Zero status / zero bytes are
// the "no response sent" sentinel until WriteHeader / Write is called.
func newCountingResponseWriter(inner http.ResponseWriter) *countingResponseWriter {
	return &countingResponseWriter{ResponseWriter: inner}
}

// WriteHeader records the status and forwards to the inner writer.
// Mirrors stdlib's first-wins contract: subsequent calls are no-ops
// (stdlib logs a warning but still passes them through; we silently
// ignore because the wrapper exists to record what the client saw).
func (c *countingResponseWriter) WriteHeader(status int) {
	if c.writeHeaderCalled.CompareAndSwap(false, true) {
		c.status.Store(int32(status)) //nolint:gosec // HTTP status fits in int32 with room to spare
		c.ResponseWriter.WriteHeader(status)
	}
}

// Write records the byte count and forwards. Implicit-200 contract:
// the first Write without a prior WriteHeader defaults the recorded
// status to 200 (matching stdlib semantics).
func (c *countingResponseWriter) Write(p []byte) (int, error) {
	if c.writeHeaderCalled.CompareAndSwap(false, true) {
		c.status.Store(http.StatusOK)
	}

	n, err := c.ResponseWriter.Write(p)
	c.bytesWritten.Add(int64(n))

	if err != nil {
		return n, fmt.Errorf("counting response writer: %w", err)
	}

	return n, nil
}

// Status returns the recorded status code, or 0 if neither WriteHeader
// nor Write has been called yet. The zero return distinguishes
// "handler emitted no response" from "handler used the implicit 200"
// (the latter shows up as 200 once Write has run).
func (c *countingResponseWriter) Status() int {
	return int(c.status.Load())
}

// BytesWritten returns the cumulative byte count from all Write calls.
func (c *countingResponseWriter) BytesWritten() int64 {
	return c.bytesWritten.Load()
}

// Flush delegates to the inner writer when it implements http.Flusher.
// Without this delegation, streaming responses (SSE / chunked /
// gRPC server-streaming) would silently lose per-frame flushes when
// the wrapper is in the call chain.
func (c *countingResponseWriter) Flush() {
	if flusher, ok := c.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack delegates to the inner writer when it implements
// http.Hijacker (the WebSocket upgrade path requires it). Returns
// errHijackNotSupported when the inner does not -- WebSocket flows
// won't run on writers that don't support Hijack regardless, so the
// fallback is the discoverability boost.
func (c *countingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := c.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errHijackNotSupported
	}

	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, fmt.Errorf("counting response writer hijack: %w", err)
	}

	return conn, rw, nil
}

// ReadFrom delegates to the inner writer when it implements
// io.ReaderFrom (which is how httputil.ReverseProxy hits the
// splice/sendfile fast path on Linux for large bodies). When the
// inner does not, falls back to io.Copy onto the inner ResponseWriter
// (the `struct{ io.Writer }` wrapper prevents the Writer-only
// adapter from re-detecting our own ReadFrom and recursing) and
// increments bytesWritten explicitly.
//
// Both paths atomically increment bytesWritten so the access-log
// emission reads the full response size whether the fast path was
// taken or the fallback ran. Without this method,
// httputil.ReverseProxy's ServeHTTP would type-assert
// io.ReaderFrom on the wrapper, fail, and fall back to the generic
// 32 KiB-buffer io.Copy loop -- correct, but it costs the
// sendfile/splice acceleration that's worth a measurable percentage
// on multi-MiB responses through Linux.
func (c *countingResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	// Mirror the implicit-200 contract Write enforces: a caller that
	// drives ReadFrom without an explicit WriteHeader must end up with
	// Status()==200, not Status()==0. Without this, an access-log
	// line for a ReadFrom-only path would render `status:0` and the
	// 5xx sampling carve-out (which gates on `>= 500`) would
	// misclassify the request.
	if c.writeHeaderCalled.CompareAndSwap(false, true) {
		c.status.Store(http.StatusOK)
	}

	readerFrom, ok := c.ResponseWriter.(io.ReaderFrom)
	if !ok {
		n, err := io.Copy(struct{ io.Writer }{c.ResponseWriter}, src)
		c.bytesWritten.Add(n)

		if err != nil {
			return n, fmt.Errorf("counting response writer fallback copy: %w", err)
		}

		return n, nil
	}

	n, err := readerFrom.ReadFrom(src)
	c.bytesWritten.Add(n)

	if err != nil {
		return n, fmt.Errorf("counting response writer ReadFrom: %w", err)
	}

	return n, nil
}

// accessLogSnapshot captures the request fields the access log
// emits, taken BEFORE filters run. URL rewrite filters mutate
// req.URL.Path in place (filter.go writeRewritePathFilter), so a
// deferred closure that re-reads req at emission time would log
// the rewritten path and lose the client-observable original.
// Snapshotting in ServeHTTP keeps the log faithful to what the
// client actually sent.
type accessLogSnapshot struct {
	method    string
	host      string
	path      string
	query     string
	userAgent string
}

// newAccessLogSnapshot captures the pre-filter request fields and the start
// time for the deferred access-log emission. stripQuery zeroes the query field
// for operators whose URLs carry tokens / PII.
func newAccessLogSnapshot(req *http.Request, stripQuery bool) (*accessLogSnapshot, time.Time) {
	query := req.URL.RawQuery
	if stripQuery {
		query = ""
	}

	return &accessLogSnapshot{
		method:    req.Method,
		host:      req.Host,
		path:      req.URL.Path,
		query:     query,
		userAgent: req.UserAgent(),
	}, time.Now()
}

// maybeEmitAccessLog is the deferred tail of ServeHTTP when access
// logging is enabled. Consults shouldSampleAccessLog with the
// handler's configured rate, the recorded status, and the injected
// rand source; emits a structured INFO-level slog line on the
// configured logger if sampling lets the request through. Skips the
// emission entirely when accessLog is nil (defence-in-depth -- the
// caller already gates on this).
//
// Fields: method, host, path, query, user_agent (from the pre-filter
// snapshot; URL rewrites do not alter what gets logged), plus status,
// bytes_written, duration_ms (response outcome).
//
// Route binding (matched hostname, backend URL) is intentionally not
// added here because router.Route runs inside ServeHTTP and the
// result isn't visible to this defer; the per-request context is
// rich enough for triage. A follow-up can add route_id once the
// router exposes a stable identifier.
func (h *Handler) maybeEmitAccessLog(counted *countingResponseWriter, req *http.Request, snapshot *accessLogSnapshot, start time.Time) {
	if h.accessLog == nil {
		return
	}

	status := counted.Status()
	if status == 0 {
		// Handler returned without writing a response. Rare; happens
		// for early-aborted requests. Skip -- nothing useful to log.
		return
	}

	// WebSocket upgrades go through pipeWebSocket which calls
	// WriteHeader(101) BEFORE Hijack(), so the wrapper records
	// status=101 and this defer would otherwise fire after the
	// pipeWebSocket goroutines exit -- i.e. with duration_ms equal
	// to the entire WS session lifetime and bytes_written=0 (post-
	// Hijack bytes bypass the wrapper). That log line is useless
	// for triage (the session-length signal misleads, the
	// byte-count is wrong) and noisy on long-lived WS routes. The
	// WS upgrade path emits its own diagnostics; skip here.
	if status == http.StatusSwitchingProtocols {
		return
	}

	if !shouldSampleAccessLog(h.accessLogSamplingRate, status, h.accessLogRandFn) {
		return
	}

	h.accessLog.LogAttrs(req.Context(), slog.LevelInfo, accessLogMsg,
		slog.String("method", snapshot.method),
		slog.String("host", snapshot.host),
		slog.String("path", snapshot.path),
		slog.String("query", snapshot.query),
		slog.Int("status", status),
		slog.Int64("bytes_written", counted.BytesWritten()),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		slog.String("user_agent", snapshot.userAgent),
	)
}

// shouldSampleAccessLog decides whether a given response should emit
// an access-log line based on the configured sampling rate and the
// observed status code.
//
// Contract:
//   - rate is clamped into [0, 1] -- operator typos like
//     `samplingRate: 50` (percent instead of fraction) clamp to 1
//     so the symptom is "always logged" instead of "silently never
//     logged"; negative rates clamp to 0.
//   - Status >= 500 is ALWAYS logged regardless of sample roll: a
//     5xx is by definition a server-side failure the operator needs
//     to see, and dropping it to keep sample rate low would hide
//     the most important diagnostic signal.
//   - Otherwise: log iff randFn() < rate. randFn is injected so
//     unit tests can drive deterministic samples; in production
//     callers pass math/rand/v2.Float64.
func shouldSampleAccessLog(rate float64, status int, randFn func() float64) bool {
	if status >= http.StatusInternalServerError {
		return true
	}

	clamped := rate
	if clamped < 0 {
		clamped = 0
	}

	if clamped > 1 {
		clamped = 1
	}

	if clamped == 0 {
		return false
	}

	if clamped >= 1 {
		return true
	}

	return randFn() < clamped
}
