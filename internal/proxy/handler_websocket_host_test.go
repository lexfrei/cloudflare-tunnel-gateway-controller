package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestHandler_WebSocketUpgrade_HostMatchesPlainPath pins that the upgrade leg
// hands the backend the same Host the plain HTTP leg does: the client's Host
// unless a URLRewrite filter chose one, with the trusted X-Original-Host
// carrier restored the same way.
func TestHandler_WebSocketUpgrade_HostMatchesPlainPath(t *testing.T) {
	t.Parallel()

	rewritten := "rewritten.example.com"

	tests := []struct {
		name        string
		filters     []proxy.RouteFilter
		opts        []proxy.HandlerOption
		originalHdr string
		wantHost    string
	}{
		{
			name:     "client Host is preserved",
			wantHost: "app.example.com",
		},
		{
			name: "URLRewrite hostname wins",
			filters: []proxy.RouteFilter{{
				Type:       proxy.FilterURLRewrite,
				URLRewrite: &proxy.URLRewriteConfig{Hostname: &rewritten},
			}},
			wantHost: rewritten,
		},
		{
			name:        "trusted X-Original-Host is restored",
			opts:        []proxy.HandlerOption{proxy.WithAllowXOriginalHost(true)},
			originalHdr: "intended.example.com",
			wantHost:    "intended.example.com",
		},
		{
			name: "URLRewrite hostname wins over a trusted X-Original-Host",
			filters: []proxy.RouteFilter{{
				Type:       proxy.FilterURLRewrite,
				URLRewrite: &proxy.URLRewriteConfig{Hostname: &rewritten},
			}},
			opts:        []proxy.HandlerOption{proxy.WithAllowXOriginalHost(true)},
			originalHdr: "intended.example.com",
			wantHost:    rewritten,
		},
		{
			name:        "untrusted X-Original-Host is ignored",
			originalHdr: "intended.example.com",
			wantHost:    "app.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			seen := make(chan *http.Request, 1)

			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r
				w.WriteHeader(http.StatusForbidden)
			}))
			t.Cleanup(backend.Close)

			router := proxy.NewRouter()
			require.NoError(t, router.UpdateConfig(&proxy.Config{
				Version: 1,
				Rules: []proxy.RouteRule{{
					Matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
					Filters: tt.filters,
					Backends: []proxy.BackendRef{
						{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolHTTP, WebSocket: true},
					},
				}},
			}))

			fake := newFakeCloudflaredRespWriter()

			t.Cleanup(func() {
				_ = fake.serverSide.Close()
				_ = fake.clientSide.Close()
			})

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/ws", nil)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", rfc6455SampleWSKey)

			if tt.originalHdr != "" {
				req.Header.Set("X-Original-Host", tt.originalHdr)
			}

			proxy.NewHandler(router, tt.opts...).ServeHTTP(fake, req)

			var got *http.Request

			select {
			case got = <-seen:
			case <-time.After(5 * time.Second):
				t.Fatal("the upgrade request never reached the backend")
			}

			assert.Equal(t, tt.wantHost, got.Host)
			assert.Empty(t, got.Header.Get("X-Original-Host"), "the carrier never reaches a backend")
			assert.Empty(t, got.Header.Get("X-Proxy-Host-Rewritten"), "the marker never reaches a backend")
		})
	}
}
