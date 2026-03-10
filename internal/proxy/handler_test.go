package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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
