package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Backend-error reason label values. A closed set keeps the
// cftunnel_proxy_backend_errors_total cardinality bounded.
const (
	backendErrReasonDial        = "dial"
	backendErrReasonTimeout     = "timeout"
	backendErrReasonTLS         = "tls"
	backendErrReasonCanceled    = "canceled"
	backendErrReasonWSDial      = "ws_dial"
	backendErrReasonWSHandshake = "ws_handshake"
	backendErrReasonOther       = "other"
)

// statusClassUpgrade is the requests_total status_class for successful
// protocol upgrades (101 lands in the 1xx class).
const statusClassUpgrade = "1xx"

// statusClassAborted is the requests_total status_class for requests where
// the handler wrote no response at all (client canceled before any write).
const statusClassAborted = "aborted"

// metricLabelHostname is the per-route label key shared by every labelled
// data-plane instrument.
const metricLabelHostname = "hostname"

// durationBucketCeiling is the extra top histogram bucket (seconds) appended
// to the Prometheus defaults, matching the proxy's header-timeout scale.
const durationBucketCeiling = 30

// maxValidHTTPStatus bounds the recognised status range for statusClass
// (HTTP statuses are 100-599; anything above lands in "other").
const maxValidHTTPStatus = 600

// Metrics holds the data-plane Prometheus instruments. Construction mirrors
// the cfmetrics pattern: NewMetrics registers everything on the passed
// Registerer, which in production is a dedicated registry — NEVER the global
// default, where the embedded cloudflared MustRegisters its own collectors
// and panics on duplicates. The registry must also stay free of Go/process
// collectors: /metrics merges it with prometheus.DefaultGatherer (which
// already carries them) and duplicate families fail the whole scrape.
//
// The hostname label on the per-route instruments is always the MATCHED rule
// hostname pattern (exact host, "*.suffix" wildcard pattern, or "" for
// default-bucket and unmatched requests) — never the raw request Host — so
// series cardinality is bounded by the pushed config, not by what clients
// send.
type Metrics struct {
	// requestsInFlight is the saturation signal an HPA should scale on:
	// requests currently inside ServeHTTP, excluding hijacked WebSocket
	// sessions (those move to wsActiveSessions at upgrade time). Label-free
	// so the scaling query needs no aggregation.
	requestsInFlight prometheus.Gauge
	// wsActiveSessions counts live post-upgrade WebSocket sessions. Kept
	// separate from requestsInFlight: a long-lived session is capacity
	// pressure of a different kind than an in-flight HTTP exchange, and
	// folding both into one gauge would make the HPA signal unusable on
	// WS-heavy workloads.
	wsActiveSessions prometheus.Gauge
	// requestDuration observes wall time from request arrival to response
	// completion. WebSocket upgrades observe time-to-upgrade (arrival → 101),
	// not session lifetime.
	requestDuration *prometheus.HistogramVec
	// requestsTotal counts completed exchanges by status class (1xx..5xx,
	// "aborted" for no-response, "other" for out-of-range). A WebSocket
	// upgrade counts as 1xx at hijack time.
	requestsTotal *prometheus.CounterVec
	// backendErrors counts backend dial/connect failures by closed-set reason.
	backendErrors *prometheus.CounterVec
	// responseBytes accumulates response body bytes as counted by the
	// response writer wrapper. Post-hijack WebSocket bytes bypass the wrapper
	// and are NOT counted.
	responseBytes *prometheus.CounterVec
	// requestBytes accumulates request body bytes actually read from the
	// client.
	requestBytes *prometheus.CounterVec
}

// requestDurationBuckets extends the Prometheus defaults (5ms..10s) with a
// 30s bucket matching the proxy's header-timeout scale.
func requestDurationBuckets() []float64 {
	return append(prometheus.DefBuckets, durationBucketCeiling)
}

// NewMetrics builds the data-plane instruments and registers them on reg.
// Panics on duplicate registration (MustRegister), consistent with cfmetrics.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	metrics := &Metrics{
		requestsInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cftunnel_proxy_requests_in_flight",
			Help: "Requests currently being served by the proxy, excluding hijacked WebSocket sessions. The saturation signal for horizontal scaling.",
		}),
		wsActiveSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cftunnel_proxy_websocket_active_sessions",
			Help: "Live post-upgrade WebSocket sessions piped by the proxy.",
		}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cftunnel_proxy_request_duration_seconds",
			Help:    "Request wall time from arrival to response completion; WebSocket upgrades observe time-to-upgrade.",
			Buckets: requestDurationBuckets(),
		}, []string{metricLabelHostname}),
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cftunnel_proxy_requests_total",
			Help: "Completed exchanges by matched hostname pattern and status class (1xx..5xx; aborted = no response written; other = status outside 100-599).",
		}, []string{metricLabelHostname, "status_class"}),
		backendErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cftunnel_proxy_backend_errors_total",
			Help: "Backend dial/connect failures by reason (dial, timeout, tls, canceled, ws_dial, ws_handshake, other).",
		}, []string{metricLabelHostname, "reason"}),
		responseBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cftunnel_proxy_response_bytes_total",
			Help: "Response body bytes written to clients (post-hijack WebSocket bytes excluded).",
		}, []string{metricLabelHostname}),
		requestBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cftunnel_proxy_request_bytes_total",
			Help: "Request body bytes read from clients.",
		}, []string{metricLabelHostname}),
	}

	reg.MustRegister(
		metrics.requestsInFlight,
		metrics.wsActiveSessions,
		metrics.requestDuration,
		metrics.requestsTotal,
		metrics.backendErrors,
		metrics.responseBytes,
		metrics.requestBytes,
	)

	return metrics
}

// metricsRequestState carries per-request bookkeeping between ServeHTTP's
// entry instrumentation and its deferred finish. Everything mutates on the
// request goroutine except body byte counting — the outbound transport may
// read the request body on its own goroutine, hence the atomic counter.
type metricsRequestState struct {
	metrics   *Metrics
	counted   *countingResponseWriter
	start     time.Time
	hostname  string
	bodyBytes atomic.Int64
	// upgraded flips at successful hijack time (set by onUpgrade on the
	// request goroutine, read by finish on the same goroutine).
	upgraded bool
}

// beginRequest increments the in-flight gauge, wraps the request body for
// byte counting, and installs the upgrade hook on the counting writer. The
// caller MUST defer the returned state's finish.
func (m *Metrics) beginRequest(counted *countingResponseWriter, req *http.Request) *metricsRequestState {
	state := &metricsRequestState{metrics: m, counted: counted, start: time.Now()}

	m.requestsInFlight.Inc()

	if req.Body != nil && req.Body != http.NoBody {
		req.Body = &countingBody{inner: req.Body, counter: &state.bodyBytes}
	}

	counted.onHijack = state.onUpgrade

	return state
}

// setHostname records the matched hostname pattern once routing has run.
// Nil-safe so ServeHTTP can call it unconditionally.
func (s *metricsRequestState) setHostname(hostname string) {
	if s == nil {
		return
	}

	s.hostname = hostname
}

// onUpgrade runs at successful hijack time (WebSocket upgrade): the HTTP
// exchange is over, so the request leaves the in-flight gauge, enters the
// session gauge, observes time-to-upgrade, and counts by the written status
// (101 → "1xx"). A hijack with NO recorded status is also a successful
// upgrade: stdlib httputil.ReverseProxy (the standalone-mode path) writes the
// 101 bytes directly to the hijacked connection, bypassing the counting
// writer. The post-upgrade session is accounted by finish.
func (s *metricsRequestState) onUpgrade() {
	if s.upgraded {
		// A second hijack on the same request must not double-count: it would
		// double-Dec the in-flight gauge (driving it negative) and double-Inc
		// the session gauge. Single-hijack-per-flow holds today; this makes the
		// no-double-count contract enforced rather than convention-dependent.
		return
	}

	s.upgraded = true
	s.metrics.requestsInFlight.Dec()
	s.metrics.wsActiveSessions.Inc()
	s.metrics.requestDuration.WithLabelValues(s.hostname).Observe(time.Since(s.start).Seconds())

	status := s.counted.Status()

	class := statusClass(status)
	if status == 0 {
		class = statusClassUpgrade
	}

	s.metrics.requestsTotal.WithLabelValues(s.hostname, class).Inc()
}

// finish is the deferred tail of an instrumented request. For an upgraded
// (hijacked) request only the session gauge is released — the HTTP exchange
// was already recorded at upgrade time and the session lifetime must not
// pollute the duration histogram. Runs on panic too (it is deferred), so the
// gauges never leak.
func (s *metricsRequestState) finish() {
	if s.upgraded {
		s.metrics.wsActiveSessions.Dec()
		s.metrics.requestBytes.WithLabelValues(s.hostname).Add(float64(s.bodyBytes.Load()))

		return
	}

	s.metrics.requestsInFlight.Dec()
	s.metrics.requestDuration.WithLabelValues(s.hostname).Observe(time.Since(s.start).Seconds())
	s.metrics.requestsTotal.WithLabelValues(s.hostname, statusClass(s.counted.Status())).Inc()
	s.metrics.responseBytes.WithLabelValues(s.hostname).Add(float64(s.counted.BytesWritten()))
	s.metrics.requestBytes.WithLabelValues(s.hostname).Add(float64(s.bodyBytes.Load()))
}

// recordBackendError increments the backend-error counter when metrics are
// enabled; no-op otherwise. Shared by the reverse-proxy error handler and the
// WebSocket upgrade path.
func (h *Handler) recordBackendError(hostname, reason string) {
	if h.metrics == nil {
		return
	}

	h.metrics.backendErrors.WithLabelValues(hostname, reason).Inc()
}

// proxyErrorHandler wraps the package errorHandler with backend-error
// counting. Returns the bare errorHandler when metrics are disabled so the
// disabled path keeps the exact pre-metrics behaviour.
func (h *Handler) proxyErrorHandler(hostname string) func(http.ResponseWriter, *http.Request, error) {
	if h.metrics == nil {
		return errorHandler
	}

	return func(writer http.ResponseWriter, req *http.Request, err error) {
		h.recordBackendError(hostname, classifyBackendError(err))
		errorHandler(writer, req, err)
	}
}

// countingBody wraps a request body to count bytes actually read from the
// client. EVERY error passes through unwrapped — stdlib transports and
// handlers compare read errors by identity (io.EOF, ErrBodyReadAfterClose,
// net sentinels), and instrumentation must stay invisible to them.
type countingBody struct {
	inner   io.ReadCloser
	counter *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	b.counter.Add(int64(n))

	return n, err //nolint:wrapcheck // errors must pass through by identity (see type comment)
}

func (b *countingBody) Close() error {
	err := b.inner.Close()
	if err != nil {
		return fmt.Errorf("counting request body close: %w", err)
	}

	return nil
}

// statusClass maps an HTTP status to its requests_total label class. Zero is
// the counting writer's "no response written" sentinel and 1-99 are
// impossible wire statuses — both land in "aborted"; 100-599
// map to "1xx".."5xx"; anything else (a handler writing a nonsense status)
// lands in "other" rather than minting a new label value.
func statusClass(status int) string {
	switch {
	case status == 0:
		return statusClassAborted
	case status < 100:
		return statusClassAborted
	case status < maxValidHTTPStatus:
		classes := [...]string{statusClassUpgrade, "2xx", "3xx", "4xx", "5xx"}

		return classes[status/100-1]
	default:
		return "other"
	}
}

// classifyBackendError maps a backend round-trip failure onto the closed
// reason set. Order matters: a dial timeout is both an *net.OpError{Op:dial}
// and a Timeout() — "dial" is the more actionable label, so the dial check
// runs before the generic timeout check. Cancellation is checked first
// because a canceled request often wraps secondary network errors.
func classifyBackendError(err error) string {
	if err == nil {
		return backendErrReasonOther
	}

	if errors.Is(err, context.Canceled) {
		// A client mid-request disconnect surfaces here too, so it lands in
		// the "backend errors" family despite not being a backend fault. This
		// is a conscious choice: it gets its OWN "canceled" bucket (never
		// conflated with dial/timeout/tls), so a dashboard can separate client
		// aborts from genuine backend failures without an extra metric.
		return backendErrReasonCanceled
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return backendErrReasonDial
	}

	if isTLSBackendError(err) {
		return backendErrReasonTLS
	}

	var timeoutErr interface{ Timeout() bool }
	if errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &timeoutErr) && timeoutErr.Timeout()) {
		return backendErrReasonTimeout
	}

	return backendErrReasonOther
}

// isTLSBackendError reports whether the error chain carries a TLS handshake /
// verification failure: stdlib record/verification errors, x509 chain errors,
// or this package's BackendTLSPolicy sentinels.
func isTLSBackendError(err error) bool {
	var (
		recordErr    tls.RecordHeaderError
		certErr      x509.CertificateInvalidError
		hostnameErr  x509.HostnameError
		authorityErr x509.UnknownAuthorityError
	)

	return errors.As(err, &recordErr) ||
		errors.As(err, &certErr) ||
		errors.As(err, &hostnameErr) ||
		errors.As(err, &authorityErr) ||
		errors.Is(err, errBackendTLSNoPeerCert) ||
		errors.Is(err, errBackendTLSSANMissing) ||
		errors.Is(err, errBackendTLSChainVerify)
}
