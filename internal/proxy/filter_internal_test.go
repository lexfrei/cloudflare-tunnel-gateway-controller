package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// errSimulatedTransientFailure is the transport-level error flakyRoundTripper
// returns for the attempts it is configured to fail. A package-level sentinel
// keeps err113 satisfied (no dynamically constructed errors).
var errSimulatedTransientFailure = errors.New("simulated transient transport failure")

// chanHandler is a minimal slog.Handler that forwards every record to a
// channel. It lets tests deterministically observe a fire-and-forget
// goroutine's log output without mutating the process-global default logger
// (which the parallel proxy_test suite also reads).
type chanHandler struct {
	records chan slog.Record
}

func (*chanHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *chanHandler) Handle(_ context.Context, rec slog.Record) error {
	h.records <- rec.Clone()

	return nil
}

func (h *chanHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *chanHandler) WithGroup(string) slog.Handler { return h }

// collectAttrs flattens a record's attributes into a key→string map for
// assertions.
func collectAttrs(rec slog.Record) map[string]string {
	attrs := make(map[string]string)

	rec.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.String()

		return true
	})

	return attrs
}

// TestRequestMirror_DispatchError_LogsWarn asserts that a failed mirror dial
// emits a Warn record identifying the mirror destination and the error,
// without affecting the primary leg. Mirroring stays fire-and-forget, but a
// silent failure makes any delivery problem undiagnosable from proxy logs.
func TestRequestMirror_DispatchError_LogsWarn(t *testing.T) {
	t.Parallel()

	records := make(chan slog.Record, 8)
	logger := slog.New(&chanHandler{records: records})

	// Port 1 refuses connections immediately, so the dial fails fast.
	const backend = "http://127.0.0.1:1"

	mirror := &requestMirror{
		backendURL: backend,
		client:     mirrorClient,
		logger:     logger,
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/test", nil)

	resp := mirror.ProcessRequest(req) //nolint:bodyclose // mirror returns a nil response
	assert.Nil(t, resp, "mirror filter must not short-circuit the primary leg")

	select {
	case rec := <-records:
		assert.Equal(t, slog.LevelWarn, rec.Level, "dispatch failure must log at Warn")

		attrs := collectAttrs(rec)
		assert.Equal(t, backend, attrs["backend"], "log must identify the mirror destination")
		assert.NotEmpty(t, attrs["error"], "log must include the dial error")
	case <-time.After(2 * time.Second):
		t.Fatal("mirror dispatch error was not logged")
	}
}

// flakyRoundTripper fails the first failFirst RoundTrip calls with a
// transport-level error, then delegates to inner. It models a transient
// proxy → mirror-backend dispatch failure (connection reset, stale idle-conn
// reuse, momentary backend unreadiness) deterministically, without depending
// on TCP timing.
type flakyRoundTripper struct {
	failFirst int32
	attempts  atomic.Int32
	inner     http.RoundTripper
}

func (f *flakyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.attempts.Add(1) <= f.failFirst {
		// A real transport consumes/closes the body on the attempt; drop it so
		// a retry that does not supply a fresh reader would observe an empty
		// body (and fail the body-replay assertions below).
		if req.Body != nil {
			_ = req.Body.Close()
		}

		return nil, errSimulatedTransientFailure
	}

	resp, err := f.inner.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("flaky inner roundtrip: %w", err)
	}

	return resp, nil
}

// TestRequestMirror_TransientDispatchFailure_Retries pins the #361 fix: a 100%
// mirror whose first dispatch attempt fails with a transient transport error
// must still be delivered. The conformance suite sends a single request and
// polls the mirror backend, so a one-shot fire-and-forget dispatch that drops
// the only copy on a transient failure flakes deterministically. With the
// bounded retry the copy lands on a later attempt. Before the fix this test is
// RED — the single client.Do drops the copy and the backend never sees it.
func TestRequestMirror_TransientDispatchFailure_Retries(t *testing.T) {
	t.Parallel()

	records := make(chan slog.Record, 8)
	logger := slog.New(&chanHandler{records: records})

	received := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		received <- struct{}{}
	}))
	defer backend.Close()

	mirror := &requestMirror{
		backendURL: backend.URL,
		client: &http.Client{
			Timeout:   mirrorTimeout,
			Transport: &flakyRoundTripper{failFirst: 1, inner: http.DefaultTransport},
		},
		logger: logger,
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/test", nil)

	resp := mirror.ProcessRequest(req) //nolint:bodyclose // mirror returns a nil response
	assert.Nil(t, resp, "mirror filter must not short-circuit the primary leg")

	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("mirror copy was dropped after a transient dispatch failure; expected a retry to deliver it")
	}
}

// TestRequestMirror_TransientDispatchFailure_RetriesWithBody pins that a retry
// supplies a fresh body reader from the buffered data, not the consumed reader
// from the failed attempt. Without per-attempt body replay the retried request
// would carry an empty body, so the backend must observe the full payload.
func TestRequestMirror_TransientDispatchFailure_RetriesWithBody(t *testing.T) {
	t.Parallel()

	const payload = "mirror-body-payload"

	gotBody := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		gotBody <- string(body)
	}))
	defer backend.Close()

	mirror := &requestMirror{
		backendURL: backend.URL,
		client: &http.Client{
			Timeout:   mirrorTimeout,
			Transport: &flakyRoundTripper{failFirst: 1, inner: http.DefaultTransport},
		},
		logger: slog.New(&chanHandler{records: make(chan slog.Record, 8)}),
	}

	req := httptest.NewRequestWithContext(
		context.Background(), http.MethodPost, "http://example.com/test", strings.NewReader(payload),
	)

	resp := mirror.ProcessRequest(req) //nolint:bodyclose // mirror returns a nil response
	assert.Nil(t, resp, "mirror filter must not short-circuit the primary leg")

	// The primary leg must still see the full body after buffering.
	primaryBody, _ := io.ReadAll(req.Body)
	assert.Equal(t, payload, string(primaryBody), "primary leg body must be restored after mirror buffering")

	select {
	case got := <-gotBody:
		assert.Equal(t, payload, got, "retried mirror copy must carry the full body, not an empty reader")
	case <-time.After(3 * time.Second):
		t.Fatal("mirror copy with body was dropped after a transient dispatch failure")
	}
}

// TestRequestMirror_DispatchSuccess_NoWarn asserts that a successful mirror
// emits no Warn record, so percentage mirroring does not produce per-request
// log noise on the happy path.
func TestRequestMirror_DispatchSuccess_NoWarn(t *testing.T) {
	t.Parallel()

	records := make(chan slog.Record, 8)
	logger := slog.New(&chanHandler{records: records})

	received := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		received <- struct{}{}
	}))
	defer backend.Close()

	mirror := &requestMirror{
		backendURL: backend.URL,
		client:     mirrorClient,
		logger:     logger,
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/test", nil)

	resp := mirror.ProcessRequest(req) //nolint:bodyclose // mirror returns a nil response
	assert.Nil(t, resp, "mirror filter must not short-circuit the primary leg")

	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("mirror request never reached the backend")
	}

	// The dispatch goroutine has nothing to log on success; confirm no record
	// arrives in a short window after delivery completed.
	select {
	case rec := <-records:
		t.Fatalf("unexpected log on mirror happy path: level=%s msg=%q", rec.Level, rec.Message)
	case <-time.After(500 * time.Millisecond):
	}
}
