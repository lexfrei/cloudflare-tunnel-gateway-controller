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

// TestBuildBackendUpgradeRequest_BaseQuery proves a WebSocket upgrade to a
// backend whose URL carries a query (an ExternalBackend's spec.path of the form
// "/v1?x=1") merges that query into the upgrade request, matching the
// non-WebSocket Director. Request parameters take precedence over base ones.
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
