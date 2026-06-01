package tunnel_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudflare/cloudflared/tracing"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
)

// TestGatewayOriginProxy_ProxyHTTP_ForwardsGRPCTrailers_WithTracing pins that
// enabling tracing on the proxy.Handler — which inserts a countingResponseWriter
// between the trailerBridge and httputil.ReverseProxy — does NOT break the gRPC
// trailer bridge. The countingResponseWriter must stay transparent to the
// stdlib http.TrailerPrefix mechanism so grpc-status still reaches the wire via
// cloudflared's AddTrailer. Without that transparency a gRPC client would see
// "server closed the stream without sending trailers". Validates the production
// tunnel transport per the proxy design principle, not httptest alone.
func TestGatewayOriginProxy_ProxyHTTP_ForwardsGRPCTrailers_WithTracing(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "0")
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", "")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("\x00\x00\x00\x00\x00"))
	}))
	defer backend.Close()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Matches:  []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
			Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolHTTP}},
		}},
	}))

	handler := proxy.NewHandler(router, proxy.WithTracing("proxy"))
	originProxy := tunnel.NewGatewayOriginProxy(handler, nil)

	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "http://example.com/pkg.Svc/Method", http.NoBody,
	)
	req.Header.Set("Content-Type", "application/grpc")

	zlog := zerolog.Nop()
	tracedReq := tracing.NewTracedHTTPRequest(req, 0, &zlog)
	rw := newTrailerContractWriter()

	require.NoError(t, originProxy.ProxyHTTP(rw, tracedReq, false))

	assert.Equal(t, "0", rw.trailers.Get("Grpc-Status"),
		"grpc-status trailer must still reach the wire via AddTrailer with tracing enabled; the "+
			"countingResponseWriter inserted for span status must not shadow the trailer bridge")
}
