package tunnel

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/cockroachdb/errors"

	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/tracing"
)

var errTCPNotSupported = errors.New("TCP proxying is not supported")

// GatewayOriginProxy implements connection.OriginProxy.
// It delegates all HTTP requests to the provided http.Handler (our L7 proxy router).
// TCP proxying is not supported (future: TCPRoute).
type GatewayOriginProxy struct {
	handler http.Handler
	logger  *slog.Logger
}

// NewGatewayOriginProxy creates an OriginProxy that delegates to the given handler.
func NewGatewayOriginProxy(handler http.Handler, logger *slog.Logger) *GatewayOriginProxy {
	if logger == nil {
		logger = slog.Default()
	}

	return &GatewayOriginProxy{
		handler: handler,
		logger:  logger,
	}
}

// ProxyHTTP delegates the HTTP request to our L7 proxy handler.
// connection.ResponseWriter implements http.ResponseWriter, so direct
// delegation works for plain HTTP.
//
// When cloudflared signals a WebSocket upgrade via the third parameter, it
// has already stripped the standard HTTP/1.1 upgrade headers from `tr
// .Request`; the upgrade is communicated out-of-band so the request can
// traverse cloudflared's HTTP/2 transport (which forbids hop-by-hop
// headers per RFC 7540 §8.1.2.2). Native cloudflared re-injects them
// before forwarding to origin (see cloudflared/proxy/proxy.go
// `proxyHTTPRequest`). Our `httputil.ReverseProxy`-based handler keys its
// 101-hijack path on those same RFC 7230 §6.1 headers, so re-injecting
// them here is what turns the bridge into a functional WebSocket origin.
//
// Without re-injection the handler sees a regular HTTP request, forwards
// it without upgrade headers, and the backend rejects with 400 "not
// websocket protocol".
func (p *GatewayOriginProxy) ProxyHTTP(
	writer connection.ResponseWriter,
	tracedReq *tracing.TracedHTTPRequest,
	isWebsocket bool,
) error {
	req := tracedReq.Request

	if isWebsocket {
		// Clone before mutating: tracedReq.Request may be retained by
		// cloudflared for tracing/logging, and a body-less header copy is
		// what the handshake needs anyway.
		req = tracedReq.Clone(tracedReq.Context())
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-Websocket-Version", "13")
		req.ContentLength = 0
		req.Body = nil
	}

	p.handler.ServeHTTP(writer, req)

	return nil
}

// Handler returns the underlying http.Handler for testing purposes.
func (p *GatewayOriginProxy) Handler() http.Handler {
	return p.handler
}

// ProxyTCP rejects TCP connections. TCPRoute support is future work.
func (p *GatewayOriginProxy) ProxyTCP(
	_ context.Context,
	_ connection.ReadWriteAcker,
	_ *connection.TCPRequest,
) error {
	return errTCPNotSupported
}
