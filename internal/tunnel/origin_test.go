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

func TestGatewayOriginProxy_ProxyHTTP_IsLBProbeIgnored(t *testing.T) {
	t.Parallel()

	var called atomic.Bool

	handler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called.Store(true)
	})

	proxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://example.com/", nil,
	)
	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newTestResponseWriter()

	// The isWebsocket (third) parameter should be ignored; handler is still called.
	err := proxy.ProxyHTTP(rw, tracedReq, true)

	require.NoError(t, err)
	assert.True(t, called.Load())
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
