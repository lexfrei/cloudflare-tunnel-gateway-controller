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
// that base onto the request path, matching the non-WebSocket Director. A
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
