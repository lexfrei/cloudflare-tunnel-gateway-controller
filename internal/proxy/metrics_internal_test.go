package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewMetrics_RegistersAllFamilies pins the full metric-family surface of
// the data-plane registry: a rename or a dropped instrument is an operator-
// visible breaking change (dashboards, HPA queries) and must fail here first.
func TestNewMetrics_RegistersAllFamilies(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	// Touch every labelled instrument once so Gather emits its family.
	metrics.requestsInFlight.Inc()
	metrics.wsActiveSessions.Inc()
	metrics.requestDuration.WithLabelValues("app.example.com").Observe(0.1)
	metrics.requestsTotal.WithLabelValues("app.example.com", "2xx").Inc()
	metrics.backendErrors.WithLabelValues("app.example.com", "dial").Inc()
	metrics.responseBytes.WithLabelValues("app.example.com").Add(42)
	metrics.requestBytes.WithLabelValues("app.example.com").Add(7)

	families, err := reg.Gather()
	require.NoError(t, err)

	got := make(map[string]bool, len(families))
	for _, family := range families {
		got[family.GetName()] = true
	}

	want := []string{
		"cftunnel_proxy_requests_in_flight",
		"cftunnel_proxy_websocket_active_sessions",
		"cftunnel_proxy_request_duration_seconds",
		"cftunnel_proxy_requests_total",
		"cftunnel_proxy_backend_errors_total",
		"cftunnel_proxy_response_bytes_total",
		"cftunnel_proxy_request_bytes_total",
	}
	for _, name := range want {
		assert.True(t, got[name], "missing metric family %s", name)
	}

	assert.Len(t, families, len(want), "unexpected extra metric families on the proxy registry")
}

// TestNewMetrics_MergedGatherersStayClean pins the /metrics composition rule:
// the proxy registry must not carry Go/process collectors, because the
// endpoint merges it with prometheus.DefaultGatherer (where cloudflared's and
// client_golang's default collectors live) and duplicate families would fail
// the whole scrape.
func TestNewMetrics_MergedGatherersStayClean(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	metrics.requestsTotal.WithLabelValues("app.example.com", "2xx").Inc()

	merged := prometheus.Gatherers{reg, prometheus.DefaultGatherer}

	_, err := merged.Gather()
	require.NoError(t, err, "merged gather must stay collision-free")
}

func TestStatusClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		want   string
	}{
		{name: "zero means handler wrote nothing", status: 0, want: "aborted"},
		{name: "101 upgrade", status: 101, want: "1xx"},
		{name: "200", status: 200, want: "2xx"},
		{name: "204", status: 204, want: "2xx"},
		{name: "301", status: 301, want: "3xx"},
		{name: "404", status: 404, want: "4xx"},
		{name: "500", status: 500, want: "5xx"},
		{name: "599", status: 599, want: "5xx"},
		{name: "out of range high", status: 723, want: "other"},
		{name: "out of range low", status: 42, want: "aborted"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, statusClass(tt.status))
		})
	}
}

// TestMetricsOnUpgrade_HijackWithoutStatusCountsAsUpgrade pins the standalone
// WebSocket accounting path: stdlib httputil.ReverseProxy writes the "101
// Switching Protocols" bytes DIRECTLY to the hijacked connection, bypassing
// the counting ResponseWriter — so at hijack time no status was recorded. A
// successful hijack IS a successful upgrade and must count as "1xx", not
// "aborted" (which would make every standalone WS session look like a failed
// exchange on dashboards).
func TestMetricsOnUpgrade_HijackWithoutStatusCountsAsUpgrade(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	counted := newCountingResponseWriter(nil)
	state := metrics.beginRequest(counted, &http.Request{})
	state.setHostname("ws.example.com")

	// Simulate the stdlib path: hijack fires with NO WriteHeader call.
	state.onUpgrade()
	state.finish()

	value := testutil.ToFloat64(metrics.requestsTotal.WithLabelValues("ws.example.com", "1xx"))
	assert.InDelta(t, 1.0, value, 0,
		"a hijack with no recorded status is a successful upgrade, not an aborted exchange")

	aborted := testutil.ToFloat64(metrics.requestsTotal.WithLabelValues("ws.example.com", "aborted"))
	assert.InDelta(t, 0.0, aborted, 0)
}

// readSentinelError is compared by IDENTITY below — the wrapper must hand it
// back unchanged.
var errReadSentinel = errors.New("read sentinel")

type sentinelReader struct{}

func (sentinelReader) Read([]byte) (int, error) { return 3, errReadSentinel }
func (sentinelReader) Close() error             { return nil }

// TestCountingBody_ReadErrorsPassThroughUnwrapped pins the invisibility
// contract of the byte-counting body wrapper: stdlib transports and handlers
// compare read errors by identity (not just io.EOF — http.ErrBodyReadAfterClose,
// net sentinel errors), so instrumentation must return the inner error AS IS
// while still counting the bytes that were read.
func TestCountingBody_ReadErrorsPassThroughUnwrapped(t *testing.T) {
	t.Parallel()

	var counter atomic.Int64

	body := &countingBody{inner: sentinelReader{}, counter: &counter}

	n, err := body.Read(make([]byte, 8))
	assert.Equal(t, 3, n)
	assert.Equal(t, int64(3), counter.Load(), "bytes preceding the error must still count")

	//nolint:errorlint,err113 // identity comparison IS the contract under test
	assert.True(t, err == errReadSentinel,
		"the inner error must come back by identity, not wrapped")
}

// timeoutSentinelError satisfies the net.Error-style Timeout() interface the
// stdlib transport uses for ResponseHeaderTimeout failures.
type timeoutSentinelError struct{}

func (timeoutSentinelError) Error() string { return "sentinel timeout" }
func (timeoutSentinelError) Timeout() bool { return true }

// Static test errors (err113: no inline errors.New at call sites).
var (
	errTestConnRefused = errors.New("connection refused")
	errTestGeneric     = errors.New("boom")
)

func TestClassifyBackendError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil is other", err: nil, want: "other"},
		{name: "context canceled", err: context.Canceled, want: "canceled"},
		{name: "wrapped canceled", err: fmt.Errorf("wrap: %w", context.Canceled), want: "canceled"},
		{
			name: "dial op error",
			err:  &net.OpError{Op: "dial", Err: errTestConnRefused},
			want: "dial",
		},
		{
			name: "dial timeout stays dial",
			err:  &net.OpError{Op: "dial", Err: timeoutSentinelError{}},
			want: "dial",
		},
		{name: "deadline exceeded", err: context.DeadlineExceeded, want: "timeout"},
		{name: "transport header timeout sentinel", err: timeoutSentinelError{}, want: "timeout"},
		{name: "tls record header", err: tls.RecordHeaderError{Msg: "bad"}, want: "tls"},
		{name: "x509 unknown authority", err: x509.UnknownAuthorityError{}, want: "tls"},
		{name: "backend tls chain sentinel", err: errBackendTLSChainVerify, want: "tls"},
		{name: "backend tls san sentinel", err: errBackendTLSSANMissing, want: "tls"},
		{name: "generic", err: errTestGeneric, want: "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, classifyBackendError(tt.err))
		})
	}
}
