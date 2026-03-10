package tunnel_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudflare/cloudflared/connection"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
)

// Compile-time check that GatewayOriginProxy implements connection.OriginProxy.
var _ connection.OriginProxy = (*tunnel.GatewayOriginProxy)(nil)

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

func TestGatewayOriginProxy_ProxyTCP_ReturnsError(t *testing.T) {
	t.Parallel()

	proxy := tunnel.NewGatewayOriginProxy(http.NotFoundHandler(), nil)

	err := proxy.ProxyTCP(context.Background(), nil, &connection.TCPRequest{
		Dest: "localhost:22",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "TCP proxying is not supported")
}

func TestGatewayOriginProxy_NilLogger(t *testing.T) {
	t.Parallel()

	proxy := tunnel.NewGatewayOriginProxy(http.NotFoundHandler(), nil)
	require.NotNil(t, proxy)
}
