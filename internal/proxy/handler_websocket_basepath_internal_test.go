package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildBackendUpgradeRequest_BasePath proves a WebSocket upgrade to a
// backend whose URL carries a base path (an ExternalBackend's spec.path) joins
// that base onto the request path, matching the non-WebSocket rewrite. A
// backend URL without a base path forwards the request path unchanged.
func TestBuildBackendUpgradeRequest_BasePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		backendURL string
		reqPath    string
		wantPath   string
	}{
		{
			name:       "base path joined",
			backendURL: "https://api.example.com:8443/v1",
			reqPath:    "/ws",
			wantPath:   "/v1/ws",
		},
		{
			name:       "no base path unchanged",
			backendURL: "http://svc.default.svc.cluster.local:80",
			reqPath:    "/ws",
			wantPath:   "/ws",
		},
		{
			name:       "root base path unchanged",
			backendURL: "https://api.example.com:8443/",
			reqPath:    "/ws",
			wantPath:   "/ws",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			backendURL, err := url.Parse(tt.backendURL)
			require.NoError(t, err)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com"+tt.reqPath, nil)

			out := buildBackendUpgradeRequest(req, backendURL)

			assert.Equal(t, tt.wantPath, out.URL.Path)
		})
	}
}

// TestBuildBackendUpgradeRequest_BaseQuery proves a WebSocket upgrade to a
// backend whose URL carries a query (an ExternalBackend's spec.path of the form
// "/v1?x=1") merges that query into the upgrade request, matching the
// non-WebSocket rewrite. Request parameters take precedence over base ones.
func TestBuildBackendUpgradeRequest_BaseQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		backendURL string
		reqTarget  string
		wantQuery  string
	}{
		{
			name:       "base query, no request query",
			backendURL: "https://api.example.com:8443/v1?token=abc",
			reqTarget:  "/ws",
			wantQuery:  "token=abc",
		},
		{
			name:       "base query merged with disjoint request query",
			backendURL: "https://api.example.com:8443/v1?token=abc",
			reqTarget:  "/ws?room=42",
			wantQuery:  "room=42&token=abc",
		},
		{
			name:       "request wins on conflicting key",
			backendURL: "https://api.example.com:8443/v1?token=base",
			reqTarget:  "/ws?token=req",
			wantQuery:  "token=req",
		},
		{
			name:       "no base query unchanged",
			backendURL: "http://svc.default.svc.cluster.local:80/v1",
			reqTarget:  "/ws?room=42",
			wantQuery:  "room=42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			backendURL, err := url.Parse(tt.backendURL)
			require.NoError(t, err)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com"+tt.reqTarget, nil)

			out := buildBackendUpgradeRequest(req, backendURL)

			assert.Equal(t, tt.wantQuery, out.URL.RawQuery)
		})
	}
}

// TestBuildBackendUpgradeRequest_HopByHopHeaders pins that the upgrade leg
// drops the client's hop-by-hop headers like the plain leg does, keeps the
// Connection and Upgrade pair the handshake needs, and carries the same
// forwarding headers as the plain leg, even when the client names them in
// Connection.
func TestBuildBackendUpgradeRequest_HopByHopHeaders(t *testing.T) {
	t.Parallel()

	backendURL, err := url.Parse("http://svc.default.svc.cluster.local:80")
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/ws", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Connection", "Upgrade, X-Named-Hop, X-Forwarded-For, Forwarded")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("X-Named-Hop", "1")
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Proxy-Connection", "keep-alive")
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Forwarded", "for=203.0.113.7")

	out := buildBackendUpgradeRequest(req, backendURL)

	for _, name := range []string{"X-Named-Hop", "Proxy-Authorization", "Keep-Alive", "Proxy-Connection"} {
		assert.Empty(t, out.Header.Values(name), "%s must not reach the backend", name)
	}

	assert.Equal(t, []string{"Upgrade"}, out.Header.Values("Connection"))
	assert.Equal(t, []string{"websocket"}, out.Header.Values("Upgrade"))
	assert.Equal(t, "dGhlIHNhbXBsZSBub25jZQ==", out.Header.Get("Sec-WebSocket-Key"))
	assert.Equal(t, "13", out.Header.Get("Sec-WebSocket-Version"))
	assert.Equal(t, "203.0.113.7, 192.0.2.1", out.Header.Get("X-Forwarded-For"))
	assert.Equal(t, "https", out.Header.Get("X-Forwarded-Proto"))
	assert.Equal(t, "for=203.0.113.7", out.Header.Get("Forwarded"))

	assert.Equal(t, "Basic c2VjcmV0", req.Header.Get("Proxy-Authorization"), "the inbound request keeps its own headers")
}
