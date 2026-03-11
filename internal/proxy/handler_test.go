package proxy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func TestHandler_NoMatchReturns404(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://unknown.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusNotFound, recorder.Code)
}

func TestHandler_ProxiesToBackend(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "reached")
		writer.WriteHeader(http.StatusOK)

		_, err := writer.Write([]byte("hello from backend"))
		if err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/test", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "reached", recorder.Header().Get("X-Backend"))

	body, err := io.ReadAll(recorder.Body)
	require.NoError(t, err)
	assert.Equal(t, "hello from backend", string(body))
}

func TestHandler_RequestHeaderFilter(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		// Echo back the header we expect to be set by the filter.
		writer.Header().Set("X-Received", req.Header.Get("X-Injected"))
		writer.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterRequestHeaderModifier,
						RequestHeaderModifier: &proxy.HeaderModifier{
							Set: []proxy.HeaderValue{{Name: "X-Injected", Value: "filter-value"}},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "filter-value", recorder.Header().Get("X-Received"))
}

func TestHandler_ResponseHeaderFilter(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Internal", "secret")
		writer.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterResponseHeaderModifier,
						ResponseHeaderModifier: &proxy.HeaderModifier{
							Remove: []string{"X-Internal"},
							Set:    []proxy.HeaderValue{{Name: "X-Added", Value: "by-filter"}},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Empty(t, recorder.Header().Get("X-Internal"))
	assert.Equal(t, "by-filter", recorder.Header().Get("X-Added"))
}

func TestHandler_RedirectFilter(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	scheme := testSchemeHTTPS
	statusCode := http.StatusMovedPermanently

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterRequestRedirect,
						RequestRedirect: &proxy.RedirectConfig{
							Scheme:     &scheme,
							StatusCode: &statusCode,
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: "http://unused:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/page", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusMovedPermanently, recorder.Code)
	assert.Equal(t, "https://app.example.com/page", recorder.Header().Get("Location"))
}

func TestHandler_PathMatchRouting(t *testing.T) {
	t.Parallel()

	apiBackend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "api")
		writer.WriteHeader(http.StatusOK)
	}))
	defer apiBackend.Close()

	webBackend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "web")
		writer.WriteHeader(http.StatusOK)
	}))
	defer webBackend.Close()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/api"}},
				},
				Backends: []proxy.BackendRef{{URL: apiBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}},
				},
				Backends: []proxy.BackendRef{{URL: webBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	// API path
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/api/users", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	assert.Equal(t, "api", recorder.Header().Get("X-Backend"))

	// Web path
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/index.html", nil)
	recorder = httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	assert.Equal(t, "web", recorder.Header().Get("X-Backend"))
}

func TestHandler_BackendError(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://127.0.0.1:1", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusBadGateway, recorder.Code)
}

func TestHandler_ClientDisconnectDoesNotReturn504(t *testing.T) {
	t.Parallel()

	// Backend that blocks until context is cancelled.
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	// Create a request with a cancelable context to simulate client disconnect.
	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	done := make(chan struct{})

	go func() {
		handler.ServeHTTP(recorder, req)
		close(done)
	}()

	// Cancel context to simulate client disconnect.
	cancel()
	<-done

	// Should NOT be 504 — client is gone, response doesn't matter much,
	// but it should not be a misleading 504 Gateway Timeout.
	assert.NotEqual(t, http.StatusGatewayTimeout, recorder.Code,
		"client disconnect should not produce 504 Gateway Timeout")
}

func TestHandler_PruneTransportsRemovesStaleHosts(t *testing.T) {
	t.Parallel()

	backendA := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "A")
		writer.WriteHeader(http.StatusOK)
	}))
	defer backendA.Close()

	backendB := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "B")
		writer.WriteHeader(http.StatusOK)
	}))
	defer backendB.Close()

	router := proxy.NewRouter()
	handler := proxy.NewHandler(router)
	router.SetHandler(handler)

	// Configure with backend A and send a request to populate the transport cache.
	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backendA.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	assert.Equal(t, "A", recorder.Header().Get("X-Backend"))

	// Update config to use backend B only. This triggers PruneTransports via SetHandler,
	// which should remove the transport for backend A's host.
	cfg2 := &proxy.Config{
		Version: 2,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backendB.URL, Weight: 1}},
			},
		},
	}

	err = router.UpdateConfig(cfg2)
	require.NoError(t, err)

	// Verify backend B is reachable after the config update.
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder = httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	assert.Equal(t, "B", recorder.Header().Get("X-Backend"))

	// Directly test PruneTransports: prune with no active hosts should not panic.
	handler.PruneTransports(map[string]bool{})
}

func TestHandler_PruneTransportsRemovesStaleHostFromSyncMap(t *testing.T) {
	t.Parallel()

	backendA := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer backendA.Close()

	backendB := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer backendB.Close()

	router := proxy.NewRouter()
	handler := proxy.NewHandler(router)
	router.SetHandler(handler)

	// Configure with backend A and send a request to populate the transport cache.
	cfgA := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backendA.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfgA)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code)

	// Verify transport for backend A was created.
	hostA := backendA.Listener.Addr().String()
	_, loaded := handler.Transports().Load(hostA)
	require.True(t, loaded, "transport for backend A should exist after request")

	// Push new config with only backend B, removing A.
	cfgB := &proxy.Config{
		Version: 2,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backendB.URL, Weight: 1}},
			},
		},
	}

	err = router.UpdateConfig(cfgB)
	require.NoError(t, err)

	// Verify transport for backend A was pruned.
	_, loaded = handler.Transports().Load(hostA)
	assert.False(t, loaded, "transport for backend A should be pruned after config update removed it")
}

func TestHandler_URLRewriteHostnamePreservedByDirector(t *testing.T) {
	t.Parallel()

	receivedHost := make(chan string, 1)

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		receivedHost <- req.Host
		writer.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	rewrittenHostname := "rewritten.example.com"

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterURLRewrite,
						URLRewrite: &proxy.URLRewriteConfig{
							Hostname: &rewrittenHostname,
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/test", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)

	host := <-receivedHost
	assert.Equal(t, "rewritten.example.com", host,
		"Director should preserve the rewritten hostname, not overwrite it with the backend host")
}

func TestHandler_PruneTransportsPreservesActiveHosts(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "active")
		writer.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	router := proxy.NewRouter()
	handler := proxy.NewHandler(router)

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	// Send a request to populate the transport cache.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusOK, recorder.Code)

	// Prune with the backend's host still active. The transport should be preserved
	// and the next request should succeed without creating a new transport.
	activeHosts := map[string]bool{
		backend.Listener.Addr().String(): true,
	}
	handler.PruneTransports(activeHosts)

	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder = httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "active", recorder.Header().Get("X-Backend"))
}

func TestHandler_RequestTimeoutCoversEntireHandler(t *testing.T) {
	t.Parallel()

	// Backend that sleeps longer than the request timeout.
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		select {
		case <-time.After(5 * time.Second):
			writer.WriteHeader(http.StatusOK)
		case <-req.Context().Done():
			// Context cancelled by timeout.
		}
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	requestTimeout := 100 * time.Millisecond

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Timeouts:  &proxy.RouteTimeouts{Request: requestTimeout},
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	start := time.Now()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	elapsed := time.Since(start)

	// The request should be cancelled well before the backend's 5s sleep.
	assert.Less(t, elapsed, 2*time.Second, "request timeout should cancel the handler promptly")
	assert.Equal(t, http.StatusGatewayTimeout, recorder.Code)
}

func TestHandler_BackendTimeoutOnlyAffectsProxyCall(t *testing.T) {
	t.Parallel()

	// Backend that sleeps longer than the backend timeout.
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		select {
		case <-time.After(5 * time.Second):
			writer.WriteHeader(http.StatusOK)
		case <-req.Context().Done():
			// Context cancelled by timeout.
		}
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	backendTimeout := 100 * time.Millisecond

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Timeouts:  &proxy.RouteTimeouts{Backend: backendTimeout},
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	start := time.Now()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	elapsed := time.Since(start)

	// Backend timeout should cancel the proxy call promptly.
	assert.Less(t, elapsed, 2*time.Second, "backend timeout should cancel the proxy call promptly")
	assert.Equal(t, http.StatusGatewayTimeout, recorder.Code)
}

func TestHandler_BothTimeoutsAppliedIndependently(t *testing.T) {
	t.Parallel()

	// Backend that sleeps longer than both timeouts.
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		select {
		case <-time.After(5 * time.Second):
			writer.WriteHeader(http.StatusOK)
		case <-req.Context().Done():
			// Context cancelled by timeout.
		}
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	// Request timeout is longer than backend timeout; backend timeout should fire first.
	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Timeouts: &proxy.RouteTimeouts{
					Request: 2 * time.Second,
					Backend: 100 * time.Millisecond,
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	start := time.Now()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	elapsed := time.Since(start)

	// Backend timeout (100ms) should fire before request timeout (2s).
	assert.Less(t, elapsed, time.Second, "backend timeout should fire before request timeout")
	assert.Equal(t, http.StatusGatewayTimeout, recorder.Code)
}
