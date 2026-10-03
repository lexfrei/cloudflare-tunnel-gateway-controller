package tunnel_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudflare/cloudflared/tracing"

	proxypkg "github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
)

// TestGatewayOriginProxy_ChunkedBodyReachesBackend sends a request body the
// way each tunnel transport hands it over and asserts the backend receives it.
// cloudflared's QUIC connection builds the request with
// http.NewRequestWithContext over the stream, so a chunked body without a
// Content-Length header keeps ContentLength at 0 (connection.buildHTTPRequest).
// The Go HTTP/2 server reports such a body as -1 instead.
func TestGatewayOriginProxy_ChunkedBodyReachesBackend(t *testing.T) {
	t.Parallel()

	cases := map[string]int64{
		"quic transport, ContentLength 0":   0,
		"http2 transport, ContentLength -1": -1,
	}

	for name, contentLength := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			received := make(chan string, 1)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, _ := io.ReadAll(req.Body)
				received <- string(body)

				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(backend.Close)

			router := proxypkg.NewRouter()
			require.NoError(t, router.UpdateConfig(&proxypkg.Config{
				Version: 1,
				Rules: []proxypkg.RouteRule{{
					Backends: []proxypkg.BackendRef{{URL: backend.URL, Weight: 1}},
				}},
			}))

			originProxy := tunnel.NewGatewayOriginProxy(proxypkg.NewHandler(router), nil)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.com/upload",
				io.NopCloser(strings.NewReader("payload")))
			require.NoError(t, err)

			req.Header.Set("Transfer-Encoding", "chunked")
			req.ContentLength = contentLength

			zlog := zerolog.Nop()
			writer := newTestResponseWriter()

			require.NoError(t, originProxy.ProxyHTTP(writer, tracing.NewTracedHTTPRequest(req, 0, &zlog), false))
			assert.Equal(t, http.StatusOK, writer.Code)
			assert.Equal(t, "payload", <-received)
		})
	}
}

// TestGatewayOriginProxy_DeclaredEmptyBodyKeepsLength pins that a body
// declared empty, without a chunked Transfer-Encoding, reaches the backend
// with Content-Length 0 rather than as a chunked body.
func TestGatewayOriginProxy_DeclaredEmptyBodyKeepsLength(t *testing.T) {
	t.Parallel()

	received := make(chan int64, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		received <- req.ContentLength

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	router := proxypkg.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxypkg.Config{
		Version: 1,
		Rules:   []proxypkg.RouteRule{{Backends: []proxypkg.BackendRef{{URL: backend.URL, Weight: 1}}}},
	}))

	originProxy := tunnel.NewGatewayOriginProxy(proxypkg.NewHandler(router), nil)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.com/upload",
		io.NopCloser(strings.NewReader("")))
	require.NoError(t, err)

	req.Header.Set("Content-Length", "0")

	zlog := zerolog.Nop()
	require.NoError(t, originProxy.ProxyHTTP(newTestResponseWriter(), tracing.NewTracedHTTPRequest(req, 0, &zlog), false))
	assert.Equal(t, int64(0), <-received)
}
