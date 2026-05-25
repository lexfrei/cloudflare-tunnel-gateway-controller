package tunnel_test

import (
	"bufio"
	"context"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/tracing"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
)

// Compile-time check that GatewayOriginProxy implements connection.OriginProxy.
var _ connection.OriginProxy = (*tunnel.GatewayOriginProxy)(nil)

// testResponseWriter wraps httptest.ResponseRecorder to implement connection.ResponseWriter.
type testResponseWriter struct {
	*httptest.ResponseRecorder
}

func (t *testResponseWriter) WriteRespHeaders(status int, header http.Header) error {
	maps.Copy(t.Header(), header)

	t.WriteHeader(status)

	return nil
}

func (t *testResponseWriter) AddTrailer(_, _ string) {}

func (t *testResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
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

// TestGatewayOriginProxy_ProxyHTTP_WebSocketReinjectsHeaders pins the bridge
// contract between cloudflared and our L7 handler for WebSocket traffic.
//
// cloudflared (HTTP/2 connection) strips the standard HTTP/1.1 upgrade
// headers from the request before invoking OriginProxy.ProxyHTTP — the
// upgrade is signalled instead via the third (`isWebsocket bool`) parameter.
// Native cloudflared re-injects `Connection: Upgrade`, `Upgrade: websocket`,
// and `Sec-Websocket-Version: 13` before forwarding to origin (see
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
		gotWSVersion = req.Header.Get("Sec-Websocket-Version")
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
		"Sec-Websocket-Version: 13 is the only WebSocket version this proxy "+
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
