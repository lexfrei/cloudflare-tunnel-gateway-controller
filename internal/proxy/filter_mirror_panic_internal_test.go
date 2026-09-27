package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// panicRoundTripper panics instead of performing the request, standing in for
// any bug reachable from inside the mirror's dispatch goroutine.
type panicRoundTripper struct {
	reached chan struct{}
}

func (p *panicRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	close(p.reached)

	panic("mirror transport exploded")
}

// TestRequestMirror_ContainsDispatchPanic pins that the mirror's fire-and-forget
// goroutine cannot take the process down.
//
// ProcessRequest returns before the mirror is delivered, so nothing is left
// watching that goroutine: a panic in it is unrecoverable by the request that
// spawned it and would kill the whole shared data plane. The guard lives at the
// top of dispatchWithRetry; this pins it, because a panic here fails the run by
// crashing the test binary rather than by failing an assertion.
func TestRequestMirror_ContainsDispatchPanic(t *testing.T) {
	t.Parallel()

	reached := make(chan struct{})

	mirror := &requestMirror{
		backendURL: "http://mirror.example.invalid",
		client:     &http.Client{Transport: &panicRoundTripper{reached: reached}},
		// NewRequestMirror always sets one; the literal must too, because the
		// retry-exhaustion path logs through it and a nil deref there would
		// crash the process from inside the goroutine this test guards.
		logger: slog.Default(),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/mirrored", nil)

	resp := mirror.ProcessRequest(req) //nolint:bodyclose // a nil response has no body; asserted below
	require.Nil(t, resp, "mirroring never answers the primary request")

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the mirror goroutine never reached the transport")
	}

	// Give the panicking goroutine a moment to unwind. An unguarded panic takes
	// the process with it, so surviving this window is the assertion.
	time.Sleep(100 * time.Millisecond)
}

// recordSink is a slog.Handler that hands every record to a channel, so a
// test can wait for a line written from another goroutine.
type recordSink struct {
	records chan slog.Record
}

func (s *recordSink) Enabled(context.Context, slog.Level) bool { return true }

func (s *recordSink) Handle(_ context.Context, record slog.Record) error {
	s.records <- record.Clone()

	return nil
}

func (s *recordSink) WithAttrs([]slog.Attr) slog.Handler { return s }

func (s *recordSink) WithGroup(string) slog.Handler { return s }

// TestRequestMirror_DispatchPanicLogsThroughInjectedLogger pins what the
// contained panic records: it goes to the filter's own logger like every other
// line the filter writes, and it carries the stack, since the request that
// spawned the goroutine was answered long before and the panic value alone
// rarely names the failing code.
func TestRequestMirror_DispatchPanicLogsThroughInjectedLogger(t *testing.T) {
	t.Parallel()

	sink := &recordSink{records: make(chan slog.Record, 4)}

	mirror := &requestMirror{
		backendURL: "http://mirror.example.invalid",
		client:     &http.Client{Transport: &panicRoundTripper{reached: make(chan struct{})}},
		logger:     slog.New(sink),
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/mirrored", nil)

	resp := mirror.ProcessRequest(req) //nolint:bodyclose // a nil response has no body
	require.Nil(t, resp)

	var record slog.Record

	select {
	case record = <-sink.records:
	case <-time.After(5 * time.Second):
		t.Fatal("the contained panic was not logged through the injected logger")
	}

	attrs := map[string]string{}

	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.String()

		return true
	})

	assert.Equal(t, slog.LevelError, record.Level)
	assert.Contains(t, attrs["panic"], "mirror transport exploded")
	assert.True(t, strings.Contains(attrs["stack"], "panicRoundTripper"),
		"the stack must name the frame that panicked, got %q", attrs["stack"])
}
