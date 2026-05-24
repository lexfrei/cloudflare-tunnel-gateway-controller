package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"sync"
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

// TransportKey exposes the per-host+protocol+tls pool key for testing purposes.
func TransportKey(host string, protocol BackendProtocol, backendTLS *BackendTLSConfig) string {
	return transportKey(host, protocol, backendTLS)
}

// NewTransportForTest exposes newTransport for testing purposes.
func NewTransportForTest(protocol BackendProtocol, backendTLS *BackendTLSConfig) http.RoundTripper {
	return newTransport(protocol, backendTLS)
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

// IsHTTPUpgradeRequestForTest exposes isHTTPUpgradeRequest so its RFC 7230
// token-parsing edge cases can be pinned by direct unit tests instead of
// relying on the WS integration test as the only proof of correctness.
func IsHTTPUpgradeRequestForTest(req *http.Request) bool {
	return isHTTPUpgradeRequest(req)
}
