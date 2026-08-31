package tunnel_test

import (
	"bufio"
	"bytes"
	"context"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/tracing"

	proxypkg "github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
)

// Compile-time check that GatewayOriginProxy implements connection.OriginProxy.
var _ connection.OriginProxy = (*tunnel.GatewayOriginProxy)(nil)

// testResponseWriter wraps httptest.ResponseRecorder to implement connection.ResponseWriter.
type testResponseWriter struct {
	*httptest.ResponseRecorder

	// trailers records values passed to AddTrailer — the ONLY path that puts
	// trailers on the cloudflared HTTP/2 wire (httptest.ResponseRecorder's own
	// Trailer map is not populated by that call).
	trailers http.Header
}

func (t *testResponseWriter) WriteRespHeaders(status int, header http.Header) error {
	maps.Copy(t.Header(), header)

	t.WriteHeader(status)

	return nil
}

func (t *testResponseWriter) AddTrailer(name, value string) {
	if t.trailers == nil {
		t.trailers = http.Header{}
	}

	t.trailers.Add(name, value)
}

func (t *testResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

// strictStatusWriter counts status writes and refuses a second one.
//
// httptest.ResponseRecorder silently ignores a repeat WriteHeader, which hides
// the production hazard: cloudflared's HTTP/2 writer serializes its header map
// exactly once, so a second status is dropped on the wire and the client keeps
// the first. A recorder-based test would pass either way.
type strictStatusWriter struct {
	*testResponseWriter

	t            *testing.T
	statusWrites int
}

func (s *strictStatusWriter) WriteHeader(status int) {
	s.statusWrites++

	require.LessOrEqual(s.t, s.statusWrites, 1,
		"the cloudflared writer serializes headers once; a second status never reaches the client")

	s.testResponseWriter.WriteHeader(status)
}

func newStrictStatusWriter(t *testing.T) *strictStatusWriter {
	t.Helper()

	return &strictStatusWriter{testResponseWriter: newTestResponseWriter(), t: t}
}

func newTestResponseWriter() *testResponseWriter {
	return &testResponseWriter{ResponseRecorder: httptest.NewRecorder()}
}

func TestGatewayOriginProxy_ProxyHTTP_DelegatesToHandler(t *testing.T) {
	t.Parallel()

	var called atomic.Bool

	var receivedHost string

	var receivedPath string

	handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		called.Store(true)
		receivedHost = req.Host
		receivedPath = req.URL.Path
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	// Test delegation through the exported Handler method.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", nil)
	recorder := httptest.NewRecorder()

	proxy.Handler().ServeHTTP(recorder, req)

	assert.True(t, called.Load())
	assert.Equal(t, "example.com", receivedHost)
	assert.Equal(t, "/test", receivedPath)
}

func TestGatewayOriginProxy_ProxyHTTP_ViaTracedRequest(t *testing.T) {
	t.Parallel()

	var receivedMethod string

	var receivedPath string

	handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		receivedMethod = req.Method
		receivedPath = req.URL.Path
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "http://example.com/api/data", nil,
	)
	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newTestResponseWriter()

	err := proxy.ProxyHTTP(rw, tracedReq, false)

	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, receivedMethod)
	assert.Equal(t, "/api/data", receivedPath)
}

func TestGatewayOriginProxy_ProxyHTTP_WritesResponse(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://example.com/resource", nil,
	)
	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newTestResponseWriter()

	err := proxy.ProxyHTTP(rw, tracedReq, false)

	require.NoError(t, err)
	assert.Equal(t, http.StatusCreated, rw.Code)
	assert.Equal(t, "created", rw.Body.String())
}

// TestGatewayOriginProxy_ProxyHTTP_GRPCNoMatchEmitsUnimplemented pins the
// gRPC half of the no-hostname-match clause (Gateway API v1.6.0, #4408) on the
// PRODUCTION path — the cloudflared HTTP/2 writer via the trailer bridge, NOT
// httptest. A gRPC request that matches no route must reach the client as
// Unimplemented, which requires a trailers-only response carrying
// grpc-status: 12. A bare HTTP 404 (what the handler emits for a non-gRPC
// no-match) reaches a gRPC client as "stream closed without trailers"
// (Internal) over the real tunnel — the exact failure the httptest-only test
// masked.
func TestGatewayOriginProxy_ProxyHTTP_GRPCNoMatchEmitsUnimplemented(t *testing.T) {
	t.Parallel()

	// Empty router → every request is a no-match.
	handler := proxypkg.NewHandler(proxypkg.NewRouter())
	originProxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "http://example.com/pkg.Service/Method", nil,
	)
	req.Header.Set("Content-Type", "application/grpc")

	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newTestResponseWriter()

	err := originProxy.ProxyHTTP(rw, tracedReq, false)
	require.NoError(t, err)

	// gRPC always uses HTTP 200; the real status rides in the trailer.
	assert.Equal(t, http.StatusOK, rw.Code)
	assert.Equal(t, "application/grpc", rw.Header().Get("Content-Type"))
	require.NotNil(t, rw.trailers, "a gRPC no-match must emit a grpc-status trailer")
	assert.Equal(t, "12", rw.trailers.Get("Grpc-Status"),
		"a gRPC request matching no route must surface as Unimplemented (12)")
}

// TestGatewayOriginProxy_ProxyHTTP_HTTPNoMatchStays404 confirms the non-gRPC
// no-match path is unchanged: a plain HTTP request still gets a bare 404, not
// the gRPC trailers-only shape.
func TestGatewayOriginProxy_ProxyHTTP_HTTPNoMatchStays404(t *testing.T) {
	t.Parallel()

	handler := proxypkg.NewHandler(proxypkg.NewRouter())
	originProxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://example.com/nope", nil,
	)

	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newTestResponseWriter()

	err := originProxy.ProxyHTTP(rw, tracedReq, false)
	require.NoError(t, err)

	assert.Equal(t, http.StatusNotFound, rw.Code)
	assert.Nil(t, rw.trailers, "a plain HTTP no-match must not emit grpc trailers")
}

// TestGatewayOriginProxy_ProxyHTTP_WebSocketReinjectsHeaders pins the bridge
// contract between cloudflared and our L7 handler for WebSocket traffic.
//
// cloudflared (HTTP/2 connection) strips the standard HTTP/1.1 upgrade
// headers from the request before invoking OriginProxy.ProxyHTTP — the
// upgrade is signalled instead via the third (`isWebsocket bool`) parameter.
// Native cloudflared re-injects `Connection: Upgrade`, `Upgrade: websocket`,
// and `Sec-WebSocket-Version: 13` before forwarding to origin (see
// vendor/github.com/cloudflare/cloudflared/proxy/proxy.go:proxyHTTPRequest).
//
// Our L7 handler routes the request through the custom
// `proxyWebSocketUpgrade` path (not `httputil.ReverseProxy`) when it sees
// those same RFC 7230 §6.1 headers via `isHTTPUpgradeRequest`. Without
// re-injection here the handler sees a regular HTTP request, forwards it
// without upgrade headers, and the backend returns 400 "not websocket
// protocol". This pin captures the fix so a future revert breaks loudly
// with a deterministic mock instead of an opaque e2e failure.
func TestGatewayOriginProxy_ProxyHTTP_WebSocketReinjectsHeaders(t *testing.T) {
	t.Parallel()

	var (
		gotConnection string
		gotUpgrade    string
		gotWSVersion  string
	)

	handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		gotConnection = req.Header.Get("Connection")
		gotUpgrade = req.Header.Get("Upgrade")
		gotWSVersion = req.Header.Get("Sec-WebSocket-Version")
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://example.com/ws", nil,
	)
	// cloudflared already stripped these by the time ProxyHTTP runs.
	req.Header.Del("Connection")
	req.Header.Del("Upgrade")

	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newTestResponseWriter()

	err := proxy.ProxyHTTP(rw, tracedReq, true)

	require.NoError(t, err)
	assert.Equal(t, "Upgrade", gotConnection,
		"isWebsocket=true MUST re-inject Connection: Upgrade so the handler's "+
			"isHTTPUpgradeRequest predicate fires and routes the request through the "+
			"custom proxyWebSocketUpgrade path that hijacks on 101")
	assert.Equal(t, "websocket", gotUpgrade,
		"isWebsocket=true MUST re-inject Upgrade: websocket so the backend "+
			"completes the RFC 6455 handshake instead of returning 400")
	assert.Equal(t, "13", gotWSVersion,
		"Sec-WebSocket-Version: 13 is the only WebSocket version this proxy "+
			"path supports; native cloudflared pins it the same way")
}

// TestGatewayOriginProxy_ProxyHTTP_NonWebSocketLeavesHeadersAlone is the
// negative pin: a regular HTTP request must NOT acquire WebSocket upgrade
// headers from the bridge, otherwise plain HTTP backends would interpret
// every routed request as an upgrade attempt.
func TestGatewayOriginProxy_ProxyHTTP_NonWebSocketLeavesHeadersAlone(t *testing.T) {
	t.Parallel()

	var (
		gotConnection string
		gotUpgrade    string
	)

	handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		gotConnection = req.Header.Get("Connection")
		gotUpgrade = req.Header.Get("Upgrade")
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://example.com/", nil,
	)
	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newTestResponseWriter()

	err := proxy.ProxyHTTP(rw, tracedReq, false)

	require.NoError(t, err)
	assert.Empty(t, gotConnection, "non-websocket request must not gain a Connection: Upgrade header")
	assert.Empty(t, gotUpgrade, "non-websocket request must not gain an Upgrade header")
}

// trailerContractWriter faithfully models cloudflared's http2RespWriter trailer
// contract (vendor/.../connection/http2.go), which the httptest.ResponseRecorder
// behind testResponseWriter does NOT: Header() returns a private map that is
// serialized into the response exactly once at WriteHeader time, so anything
// written to it afterwards (e.g. httputil.ReverseProxy's stdlib http.TrailerPrefix
// trailers) is dropped. Trailers reach the wire ONLY via AddTrailer. A gRPC
// response carries grpc-status in trailers, so without bridging stdlib trailers
// onto AddTrailer the client sees "server closed the stream without sending
// trailers".
type trailerContractWriter struct {
	header   http.Header
	wire     http.Header
	trailers http.Header
	body     bytes.Buffer
	status   int
	written  bool
}

func newTrailerContractWriter() *trailerContractWriter {
	return &trailerContractWriter{header: http.Header{}, wire: http.Header{}, trailers: http.Header{}}
}

func (w *trailerContractWriter) Header() http.Header { return w.header }

func (w *trailerContractWriter) WriteHeader(status int) {
	if w.written {
		return
	}

	w.written = true
	w.status = status

	// Snapshot the header map once — mirrors WriteRespHeaders serializing the
	// response headers a single time. Post-WriteHeader header mutations (where
	// ReverseProxy stashes unannounced trailers) never reach the wire.
	for k, v := range w.header {
		w.wire[k] = append([]string(nil), v...)
	}
}

func (w *trailerContractWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}

	// bytes.Buffer.Write never returns an error.
	n, _ := w.body.Write(b)

	return n, nil
}

func (w *trailerContractWriter) WriteRespHeaders(status int, header http.Header) error {
	maps.Copy(w.header, header)
	w.WriteHeader(status)

	return nil
}

// AddTrailer is the ONLY path that puts a trailer on the wire — exactly like
// http2RespWriter, and it is ignored before the status is written.
func (w *trailerContractWriter) AddTrailer(name, value string) {
	if !w.written {
		return
	}

	w.trailers.Add(name, value)
}

func (w *trailerContractWriter) Flush() {}

func (w *trailerContractWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

// TestGatewayOriginProxy_ProxyHTTP_ForwardsGRPCTrailers pins the production
// hazard the e2e suite surfaced: gRPC carries grpc-status in HTTP/2 trailers,
// httputil.ReverseProxy emits them via the stdlib http.TrailerPrefix mechanism
// onto the response writer's Header() map, but cloudflared's http2RespWriter
// only serializes that map once (at WriteHeader) and emits trailers solely via
// AddTrailer. ProxyHTTP must bridge the two, or the gRPC client fails with
// "server closed the stream without sending trailers". This uses a faithful
// cloudflared-contract writer (not httptest) per the proxy design principle.
func TestGatewayOriginProxy_ProxyHTTP_ForwardsGRPCTrailers(t *testing.T) {
	t.Parallel()

	// Backend emits an unannounced trailer the way a gRPC origin does: declared
	// only via the http.TrailerPrefix mechanism after the body.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "0")
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", "")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("\x00\x00\x00\x00\x00"))
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	handler := httputil.NewSingleHostReverseProxy(backendURL)
	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "http://example.com/pkg.Svc/Method", http.NoBody,
	)
	req.Header.Set("Content-Type", "application/grpc")

	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newTrailerContractWriter()

	require.NoError(t, proxy.ProxyHTTP(rw, tracedReq, false))

	assert.Equal(t, "0", rw.trailers.Get("Grpc-Status"),
		"grpc-status trailer must reach the wire via AddTrailer; ReverseProxy's "+
			"stdlib-trailer write to Header() is dropped by the cloudflared writer")
}

// TestGatewayOriginProxy_ProxyHTTP_ForwardsAnnouncedTrailers covers the other
// ReverseProxy trailer path: a backend that pre-declares its trailers in the
// Trailer header. ReverseProxy then sets the trailer value as a plain
// (non-prefixed) header after the body, so the bridge must recognize it via the
// announced-trailer set and replay it through AddTrailer — without leaking it as
// a regular wire header.
func TestGatewayOriginProxy_ProxyHTTP_ForwardsAnnouncedTrailers(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("\x00\x00\x00\x00\x00"))
		// Announced trailer value, set after the body per the stdlib contract.
		w.Header().Set("Grpc-Status", "0")
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)

	handler := httputil.NewSingleHostReverseProxy(backendURL)
	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "http://example.com/pkg.Svc/Method", http.NoBody,
	)
	req.Header.Set("Content-Type", "application/grpc")

	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newTrailerContractWriter()

	require.NoError(t, proxy.ProxyHTTP(rw, tracedReq, false))

	assert.Equal(t, "0", rw.trailers.Get("Grpc-Status"),
		"announced grpc-status trailer must reach the wire via AddTrailer")
	assert.Empty(t, rw.wire.Get("Grpc-Status"),
		"an announced trailer must not be duplicated as a serialized response header")
}

func TestGatewayOriginProxy_ProxyTCP_ReturnsError(t *testing.T) {
	t.Parallel()

	proxy := tunnel.NewGatewayOriginProxy(http.NotFoundHandler(), nil)

	err := proxy.ProxyTCP(context.Background(), nil, &connection.TCPRequest{
		Dest: "localhost:22",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "TCP proxying is not supported")
}

func TestGatewayOriginProxy_ProxyTCP_NilRequest(t *testing.T) {
	t.Parallel()

	proxy := tunnel.NewGatewayOriginProxy(http.NotFoundHandler(), nil)

	err := proxy.ProxyTCP(context.Background(), nil, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "TCP proxying is not supported")
}

func TestGatewayOriginProxy_NilLogger(t *testing.T) {
	t.Parallel()

	proxy := tunnel.NewGatewayOriginProxy(http.NotFoundHandler(), nil)
	require.NotNil(t, proxy)
	assert.NotNil(t, proxy.Handler())
}

func TestGatewayOriginProxy_WithExplicitLogger(t *testing.T) {
	t.Parallel()

	logger := slog.Default()
	proxy := tunnel.NewGatewayOriginProxy(http.NotFoundHandler(), logger)

	require.NotNil(t, proxy)
	assert.NotNil(t, proxy.Handler())
}

func TestGatewayOriginProxy_HandlerPreserved(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://example.com/", nil,
	)
	recorder := httptest.NewRecorder()

	proxy.Handler().ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusTeapot, recorder.Code)
}

// TestGatewayOriginProxy_ProxyHTTP_ContainsHandlerPanic pins panic containment
// at the tunnel entry point.
//
// Nothing recovers between cloudflared's per-stream goroutine and this method
// on the QUIC chain, and `protocol: auto` dials QUIC first, so a panic in the
// handler takes the whole proxy process down and every other tenant's traffic
// with it. Containment turns one request's bug into one failed request.
func TestGatewayOriginProxy_ProxyHTTP_ContainsHandlerPanic(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Set before panicking: these headers describe a response that never
		// happens, and a Content-Length among them would promise a body.
		w.Header().Set("Content-Length", "42")
		w.Header().Set("X-From-Handler", "yes")

		panic("handler exploded")
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/boom", nil)
	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	writer := newTestResponseWriter()

	require.NotPanics(t, func() {
		_ = proxy.ProxyHTTP(writer, tracedReq, false)
	}, "a panicking handler must not escape into cloudflared's stream goroutine")

	assert.Equal(t, http.StatusInternalServerError, writer.Code,
		"a contained panic must still answer the request")
	assert.Empty(t, writer.Header().Get("Content-Length"),
		"the 500 must not inherit a body length the panicking handler promised")
	assert.Empty(t, writer.Header().Get("X-From-Handler"),
		"headers staged for a response that never happened must not ship with the 500")
}

// TestGatewayOriginProxy_ProxyHTTP_ContainsWebSocketHandlerPanic pins the same
// contract on the upgrade branch, which hands cloudflared's writer straight to
// the handler and never goes through the trailer bridge.
func TestGatewayOriginProxy_ProxyHTTP_ContainsWebSocketHandlerPanic(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("upgrade handler exploded")
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/ws", nil)
	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	writer := newTestResponseWriter()

	var err error

	require.NotPanics(t, func() {
		err = proxy.ProxyHTTP(writer, tracedReq, true)
	}, "a panicking upgrade handler must not escape into cloudflared's stream goroutine")

	require.Error(t, err,
		"the upgrade branch cannot answer on its own, so the outcome is handed back to cloudflared")
}

// TestGatewayOriginProxy_ProxyHTTP_PanicAfterStatusResetsTheStream pins the
// other half of the containment decision.
//
// Once the status is on the wire there is no way to turn the response into a
// 500, so the only signal left is resetting the stream — cloudflared turns the
// returned error into an aborted HTTP/2 handler or a QUIC RST_STREAM. Writing a
// second status here would be worse than useless: the client already has the
// first one, and the trailer bridge would drop the write silently.
func TestGatewayOriginProxy_ProxyHTTP_PanicAfterStatusResetsTheStream(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))

		panic("handler exploded mid-response")
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/late", nil)
	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	writer := newStrictStatusWriter(t)

	var err error

	require.NotPanics(t, func() {
		err = proxy.ProxyHTTP(writer, tracedReq, false)
	})

	require.Error(t, err, "a panic that cannot be answered must reset the stream")
	assert.Equal(t, http.StatusOK, writer.Code,
		"the status already sent to the client must not be rewritten")
	assert.Equal(t, 1, writer.statusWrites,
		"the panic path must not attempt a second status write")
}

// TestGatewayOriginProxy_ProxyHTTP_AbortHandlerIsNotLoggedAsAPanic pins that a
// routine client abort stays quiet.
//
// httputil.ReverseProxy panics with http.ErrAbortHandler whenever the response
// copy fails — a client that closed mid-download, a backend that reset. The
// standard library treats that as ordinary and deliberately suppresses the
// stack; net/http and x/net/http2 both special-case the sentinel. Logging it
// would put a stack trace into a shared data plane's log for every aborted
// download and bury the panics this containment exists to surface.
//
// The stream is still reset: the response has started, so there is nothing left
// to answer with either way.
func TestGatewayOriginProxy_ProxyHTTP_AbortHandlerIsNotLoggedAsAPanic(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))

		panic(http.ErrAbortHandler)
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, slog.New(slog.NewTextHandler(&logged, nil)))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/abort", nil)
	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	writer := newTestResponseWriter()

	var err error

	require.NotPanics(t, func() {
		err = proxy.ProxyHTTP(writer, tracedReq, false)
	})

	require.Error(t, err, "an aborted response still cannot be answered, so the stream resets")
	assert.Empty(t, logged.String(),
		"a client abort is routine and must not be logged as a panic")
}

// TestGatewayOriginProxy_ProxyHTTP_RealPanicIsLogged is the other half: the
// abort exemption must not swallow the panics worth reading.
func TestGatewayOriginProxy_ProxyHTTP_RealPanicIsLogged(t *testing.T) {
	t.Parallel()

	var logged bytes.Buffer

	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("nil map write somewhere in a filter")
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, slog.New(slog.NewTextHandler(&logged, nil)))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/real", nil)
	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)

	require.NotPanics(t, func() {
		_ = proxy.ProxyHTTP(newTestResponseWriter(), tracedReq, false)
	})

	assert.Contains(t, logged.String(), "nil map write somewhere in a filter",
		"a genuine panic must still reach the operator")
}

// TestGatewayOriginProxy_ProxyHTTP_RefusedHijackStillAnswers pins that a
// hijack the writer refused does not cost the response.
//
// cloudflared's HTTP/2 writer rejects a hijack before the status is written,
// and httputil.ReverseProxy hijacks before writing one, so the refusal is
// reachable. The bridge must not read it as the handler having taken the
// connection: nothing went out, so a panic afterwards can still be answered.
func TestGatewayOriginProxy_ProxyHTTP_RefusedHijackStillAnswers(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		require.True(t, ok, "the bridge must expose Hijack for the upgrade path")

		_, _, hijackErr := hijacker.Hijack()
		require.Error(t, hijackErr, "the test writer refuses a hijack, like the HTTP/2 writer before a status")

		panic("panicked after a refused hijack")
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/refused", nil)
	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	writer := newTestResponseWriter()

	var err error

	require.NotPanics(t, func() {
		err = proxy.ProxyHTTP(writer, tracedReq, false)
	})

	require.NoError(t, err, "a refused hijack left the response writable, so the panic must be answered")
	assert.Equal(t, http.StatusInternalServerError, writer.Code)
}

// hijackableWriter succeeds at Hijack, like cloudflared's QUIC adapter.
//
// The HTTP/2 writer refuses a hijack before the status is written; the QUIC one
// has no such precondition and always succeeds. Every other writer here refuses,
// so without this shape the success half of the hijack latch is unreachable in
// tests while being the default transport in production.
type hijackableWriter struct {
	*strictStatusWriter
}

func (h *hijackableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, _ := net.Pipe()

	return conn, bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn)), nil
}

// TestGatewayOriginProxy_ProxyHTTP_HijackedResponseIsNotAnswered pins the other
// half of the containment decision: a connection the handler has taken is not
// ours to write to.
//
// ReverseProxy hijacks before writing any status, so on QUIC a panic can land
// with the connection handed over and no status sent. Answering it there would
// push response headers into a stream that has already switched protocols.
func TestGatewayOriginProxy_ProxyHTTP_HijackedResponseIsNotAnswered(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		require.True(t, ok)

		conn, _, hijackErr := hijacker.Hijack()
		require.NoError(t, hijackErr, "this writer models the QUIC adapter, which always succeeds")

		t.Cleanup(func() { _ = conn.Close() })

		panic("panicked after taking the connection")
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/hijacked", nil)
	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	writer := &hijackableWriter{strictStatusWriter: newStrictStatusWriter(t)}

	var err error

	require.NotPanics(t, func() {
		err = proxy.ProxyHTTP(writer, tracedReq, false)
	})

	require.Error(t, err, "a hijacked response cannot be answered, so the panic resets the stream")
	assert.Zero(t, writer.statusWrites,
		"nothing may be written to a connection the handler already took over")
}

// TestGatewayOriginProxy_ProxyHTTP_PanicHandlerSurvivesANilRequest pins that
// the recover cannot become the crash.
//
// A nil Request is how the clone in the upgrade branch panics, which is one of
// the cases the recover was armed early to catch. Reading req.Host to log it
// would then panic inside the panic handler and take the process down for the
// exact input the guard exists for.
func TestGatewayOriginProxy_ProxyHTTP_PanicHandlerSurvivesANilRequest(t *testing.T) {
	t.Parallel()

	// Reads the request, so the nil reaches the recover on the plain branch the
	// same way the clone raises it on the upgrade branch.
	proxy := tunnel.NewGatewayOriginProxy(
		http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) { _ = req.Host }), nil)

	var upgradeErr error

	require.NotPanics(t, func() {
		upgradeErr = proxy.ProxyHTTP(newTestResponseWriter(), &tracing.TracedHTTPRequest{}, true)
	}, "the recover must contain a panic whose request is nil, not add a second one")

	require.Error(t, upgradeErr, "an upgrade has nothing left to answer with")

	plain := newTestResponseWriter()

	var plainErr error

	require.NotPanics(t, func() {
		plainErr = proxy.ProxyHTTP(plain, &tracing.TracedHTTPRequest{}, false)
	}, "the recover must contain a panic whose request is nil, not add a second one")

	require.NoError(t, plainErr, "nothing was written yet, so the plain branch answers instead")
	assert.Equal(t, http.StatusInternalServerError, plain.Code)
}

// TestGatewayOriginProxy_ProxyHTTP_NilTracedRequestIsRefused pins the boundary
// check. Both branches reach the request through that pointer, including from
// inside their recovers, so a nil there would panic in the one place that must
// not.
func TestGatewayOriginProxy_ProxyHTTP_NilTracedRequestIsRefused(t *testing.T) {
	t.Parallel()

	proxy := tunnel.NewGatewayOriginProxy(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil)

	for _, isWebsocket := range []bool{true, false} {
		var err error

		require.NotPanics(t, func() {
			err = proxy.ProxyHTTP(newTestResponseWriter(), nil, isWebsocket)
		}, "websocket=%v", isWebsocket)

		require.Error(t, err, "websocket=%v", isWebsocket)
	}
}

// TestGatewayOriginProxy_ProxyHTTP_RefusedHijackStillFlushesTrailers pins a
// side effect of latching the hijack flag on success only.
//
// flushTrailers skips its work when the flag is set. While a refused hijack
// latched it too, a handler that went on to stage trailers would have had them
// dropped, grpc-status included. No handler does that today — ReverseProxy
// hands a refused hijack to its error handler and returns — so this closes the
// window rather than recovering a loss anyone was taking.
func TestGatewayOriginProxy_ProxyHTTP_RefusedHijackStillFlushesTrailers(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _, hijackErr := w.(http.Hijacker).Hijack()
		require.Error(t, hijackErr, "the test writer refuses a hijack")

		w.Header().Set(http.TrailerPrefix+"grpc-status", "0")
		w.WriteHeader(http.StatusOK)
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com/svc/method", nil)
	zlog := zerolog.Nop()
	writer := newTestResponseWriter()

	require.NoError(t, proxy.ProxyHTTP(writer, tracing.NewTracedHTTPRequest(req, 0, &zlog), false))

	assert.Equal(t, "0", writer.trailers.Get("Grpc-Status"),
		"a refused hijack left the response ours, so its trailers must still reach the wire")
}
