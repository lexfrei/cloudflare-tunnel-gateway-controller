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
// connection.ResponseWriter implements http.ResponseWriter, so direct delegation works.
func (p *GatewayOriginProxy) ProxyHTTP(
	writer connection.ResponseWriter,
	tr *tracing.TracedHTTPRequest,
	_ bool,
) error {
	p.handler.ServeHTTP(writer, tr.Request)

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
