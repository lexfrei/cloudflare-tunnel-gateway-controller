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

// NewH2CDialerForTest exposes newH2CDialer so tests can assert the dialer's
// Timeout/KeepAlive fields without reaching inside the http2.Transport closure.
func NewH2CDialerForTest() *net.Dialer {
	return newH2CDialer()
}
