package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// pathRecordingBackend returns an httptest server that records the path of the
// last request it received and replies 200.
func pathRecordingBackend(t *testing.T, gotPath *atomic.Pointer[string]) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		gotPath.Store(&p)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func handlerForBackendURL(t *testing.T, backendURL string) *proxy.Handler {
	t.Helper()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Matches:  []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{{URL: backendURL, Weight: 1, Protocol: proxy.BackendProtocolHTTP}},
			},
		},
	}))

	return proxy.NewHandler(router)
}

// TestHandler_ExternalBackendBasePath_Joined proves the backend URL's base path
// (an ExternalBackend's spec.path, resolved into the backend URL) is prepended
// to the request path on the wire. Without the Director honoring backendURL.Path
// the backend would see "/users" instead of "/v1/users".
func TestHandler_ExternalBackendBasePath_Joined(t *testing.T) {
	t.Parallel()

	var gotPath atomic.Pointer[string]

	backend := pathRecordingBackend(t, &gotPath)
	handler := handlerForBackendURL(t, backend.URL+"/v1")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/users", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Eventually(t, func() bool { return gotPath.Load() != nil }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, "/v1/users", *gotPath.Load(),
		"the backend base path must be prepended to the request path")
}

// TestHandler_ExternalBackendBasePath_OverTunnelWriter exercises the same
// path-join through the cloudflared HTTP/2 response writer contract (per the
// project's tunnel-transport testing rule), proving the Director rewrite holds
// on the production write path, not only with httptest's recorder.
func TestHandler_ExternalBackendBasePath_OverTunnelWriter(t *testing.T) {
	t.Parallel()

	var gotPath atomic.Pointer[string]

	backend := pathRecordingBackend(t, &gotPath)
	handler := handlerForBackendURL(t, backend.URL+"/v1")

	fake := newFakeCloudflaredRespWriter()
	t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/users", nil)

	handler.ServeHTTP(fake, req)

	require.Eventually(t, func() bool { return gotPath.Load() != nil }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, "/v1/users", *gotPath.Load(),
		"the backend base path must be prepended on the tunnel write path too")
	assert.Equal(t, http.StatusOK, fake.Status())
}

// TestHandler_NoBasePath_Unchanged proves a backend URL without a base path
// (every Service / ServiceImport URL) forwards the request path verbatim.
func TestHandler_NoBasePath_Unchanged(t *testing.T) {
	t.Parallel()

	var gotPath atomic.Pointer[string]

	backend := pathRecordingBackend(t, &gotPath)
	handler := handlerForBackendURL(t, backend.URL)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/users", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Eventually(t, func() bool { return gotPath.Load() != nil }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, "/users", *gotPath.Load(), "a backend with no base path forwards the path unchanged")
}
