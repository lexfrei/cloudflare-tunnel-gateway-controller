package proxy_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// (no test-local errHijackNotSupported: tests use
// proxy.ErrHijackNotSupportedForTest to assert against the SAME
// sentinel the production wrapper returns -- a redeclared copy would
// trip errors.Is even with an identical message.)

// TestCountingResponseWriter_CapturesStatusAndBytes pins the wrapper's
// core contract: WriteHeader captures status; Write captures
// cumulative byte count even across multiple writes; first WriteHeader
// wins (the second is silently ignored, mirroring stdlib http behaviour).
func TestCountingResponseWriter_CapturesStatusAndBytes(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	wrapper := proxy.NewCountingResponseWriterForTest(rec)

	wrapper.WriteHeader(http.StatusTeapot)

	n1, err := wrapper.Write([]byte("hello "))
	require.NoError(t, err)
	assert.Equal(t, 6, n1)

	n2, err := wrapper.Write([]byte("world"))
	require.NoError(t, err)
	assert.Equal(t, 5, n2)

	assert.Equal(t, http.StatusTeapot, wrapper.Status(),
		"WriteHeader value must be exposed via Status()")
	assert.Equal(t, int64(11), wrapper.BytesWritten(),
		"BytesWritten must sum all Write() calls")
	assert.Equal(t, "hello world", rec.Body.String(),
		"underlying writer must receive all bytes")
}

// TestCountingResponseWriter_DefaultStatusIs200 pins stdlib's implicit
// 200 contract: if a handler calls Write without WriteHeader first,
// the recorded status must be 200 (matching httptest.ResponseRecorder
// behaviour). Without this default the access log would emit `status=0`
// for the most common handler shape.
func TestCountingResponseWriter_DefaultStatusIs200(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	wrapper := proxy.NewCountingResponseWriterForTest(rec)

	_, err := wrapper.Write([]byte("body without explicit status"))
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, wrapper.Status(),
		"implicit Write must default Status() to 200")
}

// TestCountingResponseWriter_FirstWriteHeaderWins pins the
// double-WriteHeader contract: stdlib silently ignores the second
// call. The wrapper must mirror that so the access log records the
// status the client actually saw.
func TestCountingResponseWriter_FirstWriteHeaderWins(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	wrapper := proxy.NewCountingResponseWriterForTest(rec)

	wrapper.WriteHeader(http.StatusNotFound)
	wrapper.WriteHeader(http.StatusInternalServerError)

	assert.Equal(t, http.StatusNotFound, wrapper.Status(),
		"second WriteHeader must be ignored; first one wins")
}

// TestCountingResponseWriter_PassesThroughHeader pins that Header()
// delegates to the inner writer so handlers manipulating response
// headers see the same map as without the wrapper.
func TestCountingResponseWriter_PassesThroughHeader(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	wrapper := proxy.NewCountingResponseWriterForTest(rec)

	wrapper.Header().Set("X-Custom", "value")

	assert.Equal(t, "value", rec.Header().Get("X-Custom"))
}

// TestCountingResponseWriter_FlushDelegates pins that Flush() passes
// through to the inner writer when it implements http.Flusher.
// Without this, streaming responses (SSE / chunked) would lose
// per-frame flushes when the wrapper is in place.
func TestCountingResponseWriter_FlushDelegates(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	wrapper := proxy.NewCountingResponseWriterForTest(rec)

	var rw http.ResponseWriter = wrapper

	flusher, ok := rw.(http.Flusher)
	require.True(t, ok, "wrapper must implement http.Flusher when inner does")

	// Flush should not panic; we can't easily assert it happened against
	// the recorder, but the type assertion + non-panic confirms the
	// pass-through is wired.
	flusher.Flush()

	assert.True(t, rec.Flushed, "inner recorder must receive the Flush()")
}

// TestCountingResponseWriter_HijackDelegates_NonHijacker pins the
// no-Hijack-on-the-inner contract: if the inner writer is NOT a
// http.Hijacker (httptest.NewRecorder isn't), the wrapper's Hijack
// must return a real error rather than panicking. The WebSocket
// upgrade path uses Hijack; failing loudly with a typed error keeps
// that fallback discoverable.
func TestCountingResponseWriter_HijackDelegates_NonHijacker(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	wrapper := proxy.NewCountingResponseWriterForTest(rec)

	var rw http.ResponseWriter = wrapper

	hijacker, ok := rw.(http.Hijacker)
	require.True(t, ok, "wrapper must implement http.Hijacker (delegates fallback)")

	conn, _, err := hijacker.Hijack()
	require.Error(t, err)
	require.Nil(t, conn)
	assert.ErrorIs(t, err, proxy.ErrHijackNotSupportedForTest,
		"non-Hijacker inner must surface the wrapper's static sentinel error")
}

// TestShouldSampleAccessLog_RateZeroSkipsNon5xx pins the
// always-log-errors carve-out: with rate=0.0, only status >= 500 is
// logged. 4xx and 2xx are dropped to keep volume zero on the happy
// path while still surfacing every server-side failure.
func TestShouldSampleAccessLog_RateZeroSkipsNon5xx(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		wantLog  bool
		wantRule string
	}{
		{"200 dropped at rate 0", http.StatusOK, false, "happy-path dropped to keep volume zero"},
		{"404 dropped at rate 0", http.StatusNotFound, false, "client errors dropped"},
		{"500 always logged at rate 0", http.StatusInternalServerError, true, "5xx forced through sampling"},
		{"502 always logged at rate 0", http.StatusBadGateway, true, "5xx forced through sampling"},
		{"504 always logged at rate 0", http.StatusGatewayTimeout, true, "5xx forced through sampling"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := proxy.ShouldSampleAccessLogForTest(0.0, tt.status, func() float64 { return 0.9 })
			assert.Equal(t, tt.wantLog, got, tt.wantRule)
		})
	}
}

// TestShouldSampleAccessLog_RateOneLogsEverything pins that rate=1.0
// logs every status, including 200 and 304.
func TestShouldSampleAccessLog_RateOneLogsEverything(t *testing.T) {
	t.Parallel()

	for _, status := range []int{200, 204, 301, 304, 404, 500} {
		got := proxy.ShouldSampleAccessLogForTest(1.0, status, func() float64 { return 0.999999 })
		assert.True(t, got, "rate 1.0 must log status %d", status)
	}
}

// TestShouldSampleAccessLog_PartialRate exercises a 50% rate:
// rand=0.4 logs, rand=0.6 drops, 5xx always logs regardless.
func TestShouldSampleAccessLog_PartialRate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		randVal float64
		status  int
		wantLog bool
	}{
		{"rand below rate logs 200", 0.4, http.StatusOK, true},
		{"rand above rate drops 200", 0.6, http.StatusOK, false},
		{"rand above rate still logs 503", 0.99, http.StatusServiceUnavailable, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := proxy.ShouldSampleAccessLogForTest(0.5, tt.status, func() float64 { return tt.randVal })
			assert.Equal(t, tt.wantLog, got)
		})
	}
}

// TestShouldSampleAccessLog_NegativeOrAbove1RateClamped pins that
// out-of-range rates are normalised: rate < 0 → 0 (errors-only),
// rate > 1 → 1 (always). Without this an operator typo like
// `samplingRate: 50` (intending percent) would silently match the
// rand<rate floor and log nothing.
func TestShouldSampleAccessLog_NegativeOrAbove1RateClamped(t *testing.T) {
	t.Parallel()

	// rate=50 (operator typed percent instead of fraction) clamps to 1.0 → always logs.
	assert.True(t, proxy.ShouldSampleAccessLogForTest(50.0, http.StatusOK, func() float64 { return 0.999 }),
		"rate above 1 must clamp to always-log, not silently drop everything")

	// rate=-1 clamps to 0.0 → only 5xx.
	assert.False(t, proxy.ShouldSampleAccessLogForTest(-1.0, http.StatusOK, func() float64 { return 0.001 }),
		"negative rate must clamp to errors-only, not always-log")
	assert.True(t, proxy.ShouldSampleAccessLogForTest(-1.0, http.StatusInternalServerError, func() float64 { return 0.001 }),
		"negative rate must still log 5xx")
}

// TestCountingResponseWriter_ZeroAlloc_OnEmptyBody pins that a
// handler that calls neither Write nor WriteHeader leaves BytesWritten
// at 0 and Status at 0 (the "no response sent" signal -- distinguishes
// from "implicit 200" which is observed after the first Write).
func TestCountingResponseWriter_ZeroAlloc_OnEmptyBody(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	wrapper := proxy.NewCountingResponseWriterForTest(rec)

	assert.Equal(t, int64(0), wrapper.BytesWritten(),
		"BytesWritten must start at 0")
	assert.Equal(t, 0, wrapper.Status(),
		"Status() must be 0 before any WriteHeader/Write -- distinguishes from implicit 200")
}

// TestCountingResponseWriter_SequentialWrites_SumCorrectly pins
// that the atomic byte counter sums correctly across many sequential
// Write calls. Renamed from a misnamed "ConcurrentWrites" -- HTTP/1.x
// serving is single-threaded per request, so the production hot path
// is sequential; the atomic counter exists for the deferred-emission
// read in maybeEmitAccessLog (which is sequential w.r.t. the writes
// because the defer runs after ServeHTTP returns).
func TestCountingResponseWriter_SequentialWrites_SumCorrectly(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	wrapper := proxy.NewCountingResponseWriterForTest(rec)

	wrapper.WriteHeader(http.StatusOK)

	const n = 100

	for i := range n {
		_, err := fmt.Fprintf(wrapper, "%d ", i)
		require.NoError(t, err)
	}

	// 0..99 each followed by a single space: 0..9 = 2 bytes each (20),
	// 10..99 = 3 bytes each (270), total 290.
	assert.Equal(t, int64(290), wrapper.BytesWritten())
	assert.Equal(t, http.StatusOK, wrapper.Status())
}

// errHijackerStub is a tiny http.ResponseWriter+http.Hijacker fake
// used by TestCountingResponseWriter_HijackDelegates_RealHijacker
// to verify the wrapper's Hijack call forwards correctly when the
// inner DOES support hijacking. Hijack just records that it was
// called and returns a sentinel error so the test can check the
// call path without needing a real net.Conn.
type errHijackerStub struct {
	http.ResponseWriter

	called *atomic.Bool
}

// errStubHijack is the sentinel returned by errHijackerStub.Hijack.
// Hoisted to package scope for err113 the same way as errHijackNotSupported.
var errStubHijack = errors.New("stub-hijack-sentinel")

func (e *errHijackerStub) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	e.called.Store(true)
	return nil, nil, errStubHijack
}

// TestHandler_AccessLog_DisabledByDefault pins that NewHandler
// without WithAccessLog emits zero access log lines through the
// PROCESS-WIDE default sink. Zero-cost-when-disabled is the core
// promise of the opt-in design. The earlier shape of this test
// constructed a buffer-backed logger and then discarded it -- which
// proved "an unwired buffer stays empty" not "the disabled path is
// silent" -- so swap slog.Default() to capture into buf for the
// test's lifetime, then run the handler. Any line emitted via
// slog.Default() (the failure mode the disabled-gate is supposed
// to prevent) shows up in buf and fails the assertion.
func TestHandler_AccessLog_DisabledByDefault(t *testing.T) {
	// t.Parallel skipped: slog.SetDefault mutates a process-wide
	// global; other parallel tests using slog.Default would race
	// on the swap.
	var buf bytes.Buffer

	captureLogger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	prevDefault := slog.Default()
	slog.SetDefault(captureLogger)

	t.Cleanup(func() {
		slog.SetDefault(prevDefault)
	})

	router := proxy.NewRouter()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}
	require.NoError(t, router.UpdateConfig(cfg))

	handler := proxy.NewHandler(router) // no WithAccessLog
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	// Any "msg":"access" line in buf means the disabled gate leaked
	// through to slog.Default. Allow other slog lines (e.g. unrelated
	// debug from other subsystems) so the test stays robust against
	// shared-default chatter.
	assert.NotContains(t, buf.String(), `"msg":"access"`,
		"NewHandler without WithAccessLog must emit zero access log lines through slog.Default -- zero-cost-when-disabled invariant")
}

// TestHandler_AccessLog_EmitsStructuredLine pins the full happy
// path: with WithAccessLog + rate=1.0 the handler emits one
// structured line per request containing method/host/path/status/
// bytes_written/duration_ms.
func TestHandler_AccessLog_EmitsStructuredLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	router := proxy.NewRouter()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	}))
	defer backend.Close()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}
	require.NoError(t, router.UpdateConfig(cfg))

	handler := proxy.NewHandler(router, proxy.WithAccessLog(logger, 1.0))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/api/ping", nil)
	req.Header.Set("User-Agent", "access-log-test/1.0")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	line := buf.String()
	require.NotEmpty(t, line, "rate=1.0 must produce exactly one access log line")

	// Defensive: assert the structured fields land in the JSON output.
	// JSON match by substring is fine because slog.JSONHandler emits
	// stable key="value" pairs.
	for _, want := range []string{
		`"msg":"access"`,
		`"method":"GET"`,
		`"host":"app.example.com"`,
		`"path":"/api/ping"`,
		`"status":200`,
		`"user_agent":"access-log-test/1.0"`,
	} {
		assert.Contains(t, line, want,
			"access log line must contain field: %s", want)
	}

	assert.Contains(t, line, `"bytes_written":4`,
		"bytes_written must reflect the 4 bytes of \"pong\"")
}

// TestHandler_AccessLog_RateZeroOnlyLogs5xx pins the sampling
// contract end-to-end through the handler: rate=0.0 + a 200 backend
// produces zero log lines; rate=0.0 + a 500 backend produces one.
func TestHandler_AccessLog_RateZeroOnlyLogs5xx(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		backendCode int
		wantLogged  bool
	}{
		{"200 dropped at rate 0", http.StatusOK, false},
		{"500 forced through rate 0", http.StatusInternalServerError, true},
		{"503 forced through rate 0", http.StatusServiceUnavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

			router := proxy.NewRouter()

			code := tc.backendCode
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer backend.Close()

			cfg := &proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{{
					Hostnames: []string{"app.example.com"},
					Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
				}},
			}
			require.NoError(t, router.UpdateConfig(cfg))

			handler := proxy.NewHandler(router, proxy.WithAccessLog(logger, 0.0))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if tc.wantLogged {
				require.NotEmpty(t, buf.String(),
					"rate=0 must still log %d (5xx always-log carve-out)", code)
				assert.Contains(t, buf.String(), fmt.Sprintf(`"status":%d`, code))
			} else {
				assert.Empty(t, strings.TrimSpace(buf.String()),
					"rate=0 must drop non-5xx (status %d)", code)
			}
		})
	}
}

// TestHandler_AccessLog_SkipsStatus101 pins the status-101
// carve-out: pipeWebSocket emits WriteHeader(101) BEFORE Hijack,
// so the wrapper records status=101 and the deferred
// maybeEmitAccessLog would otherwise log a line per WS upgrade
// with bytes_written=0 (post-Hijack bytes bypass the wrapper) and
// duration_ms == entire WS session length. That line is misleading
// for triage and noisy on long-lived WS routes. The skip on 101
// keeps the WS path emitting only its own diagnostics.
//
// Direct test through MaybeEmitAccessLogForTest: a normal ServeHTTP
// call can't reach maybeEmitAccessLog with status=101 because
// httputil.ReverseProxy translates a backend 101 into a 502 on the
// client side, so this is the only honest way to exercise the
// skip branch.
func TestHandler_AccessLog_SkipsStatus101(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	router := proxy.NewRouter()
	handler := proxy.NewHandler(router, proxy.WithAccessLog(logger, 1.0))

	rec := httptest.NewRecorder()
	wrapper := proxy.NewCountingResponseWriterForTest(rec)
	wrapper.WriteHeader(http.StatusSwitchingProtocols)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/ws", nil)

	proxy.MaybeEmitAccessLogForTest(handler, wrapper, req, time.Now())

	assert.Empty(t, strings.TrimSpace(buf.String()),
		"status=101 must NOT produce an access log line -- WS upgrades have their own diagnostics and the duration_ms=session-length signal would mislead")
}

// TestHandler_AccessLog_EmitsForStatus200_DirectPath is the
// positive control for MaybeEmitAccessLogForTest: with status=200
// the same direct path DOES emit a line, proving the skip in
// TestHandler_AccessLog_SkipsStatus101 is specifically gated on
// 101 rather than the helper being broken.
func TestHandler_AccessLog_EmitsForStatus200_DirectPath(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	router := proxy.NewRouter()
	handler := proxy.NewHandler(router, proxy.WithAccessLog(logger, 1.0))

	rec := httptest.NewRecorder()
	wrapper := proxy.NewCountingResponseWriterForTest(rec)
	wrapper.WriteHeader(http.StatusOK)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/x", nil)

	proxy.MaybeEmitAccessLogForTest(handler, wrapper, req, time.Now())

	assert.Contains(t, buf.String(), `"status":200`,
		"positive control: status=200 through the direct path must emit a log line")
}

// TestHandler_AccessLog_PathSnapshotPredatesURLRewrite pins that
// the access log records the path the client ASKED FOR, not the
// rewritten path the backend SAW. URL rewrite filters mutate
// req.URL.Path in place (filter.go writeRewritePathFilter); a naive
// deferred closure re-reading req at emission time would log
// `/new` instead of `/old`, hiding the operator-visible signal
// they actually need for triage.
func TestHandler_AccessLog_PathSnapshotPredatesURLRewrite(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	rewriteTo := "/rewritten/handler"

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
			Filters: []proxy.RouteFilter{{
				Type: proxy.FilterURLRewrite,
				URLRewrite: &proxy.URLRewriteConfig{
					Path: &proxy.URLRewritePath{
						Type:            proxy.URLRewriteFullPath,
						ReplaceFullPath: &rewriteTo,
					},
				},
			}},
			Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	handler := proxy.NewHandler(router, proxy.WithAccessLog(logger, 1.0))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/client-original", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	line := buf.String()
	require.NotEmpty(t, line)
	assert.Contains(t, line, `"path":"/client-original"`,
		"access log must record the path the client SENT (pre-rewrite), not the rewritten path the backend received")
	assert.NotContains(t, line, `"path":"/rewritten/handler"`,
		"access log must NOT leak the post-rewrite path -- it would hide the operator-visible client signal")
}

// TestHandler_AccessLog_EmitsQueryString pins the `query` field
// addition from round-1 review: operators triaging a `?action=delete`
// vs `?action=read` incident need the query string in the log line.
func TestHandler_AccessLog_EmitsQueryString(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	router := proxy.NewRouter()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}
	require.NoError(t, router.UpdateConfig(cfg))

	handler := proxy.NewHandler(router, proxy.WithAccessLog(logger, 1.0))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://app.example.com/api?action=delete&id=42", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	line := buf.String()
	assert.Contains(t, line, `"query":"action=delete&id=42"`,
		"access log must include the request query string")
	assert.Contains(t, line, `"path":"/api"`,
		"path must remain query-stripped (path/query split is structured)")
}

// TestCountingResponseWriter_HijackDelegates_RealHijacker pins that
// when the inner IS an http.Hijacker, the wrapper forwards Hijack
// to it (rather than returning the not-supported sentinel). This is
// the path the WebSocket upgrade handler exercises in production.
func TestCountingResponseWriter_HijackDelegates_RealHijacker(t *testing.T) {
	t.Parallel()

	var called atomic.Bool

	inner := &errHijackerStub{ResponseWriter: httptest.NewRecorder(), called: &called}
	wrapper := proxy.NewCountingResponseWriterForTest(inner)

	var asRW http.ResponseWriter = wrapper

	hijacker, ok := asRW.(http.Hijacker)
	require.True(t, ok)

	conn, brw, err := hijacker.Hijack()
	require.Error(t, err)
	assert.ErrorIs(t, err, errStubHijack)
	assert.Nil(t, conn)
	assert.Nil(t, brw)
	assert.True(t, called.Load(), "wrapper must delegate Hijack to inner when it implements http.Hijacker")
}

// readerFromRecorder wraps httptest.NewRecorder with an io.ReaderFrom
// implementation so the test can drive the wrapper's ReadFrom
// delegation path and verify (a) the fast path is taken when the
// inner supports it, (b) the byte count includes those bytes, (c)
// the inner's recorded body matches.
type readerFromRecorder struct {
	*httptest.ResponseRecorder

	readFromCalls *atomic.Int32
}

func (r *readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.readFromCalls.Add(1)
	n, err := io.Copy(r.Body, src)
	if err != nil {
		return n, fmt.Errorf("readerFromRecorder: %w", err)
	}

	return n, nil
}

// TestCountingResponseWriter_ReadFrom_DelegatesToInner pins the
// performance contract: when the inner writer implements
// io.ReaderFrom, the wrapper MUST delegate to it (splice / sendfile
// fast path on Linux for large bodies) instead of falling back to
// the generic io.Copy + Write loop. The wrapper's own
// bytesWritten counter must include the delegated bytes so the
// access-log line reports the real response size, not zero.
func TestCountingResponseWriter_ReadFrom_DelegatesToInner(t *testing.T) {
	t.Parallel()

	var rfCount atomic.Int32

	inner := &readerFromRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		readFromCalls:    &rfCount,
	}
	wrapper := proxy.NewCountingResponseWriterForTest(inner)

	const payload = "a-large-payload-that-would-be-spliced"

	src := strings.NewReader(payload)

	var rw http.ResponseWriter = wrapper

	rf, ok := rw.(io.ReaderFrom)
	require.True(t, ok, "wrapper must implement io.ReaderFrom when inner does")

	n, err := rf.ReadFrom(src)
	require.NoError(t, err)
	assert.Equal(t, int64(len(payload)), n,
		"ReadFrom must return the inner's byte count")
	assert.Equal(t, int64(len(payload)), wrapper.BytesWritten(),
		"wrapper.BytesWritten must reflect the bytes ReadFrom transferred")
	assert.Equal(t, int32(1), rfCount.Load(),
		"inner ReadFrom must be called exactly once (splice fast path)")
	assert.Equal(t, payload, inner.Body.String(),
		"inner's body buffer must contain the transferred payload")
}

// TestCountingResponseWriter_ReadFrom_FallbackWhenInnerLacksIt pins
// the fallback contract: when the inner writer does NOT implement
// io.ReaderFrom (e.g. cloudflared's HTTP/2 writer fake), the wrapper
// MUST still satisfy io.ReaderFrom by falling back to the generic
// Write loop, AND the bytesWritten counter must still reflect the
// transferred bytes (otherwise the access log would report 0 for
// every h2 response).
func TestCountingResponseWriter_ReadFrom_FallbackWhenInnerLacksIt(t *testing.T) {
	t.Parallel()

	// httptest.ResponseRecorder does NOT implement io.ReaderFrom.
	// noReadFromWriter wraps it as a future-proofing guard so the
	// fallback path stays exercised even if a future stdlib bump
	// adds io.ReaderFrom to the recorder. The type assertion below
	// verifies the fake exposes only http.ResponseWriter so this
	// test never silently flips to the fast path.
	inner := &noReadFromWriter{
		ResponseWriter: httptest.NewRecorder(),
		writeBytes:     &bytes.Buffer{},
	}

	// Confirm the test fake does NOT implement io.ReaderFrom so the
	// fallback path is the one we're actually exercising.
	_, isReaderFrom := any(inner).(io.ReaderFrom)
	require.False(t, isReaderFrom, "test fake must NOT implement io.ReaderFrom to exercise the fallback")

	wrapper := proxy.NewCountingResponseWriterForTest(inner)

	const payload = "fallback-payload"

	src := strings.NewReader(payload)

	var rw http.ResponseWriter = wrapper

	rf, ok := rw.(io.ReaderFrom)
	require.True(t, ok, "wrapper must still implement io.ReaderFrom even when inner doesn't")

	n, err := rf.ReadFrom(src)
	require.NoError(t, err)
	assert.Equal(t, int64(len(payload)), n,
		"fallback ReadFrom must return the full transferred byte count")
	assert.Equal(t, int64(len(payload)), wrapper.BytesWritten(),
		"bytesWritten must reflect the fallback transfer")
	assert.Equal(t, payload, inner.writeBytes.String(),
		"inner Write must receive the full payload via the fallback loop")
}

// noReadFromWriter is a test fake that wraps an http.ResponseWriter
// but explicitly does NOT implement io.ReaderFrom. Used to drive the
// counting wrapper's fallback path.
type noReadFromWriter struct {
	http.ResponseWriter

	writeBytes *bytes.Buffer
}

func (n *noReadFromWriter) Write(p []byte) (int, error) {
	written, err := n.writeBytes.Write(p)
	if err != nil {
		return written, fmt.Errorf("noReadFromWriter: %w", err)
	}

	return written, nil
}

// TestHandler_AccessLog_StripQuery_DefaultsOff pins the default
// behaviour: WithAccessLog without stripQuery option logs the query
// string verbatim. Catches a future regression that flips the
// default to "always strip" -- operators triaging non-token query
// params (?trace=true, ?action=delete) would lose the signal.
func TestHandler_AccessLog_StripQuery_DefaultsOff(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	router := proxy.NewRouter()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	handler := proxy.NewHandler(router, proxy.WithAccessLog(logger, 1.0))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://app.example.com/api?trace=true&token=secret", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Contains(t, buf.String(), `"query":"trace=true&token=secret"`,
		"WithAccessLog without WithAccessLogStripQuery must log query verbatim")
}

// TestHandler_AccessLog_StripQuery_ElidesQuery pins the opt-in
// strip behaviour: WithAccessLogStripQuery(true) zeroes the query
// field in every emitted line. The path / status / other fields are
// untouched -- operators still see WHICH endpoint was hit, just not
// WHICH parameters carried token-shaped values.
func TestHandler_AccessLog_StripQuery_ElidesQuery(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	router := proxy.NewRouter()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	handler := proxy.NewHandler(router,
		proxy.WithAccessLog(logger, 1.0),
		proxy.WithAccessLogStripQuery(true),
	)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://app.example.com/api?token=secret&action=delete", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	line := buf.String()
	assert.Contains(t, line, `"query":""`,
		"stripQuery=true must zero the query field in the log line")
	assert.NotContains(t, line, "token=secret",
		"stripped query string must not leak through any field")
	assert.NotContains(t, line, "action=delete",
		"stripped query string must not leak through any field")
	assert.Contains(t, line, `"path":"/api"`,
		"path field must be unaffected by stripQuery")
	assert.Contains(t, line, `"status":200`,
		"status field must be unaffected by stripQuery")
}

// TestHandler_AccessLog_StripQuery_FalseIsNoop pins that explicitly
// passing WithAccessLogStripQuery(false) is equivalent to not
// passing it at all -- both produce the verbatim-query default.
// Catches a regression where the option mistakenly always strips
// regardless of the bool.
func TestHandler_AccessLog_StripQuery_FalseIsNoop(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	router := proxy.NewRouter()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	handler := proxy.NewHandler(router,
		proxy.WithAccessLog(logger, 1.0),
		proxy.WithAccessLogStripQuery(false),
	)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://app.example.com/api?x=1", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Contains(t, buf.String(), `"query":"x=1"`,
		"WithAccessLogStripQuery(false) must NOT strip; verbatim default holds")
}

// TestCountingResponseWriter_ReadFrom_SetsImplicitStatus200 pins
// that ReadFrom honours the same implicit-200 contract Write does:
// a caller that drives ReadFrom without an explicit WriteHeader
// must end up with Status()==200. Without this the access-log
// line would render `status:0` and the 5xx-always-log sampling
// carve-out (which gates on >= 500) would misclassify the request.
func TestCountingResponseWriter_ReadFrom_SetsImplicitStatus200(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	wrapper := proxy.NewCountingResponseWriterForTest(rec)

	src := strings.NewReader("body")
	_, err := wrapper.ReadFrom(src)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, wrapper.Status(),
		"ReadFrom without prior WriteHeader must default Status() to 200 (same as Write)")
}

// TestCountingResponseWriter_ReadFrom_DoesNotOverrideExplicitStatus
// pins that ReadFrom MUST NOT clobber an explicit WriteHeader. If
// the handler called WriteHeader(StatusTeapot) and then funneled
// through ReadFrom, the access log must still record 418, not 200.
func TestCountingResponseWriter_ReadFrom_DoesNotOverrideExplicitStatus(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	wrapper := proxy.NewCountingResponseWriterForTest(rec)

	wrapper.WriteHeader(http.StatusTeapot)

	src := strings.NewReader("body")
	_, err := wrapper.ReadFrom(src)
	require.NoError(t, err)

	assert.Equal(t, http.StatusTeapot, wrapper.Status(),
		"explicit WriteHeader before ReadFrom must NOT be clobbered by the implicit-200 path")
}
