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
