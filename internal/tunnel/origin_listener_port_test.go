package tunnel_test

import (
	"crypto/tls"
	"net/http"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudflare/cloudflared/tracing"

	proxypkg "github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
)

// TestGatewayOriginProxy_ListenerFromEdgeHostAndProto routes requests shaped
// the way each cloudflared transport hands them over. The edge keeps a
// non-default port in Host and sets X-Forwarded-Proto. The URL is not usable:
// the HTTP/2 transport fills a missing one with http://localhost:8080
// (connection.handleMissingRequestParts) over an edge connection that is TLS
// whatever the client used, and the QUIC transport takes it from the edge's
// destination. Each listener's route redirects without a scheme or port, so
// Location shows the scheme and port the request was matched on, and a
// request whose scheme no listener on its port has gets a 404.
func TestGatewayOriginProxy_ListenerFromEdgeHostAndProto(t *testing.T) {
	t.Parallel()

	const gateway = "infra/gw"

	listeners := []proxypkg.Listener{
		{Port: 80, Scheme: "http"}, {Port: 8080, Scheme: "http"}, {Port: 443, Scheme: "https"}, {Port: 8443, Scheme: "https"},
	}

	redirectRule := func(hostname string, listener proxypkg.Listener) proxypkg.RouteRule {
		return proxypkg.RouteRule{
			Listeners: map[string][]proxypkg.Listener{gateway: {listener}},
			Filters: []proxypkg.RouteFilter{{
				Type:            proxypkg.FilterRequestRedirect,
				RequestRedirect: &proxypkg.RedirectConfig{Hostname: new(hostname)},
			}},
		}
	}

	router := proxypkg.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxypkg.Config{
		Version:          1,
		GatewayListeners: map[string][]proxypkg.Listener{gateway: listeners},
		Rules: []proxypkg.RouteRule{
			redirectRule("on-80.example", listeners[0]),
			redirectRule("on-8080.example", listeners[1]),
			redirectRule("on-443.example", listeners[2]),
			redirectRule("on-8443.example", listeners[3]),
		},
	}))

	originProxy := tunnel.NewGatewayOriginProxy(proxypkg.NewHandler(router), nil)

	cases := []struct {
		name, url, host, proto, location string
		edgeTLS                          bool
	}{
		{"http2 https default port", "http://localhost:8080/", "app.example.com", "https", "https://on-443.example/", true},
		{"http2 https 8443", "http://localhost:8080/", "app.example.com:8443", "https", "https://on-8443.example:8443/", true},
		{"http2 http default port", "http://localhost:8080/", "app.example.com", "http", "http://on-80.example/", true},
		{"http2 http 8080", "http://localhost:8080/", "app.example.com:8080", "http", "http://on-8080.example:8080/", true},
		{"quic https 8443", "https://app.example.com:8443/", "app.example.com:8443", "https", "https://on-8443.example:8443/", false},
		{"quic http default port", "http://app.example.com/", "app.example.com", "http", "http://on-80.example/", false},
		{"http2 http on an https port", "http://localhost:8080/", "app.example.com:8443", "http", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.url, nil)
			require.NoError(t, err)

			req.Host = tc.host
			req.Header.Set("X-Forwarded-Proto", tc.proto)

			if tc.edgeTLS {
				req.TLS = &tls.ConnectionState{}
			}

			zlog := zerolog.Nop()
			writer := newTestResponseWriter()

			require.NoError(t, originProxy.ProxyHTTP(writer, tracing.NewTracedHTTPRequest(req, 0, &zlog), false))
			if tc.location == "" {
				assert.Equal(t, http.StatusNotFound, writer.Code)

				return
			}

			assert.Equal(t, http.StatusFound, writer.Code)
			assert.Equal(t, tc.location, writer.Header().Get("Location"))
		})
	}
}
