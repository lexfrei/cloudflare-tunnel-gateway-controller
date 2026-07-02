//go:build conformance

package conformance

import (
	"crypto/tls"
	"fmt"
	"net/url"

	"golang.org/x/net/websocket"
	confwebsocket "sigs.k8s.io/gateway-api/conformance/utils/websocket"
)

// TunnelWebSocketDialer is the WebSocket counterpart of TunnelRoundTripper
// (HTTP) and TunnelGRPCClient (gRPC): it satisfies the conformance suite's
// injectable websocket.Dialer interface (upstream gateway-api v1.6.0,
// kubernetes-sigs/gateway-api#4936 / #5003) but connects through the
// Cloudflare edge instead of dialing the Gateway address directly.
//
// The suite hands Dial a ws://<gwAddr>/path URL built from the Gateway
// status address — a *.cfargotunnel.com CNAME whose AAAA records point at
// Cloudflare's ULA (fd10::/8), unroutable from any external test runner. We
// redirect the handshake to the real tunnel hostname
// (CONFORMANCE_TUNNEL_HOSTNAME) over wss on port 443, with TLS SNI set to
// that hostname so Cloudflare routes the connection to our tunnel. The
// handshake's wire Host header must also be the edge hostname — Cloudflare
// edge rejects any Host it does not recognise on the account — so the
// test's intended host travels via X-Original-Host instead, exactly like
// TunnelRoundTripper and TunnelGRPCClient. The in-cluster proxy's
// extractHost already prefers this header over the wire Host/authority for
// both of those paths, and WebSocket upgrades route through the same
// extractHost call before the connection is hijacked.
type TunnelWebSocketDialer struct{}

var _ confwebsocket.Dialer = (*TunnelWebSocketDialer)(nil)

// Dial implements websocket.Dialer.
func (d *TunnelWebSocketDialer) Dial(rawURL, protocol, origin string) (*websocket.Conn, error) {
	config, err := buildEdgeWebSocketConfig(rawURL, protocol, origin, tunnelHostname())
	if err != nil {
		return nil, err
	}

	conn, err := websocket.DialConfig(config)
	if err != nil {
		return nil, fmt.Errorf("dialing tunnel edge websocket: %w", err)
	}

	return conn, nil
}

// buildEdgeWebSocketConfig rewrites the suite-supplied WebSocket URL to
// target the tunnel edge hostname while carrying the test's intended host
// via X-Original-Host, mirroring buildEdgeRequest for HTTP. Path and query
// are preserved from rawURL; origin and the requested subprotocol pass
// through unchanged, since neither is validated against Cloudflare DNS.
func buildEdgeWebSocketConfig(rawURL, protocol, origin, edgeHost string) (*websocket.Config, error) {
	target, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing websocket url: %w", err)
	}

	intendedHost := target.Host

	edgeURL := url.URL{
		Scheme:   "wss",
		Host:     edgeHost,
		Path:     target.Path,
		RawQuery: target.RawQuery,
	}

	config, err := websocket.NewConfig(edgeURL.String(), origin)
	if err != nil {
		return nil, fmt.Errorf("building websocket config: %w", err)
	}

	if protocol != "" {
		config.Protocol = []string{protocol}
	}

	if intendedHost != edgeHost {
		config.Header.Set(originalHostHeader, intendedHost)
	}

	config.TlsConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: edgeHost,
	}

	return config, nil
}
