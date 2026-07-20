package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"sync"
	"time"
)

// BuildBackendTLSConfigForTest exposes buildBackendTLSConfig so attach-client-cert
// tests can drive it directly with a precomputed root pool.
func BuildBackendTLSConfigForTest(backendTLS *BackendTLSConfig, rootCAs *x509.CertPool) *tls.Config {
	return buildBackendTLSConfig(backendTLS, rootCAs)
}

// Transports returns the handler's internal transport pool for testing purposes.
func (h *Handler) Transports() *sync.Map {
	return &h.transports
}

// EffectiveWSDialTimeoutForTest exposes the Handler's resolved WebSocket
// dial timeout so unit tests can pin that WithWSDialTimeout overrides
// flow through correctly and that the zero-value fallback returns the
// package default.
func (h *Handler) EffectiveWSDialTimeoutForTest() time.Duration {
	return h.effectiveWSDialTimeout()
}

// EffectiveWSHandshakeReadTimeoutForTest exposes the Handler's resolved
// WebSocket handshake read timeout for the same reason.
func (h *Handler) EffectiveWSHandshakeReadTimeoutForTest() time.Duration {
	return h.effectiveWSHandshakeReadTimeout()
}

// TransportKey exposes the per-host+protocol+tls+headerTimeout pool key
// for testing purposes. Existing tests that predate the per-rule timeout
// dimension pass 0 to mean "no header deadline" and behave exactly as
// before.
func TransportKey(host string, protocol BackendProtocol, backendTLS *BackendTLSConfig) string {
	return transportKey(host, protocol, backendTLS, 0)
}

// TransportKeyWithTimeout exposes transportKey with an explicit header
// timeout so timeout-dimensional tests can assert that two routes with
// different per-rule timeouts get distinct keys.
func TransportKeyWithTimeout(host string, protocol BackendProtocol, backendTLS *BackendTLSConfig, headerTimeout time.Duration) string {
	return transportKey(host, protocol, backendTLS, headerTimeout)
}

// NewTransportForTest exposes newTransport for testing purposes. Existing
// callers receive a no-timeout transport (matching the pre-fix shape);
// timeout-dimensional tests can use NewTransportForTestWithTimeout.
func NewTransportForTest(protocol BackendProtocol, backendTLS *BackendTLSConfig) http.RoundTripper {
	return newTransport(protocol, backendTLS, 0)
}

// NewTransportForTestWithTimeout exposes newTransport with the
// ResponseHeaderTimeout knob so per-rule-timeout tests can assert on
// the resulting transport's response-header deadline.
func NewTransportForTestWithTimeout(protocol BackendProtocol, backendTLS *BackendTLSConfig, headerTimeout time.Duration) http.RoundTripper {
	return newTransport(protocol, backendTLS, headerTimeout)
}

// ShouldMirrorForTest exposes the unexported requestMirror.shouldMirror
// decision so its contract can be pinned by direct unit tests rather than the
// statistical integration test.
func ShouldMirrorForTest(percent *int32) bool {
	return (&requestMirror{percent: percent}).shouldMirror()
}

// NewH2CDialerForTest exposes newH2CDialer so tests can assert the dialer's
// Timeout/KeepAlive fields without reaching inside the http2.Transport closure.
func NewH2CDialerForTest() *net.Dialer {
	return newH2CDialer()
}

// NewHeaderTimeoutRoundTripperForTest exposes newHeaderTimeoutRoundTripper so
// the wrapper's streaming-response contract can be pinned directly without
// driving the full http2 backend stack.
func NewHeaderTimeoutRoundTripperForTest(inner http.RoundTripper, timeout time.Duration) http.RoundTripper {
	return newHeaderTimeoutRoundTripper(inner, timeout)
}

// CountingResponseWriterForTest is the exported alias of the concrete
// counting-response-writer type. Tests use it directly so they can
// call the inspection methods (Status, BytesWritten) without losing
// the type to an http.ResponseWriter interface return.
type CountingResponseWriterForTest = countingResponseWriter

// NewCountingResponseWriterForTest exposes newCountingResponseWriter so
// access-log writer-wrapping contracts can be pinned directly without
// driving the full handler stack. Returned as the concrete pointer
// type so Status / BytesWritten are callable; the type still
// satisfies http.ResponseWriter / Flusher / Hijacker for the
// interface-assertion test paths.
func NewCountingResponseWriterForTest(inner http.ResponseWriter) *CountingResponseWriterForTest {
	return newCountingResponseWriter(inner)
}

// ShouldSampleAccessLogForTest exposes shouldSampleAccessLog so the
// sampling contract (errors-always-logged, rate clamping, deterministic
// rand injection) can be pinned without spinning up the full handler.
func ShouldSampleAccessLogForTest(rate float64, status int, randFn func() float64) bool {
	return shouldSampleAccessLog(rate, status, randFn)
}

// ErrHijackNotSupportedForTest exposes errHijackNotSupported so the
// non-Hijacker-fallback test in the _test package can assert on the
// same sentinel value the production wrapper returns. Without an
// exported handle the test would have to redeclare its own copy, and
// errors.Is would never match (different package-level variables
// hash to different identities even with identical messages).
var ErrHijackNotSupportedForTest = errHijackNotSupported

// MaybeEmitAccessLogForTest exposes Handler.maybeEmitAccessLog so
// the status-101 skip carve-out can be pinned without driving the
// full ServeHTTP pipeline. httputil.ReverseProxy translates a
// backend 101 to a client-visible 502, so a normal ServeHTTP call
// can't actually reach maybeEmitAccessLog with status=101; this
// helper bypasses that translation by letting the test seed the
// wrapped writer directly. The snapshot is synthesised from the
// request so callers don't have to construct accessLogSnapshot
// themselves -- the snapshot type stays unexported.
func MaybeEmitAccessLogForTest(h *Handler, counted *CountingResponseWriterForTest, req *http.Request, start time.Time) {
	h.maybeEmitAccessLog(counted, req, &accessLogSnapshot{
		method:    req.Method,
		host:      req.Host,
		path:      req.URL.Path,
		query:     req.URL.RawQuery,
		userAgent: req.UserAgent(),
	}, start)
}

// ErrorHandlerForTest exposes errorHandler so unit tests can pin its
// HTTP-status mapping for every error class it recognises without
// having to spin up a real backend that produces the right error
// shape (dial timeouts and DNS timeouts in particular are awkward to
// reproduce deterministically through httptest).
func ErrorHandlerForTest(writer http.ResponseWriter, req *http.Request, err error) {
	errorHandler(writer, req, err)
}

// ExtractActiveTransportKeysForTest exposes extractActiveTransportKeys
// so the router-side derivation of the transport cache key can be
// pinned directly. Without this hook the per-rule-timeout dimension
// of the cache key would only be observable through the full
// SetHandler → PruneTransports flow, where a missed eviction
// silently leaks a stale entry instead of failing a test.
func ExtractActiveTransportKeysForTest(cfg *Config) map[string]bool {
	return extractActiveTransportKeys(cfg)
}

// IsHTTPUpgradeRequestForTest exposes isHTTPUpgradeRequest so its RFC 7230
// token-parsing edge cases can be pinned by direct unit tests instead of
// relying on the WS integration test as the only proof of correctness.
func IsHTTPUpgradeRequestForTest(req *http.Request) bool {
	return isHTTPUpgradeRequest(req)
}

// SetConfigVersionCounterForTest pins the package-level monotonic config
// version counter to version and returns its previous value so the caller can
// restore it. The pusher's 409-recovery compares a replica's reported version
// against this counter to tell a controller restart (replica version strictly
// ABOVE the counter → clock skew → force-bump and retry) from a lost race to a
// concurrent same-process pusher (replica version at or below → abandon the
// push). Tests that pin the counter to exercise the restart-skew branch MUST
// run sequentially (no t.Parallel) because the counter is process-global.
func SetConfigVersionCounterForTest(version int64) int64 {
	return configVersionCounter.Swap(version)
}
