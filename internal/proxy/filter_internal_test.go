package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

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
