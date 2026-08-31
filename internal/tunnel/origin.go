package tunnel

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
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
	// Both branches reach the request through this pointer, including from
	// inside their recovers.
	if tracedReq == nil {
		return errNilTracedRequest
	}

	if isWebsocket {
		return p.proxyUpgrade(writer, tracedReq)
	}

	return p.proxyRequest(writer, tracedReq)
}

// errNilTracedRequest is returned when the connector hands over no request at
// all, which is a caller-contract violation rather than anything to serve.
var errNilTracedRequest = errors.New("no request to proxy")

// errHandlerPanic is returned when a handler panic left the response in a state
// that cannot be answered, so the caller resets the stream instead.
var errHandlerPanic = errors.New("panic in request handler")

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

// Hijack latches only on success. The flag decides whether a later panic can
// still answer with a 500, and cloudflared's HTTP/2 writer refuses a hijack
// before the status is written — treating that refusal as ownership would
// throw away a response that was still writable.
//
// The refusal is reachable, not hypothetical: ReverseProxy hijacks before
// writing a status, which is exactly the precondition that writer rejects. What
// keeps it harmless today is that ReverseProxy writes a status right after a
// refusal, so no panic lands in the window. The latch is what makes that
// ordering irrelevant.
func (b *trailerBridge) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buf, err := b.ResponseWriter.Hijack()
	if err == nil {
		b.hijacked = true
	}

	//nolint:wrapcheck // transparent pass-through to the cloudflared writer.
	return conn, buf, err
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

// proxyUpgrade runs the WebSocket branch.
//
// The raw cloudflared writer is passed straight through so the delicate
// 101 + Hijack contract is untouched. Knowing what that writer has already sent
// would mean wrapping it, which this branch declines to do, so it never decides
// the outcome itself: it returns the error and cloudflared decides. Nothing
// sent yet gets a 502 (WriteErrorResponse on HTTP/2, WriteConnectResponseData
// on QUIC); once the status is out, the stream is reset instead.
func (p *GatewayOriginProxy) proxyUpgrade(
	writer connection.ResponseWriter,
	tracedReq *tracing.TracedHTTPRequest,
) (err error) {
	// Armed before the clone: a panic in Clone or the header rewrite is as
	// fatal to the process as one in the handler, and logging reads the
	// original request, which is available either way. Neither is reachable
	// from cloudflared's request construction today, so the placement keeps the
	// window from opening later rather than closing one that is open.
	defer func() {
		if recovered := recover(); recovered != nil {
			p.logHandlerPanic(tracedReq.Request, recovered)

			err = errHandlerPanic
		}
	}()

	// Clone before mutating: tracedReq.Request may be retained by
	// cloudflared for tracing/logging, and a body-less header copy is
	// what the handshake needs anyway.
	req := tracedReq.Clone(tracedReq.Context())
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.ContentLength = 0
	req.Body = nil

	p.handler.ServeHTTP(writer, req)

	return nil
}

// proxyRequest runs the ordinary HTTP branch.
//
// Requests here may carry HTTP trailers (gRPC puts grpc-status there).
// httputil.ReverseProxy emits trailers via the stdlib http.TrailerPrefix
// mechanism on the writer's Header() map, but cloudflared's http2RespWriter
// serializes that map only once at WriteHeader and emits trailers solely via
// AddTrailer. The bridge joins the two so gRPC clients receive grpc-status
// instead of "server closed the stream without sending trailers".
func (p *GatewayOriginProxy) proxyRequest(
	writer connection.ResponseWriter,
	tracedReq *tracing.TracedHTTPRequest,
) (err error) {
	bridge := newTrailerBridge(writer)

	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		p.logHandlerPanic(tracedReq.Request, recovered)

		// A response that has not started yet can still be answered. Once the
		// status is on the wire, or the handler has taken the connection, the
		// only remaining signal is resetting the stream.
		if bridge.wroteHeader || bridge.hijacked {
			err = errHandlerPanic

			return
		}

		// Drop whatever the handler accumulated before it panicked. Those
		// headers describe the response it was going to send, not this one — a
		// Content-Length among them would promise a body that never arrives.
		clear(bridge.header)
		bridge.WriteHeader(http.StatusInternalServerError)
	}()

	p.handler.ServeHTTP(bridge, tracedReq.Request)
	bridge.flushTrailers()

	return nil
}

// logHandlerPanic records a contained panic. Without a stack the entry is
// almost useless, since the panic value alone rarely names the failing code.
//
// http.ErrAbortHandler is exempt. httputil.ReverseProxy raises it whenever the
// response copy fails — a client that closed mid-download, a backend that reset
// mid-body — and the standard library treats that as routine: net/http and
// x/net/http2 both compare against the sentinel to suppress the stack. Logging
// it here would write one stack trace per aborted download into a log every
// tenant shares, and bury the panics this containment exists to surface.
func (p *GatewayOriginProxy) logHandlerPanic(req *http.Request, recovered any) {
	recoveredErr, isError := recovered.(error)
	if isError && errors.Is(recoveredErr, http.ErrAbortHandler) {
		return
	}

	// A nil request is how Clone panics, which is one of the cases this handler
	// exists to catch. Dereferencing it here would panic inside the recover and
	// take the process down anyway.
	if req == nil || req.URL == nil {
		p.logger.Error("panic in request handler; request failed, proxy kept running",
			"panic", recovered,
			"stack", string(debug.Stack()))

		return
	}

	p.logger.Error("panic in request handler; request failed, proxy kept running",
		"panic", recovered,
		"host", req.Host,
		"path", req.URL.Path,
		"stack", string(debug.Stack()))
}
