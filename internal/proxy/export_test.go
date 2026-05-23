package proxy

import (
	"net"
	"net/http"
	"sync"
)

// Transports returns the handler's internal transport pool for testing purposes.
func (h *Handler) Transports() *sync.Map {
	return &h.transports
}

// TransportKey exposes the per-host+protocol pool key for testing purposes.
func TransportKey(host string, protocol BackendProtocol) string {
	return transportKey(host, protocol)
}

// NewTransportForTest exposes newTransport for testing purposes.
func NewTransportForTest(protocol BackendProtocol) http.RoundTripper {
	return newTransport(protocol)
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
