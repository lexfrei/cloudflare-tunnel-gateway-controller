package tunnel

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"

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

		// WebSocket hijacks the connection and carries no HTTP trailers; pass
		// the raw cloudflared writer straight through so the delicate 101 +
		// Hijack contract is untouched.
		p.handler.ServeHTTP(writer, req)

		return nil
	}

	// Non-WebSocket requests may carry HTTP trailers (gRPC puts grpc-status
	// there). httputil.ReverseProxy emits trailers via the stdlib
	// http.TrailerPrefix mechanism on the writer's Header() map, but
	// cloudflared's http2RespWriter serializes that map only once at
	// WriteHeader and emits trailers solely via AddTrailer. Bridge the two so
	// gRPC clients receive grpc-status instead of "server closed the stream
	// without sending trailers".
	bridge := newTrailerBridge(writer)
	p.handler.ServeHTTP(bridge, req)
	bridge.flushTrailers()

	return nil
}

// trailerBridge wraps a connection.ResponseWriter and forwards HTTP trailers a
// handler emits via the stdlib mechanism onto cloudflared's AddTrailer, which
// is the only path that puts trailers on the HTTP/2 wire. Response headers
// written before the status are passed straight through; trailers (entries
// keyed with http.TrailerPrefix, plus values for keys announced in the Trailer
// header) are replayed via AddTrailer once the handler returns.
type trailerBridge struct {
	connection.ResponseWriter

	header      http.Header
	announced   map[string]struct{}
	wroteHeader bool
	hijacked    bool
}

func newTrailerBridge(w connection.ResponseWriter) *trailerBridge {
	return &trailerBridge{
		ResponseWriter: w,
		header:         http.Header{},
		announced:      make(map[string]struct{}),
	}
}

func (b *trailerBridge) Header() http.Header { return b.header }

func (b *trailerBridge) WriteHeader(status int) {
	if b.wroteHeader {
		return
	}

	b.wroteHeader = true

	dst := b.ResponseWriter.Header()

	for key, values := range b.header {
		switch {
		case http.CanonicalHeaderKey(key) == "Trailer":
			for _, value := range values {
				for name := range strings.SplitSeq(value, ",") {
					b.announced[http.CanonicalHeaderKey(strings.TrimSpace(name))] = struct{}{}
				}
			}
		case strings.HasPrefix(key, http.TrailerPrefix):
			// Trailer set before the body — replayed in flushTrailers, not as a header.
		default:
			dst[key] = values
		}
	}

	b.ResponseWriter.WriteHeader(status)
}

func (b *trailerBridge) Write(payload []byte) (int, error) {
	if !b.wroteHeader {
		b.WriteHeader(http.StatusOK)
	}

	//nolint:wrapcheck // transparent pass-through to the cloudflared writer.
	return b.ResponseWriter.Write(payload)
}

// Flush re-exposes the underlying writer's flush capability. The
// connection.ResponseWriter interface does not include http.Flusher, so
// embedding alone would hide cloudflared's Flush from httputil.ReverseProxy
// (which keys streaming on it).
func (b *trailerBridge) Flush() {
	if flusher, ok := b.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (b *trailerBridge) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	b.hijacked = true

	//nolint:wrapcheck // transparent pass-through to the cloudflared writer.
	return b.ResponseWriter.Hijack()
}

// flushTrailers replays accumulated trailers via the cloudflared writer's
// AddTrailer. A hijacked or never-written response carries no trailers.
func (b *trailerBridge) flushTrailers() {
	if b.hijacked || !b.wroteHeader {
		return
	}

	for key, values := range b.header {
		name := ""

		switch {
		case strings.HasPrefix(key, http.TrailerPrefix):
			name = strings.TrimPrefix(key, http.TrailerPrefix)
		default:
			if _, ok := b.announced[http.CanonicalHeaderKey(key)]; ok {
				name = key
			}
		}

		if name == "" {
			continue
		}

		for _, value := range values {
			b.AddTrailer(name, value)
		}
	}
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
