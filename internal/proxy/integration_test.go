package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// newBackend creates an httptest.Server that sets X-Backend to the given name
// and echoes the received path in X-Received-Path. The server is automatically
// closed when the test completes.
func newBackend(t *testing.T, name string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		writer.Header().Set("X-Backend", name)
		writer.Header().Set("X-Received-Path", req.URL.Path)
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return server
}

// newEchoHeadersBackend creates a backend that echoes received request headers
// back as response headers with an "X-Echo-" prefix. The server is
// automatically closed when the test completes.
func newEchoHeadersBackend(t *testing.T, headerNames ...string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		for _, name := range headerNames {
			if val := req.Header.Get(name); val != "" {
				writer.Header().Set("X-Echo-"+name, val)
			}
		}

		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return server
}

func TestHandler_PathMatchPrecedence(t *testing.T) {
	t.Parallel()

	exactBackend := newBackend(t, "exact")
	prefixBackend := newBackend(t, "prefix")
	regexBackend := newBackend(t, "regex")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/api/v1/users"}},
				},
				Backends: []proxy.BackendRef{{URL: exactBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/api"}},
				},
				Backends: []proxy.BackendRef{{URL: prefixBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchRegularExpression, Value: "/api/v[0-9]+/.*"}},
				},
				Backends: []proxy.BackendRef{{URL: regexBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	tests := []struct {
		name            string
		path            string
		expectedBackend string
		expectedStatus  int
	}{
		{
			name:            "exact wins over prefix and regex",
			path:            "/api/v1/users",
			expectedBackend: "exact",
			expectedStatus:  http.StatusOK,
		},
		{
			// Per Gateway API spec, regex paths have higher precedence than
			// prefix paths, so the regex rule wins.
			name:            "regex wins over prefix per gateway api precedence",
			path:            "/api/v1/orders",
			expectedBackend: "regex",
			expectedStatus:  http.StatusOK,
		},
		{
			name:            "only prefix matches",
			path:            "/api/health",
			expectedBackend: "prefix",
			expectedStatus:  http.StatusOK,
		},
		{
			name:           "no match returns 404",
			path:           "/other",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com"+tt.path, nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)

			if tt.expectedBackend != "" {
				assert.Equal(t, tt.expectedBackend, recorder.Header().Get("X-Backend"))
			}
		})
	}
}

func TestHandler_HeaderBasedRouting(t *testing.T) {
	t.Parallel()

	prodBackend := newBackend(t, "prod")
	stagingBackend := newBackend(t, "staging")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						Headers: []proxy.HeaderMatch{
							{Type: proxy.HeaderMatchExact, Name: "X-Env", Value: "prod"},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: prodBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						Headers: []proxy.HeaderMatch{
							{Type: proxy.HeaderMatchExact, Name: "X-Env", Value: "staging"},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: stagingBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	tests := []struct {
		name            string
		headerValue     string
		expectedBackend string
		expectedStatus  int
	}{
		{
			name:            "prod header routes to prod",
			headerValue:     "prod",
			expectedBackend: "prod",
			expectedStatus:  http.StatusOK,
		},
		{
			name:            "staging header routes to staging",
			headerValue:     "staging",
			expectedBackend: "staging",
			expectedStatus:  http.StatusOK,
		},
		{
			name:           "no matching header returns 404",
			headerValue:    "dev",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
			req.Header.Set("X-Env", tt.headerValue)

			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)

			if tt.expectedBackend != "" {
				assert.Equal(t, tt.expectedBackend, recorder.Header().Get("X-Backend"))
			}
		})
	}
}

func TestHandler_QueryParamRouting(t *testing.T) {
	t.Parallel()

	jsonBackend := newBackend(t, "json")
	xmlBackend := newBackend(t, "xml")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						QueryParams: []proxy.QueryParamMatch{
							{Type: proxy.QueryParamMatchExact, Name: "format", Value: "json"},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: jsonBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						QueryParams: []proxy.QueryParamMatch{
							{Type: proxy.QueryParamMatchExact, Name: "format", Value: "xml"},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: xmlBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	tests := []struct {
		name            string
		query           string
		expectedBackend string
		expectedStatus  int
	}{
		{
			name:            "json query param routes to json backend",
			query:           "?format=json",
			expectedBackend: "json",
			expectedStatus:  http.StatusOK,
		},
		{
			name:            "xml query param routes to xml backend",
			query:           "?format=xml",
			expectedBackend: "xml",
			expectedStatus:  http.StatusOK,
		},
		{
			name:           "unknown format returns 404",
			query:          "?format=csv",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/"+tt.query, nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)

			if tt.expectedBackend != "" {
				assert.Equal(t, tt.expectedBackend, recorder.Header().Get("X-Backend"))
			}
		})
	}
}

func TestHandler_MethodBasedRouting(t *testing.T) {
	t.Parallel()

	readerBackend := newBackend(t, "reader")
	writerBackend := newBackend(t, "writer")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						Path:   &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/api/data"},
						Method: http.MethodGet,
					},
				},
				Backends: []proxy.BackendRef{{URL: readerBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						Path:   &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/api/data"},
						Method: http.MethodPost,
					},
				},
				Backends: []proxy.BackendRef{{URL: writerBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	tests := []struct {
		name            string
		method          string
		expectedBackend string
		expectedStatus  int
	}{
		{
			name:            "GET routes to reader",
			method:          http.MethodGet,
			expectedBackend: "reader",
			expectedStatus:  http.StatusOK,
		},
		{
			name:            "POST routes to writer",
			method:          http.MethodPost,
			expectedBackend: "writer",
			expectedStatus:  http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), tt.method, "http://app.example.com/api/data", nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)
			assert.Equal(t, tt.expectedBackend, recorder.Header().Get("X-Backend"))
		})
	}
}

func TestHandler_CombinedMatchConditions(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "combined")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{
						Path:   &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/api"},
						Method: http.MethodGet,
						Headers: []proxy.HeaderMatch{
							{Type: proxy.HeaderMatchExact, Name: "X-Version", Value: "v2"},
						},
						QueryParams: []proxy.QueryParamMatch{
							{Type: proxy.QueryParamMatchExact, Name: "format", Value: "json"},
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

	tests := []struct {
		name           string
		method         string
		path           string
		headers        map[string]string
		query          string
		expectedStatus int
	}{
		{
			name:           "all conditions match",
			method:         http.MethodGet,
			path:           "/api/data",
			headers:        map[string]string{"X-Version": "v2"},
			query:          "?format=json",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "wrong method",
			method:         http.MethodPost,
			path:           "/api/data",
			headers:        map[string]string{"X-Version": "v2"},
			query:          "?format=json",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "wrong path",
			method:         http.MethodGet,
			path:           "/other",
			headers:        map[string]string{"X-Version": "v2"},
			query:          "?format=json",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "wrong header",
			method:         http.MethodGet,
			path:           "/api/data",
			headers:        map[string]string{"X-Version": "v1"},
			query:          "?format=json",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "missing header",
			method:         http.MethodGet,
			path:           "/api/data",
			query:          "?format=json",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "wrong query param",
			method:         http.MethodGet,
			path:           "/api/data",
			headers:        map[string]string{"X-Version": "v2"},
			query:          "?format=xml",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "missing query param",
			method:         http.MethodGet,
			path:           "/api/data",
			headers:        map[string]string{"X-Version": "v2"},
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), tt.method, "http://app.example.com"+tt.path+tt.query, nil)

			for key, val := range tt.headers {
				req.Header.Set(key, val)
			}

			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)
		})
	}
}

func TestHandler_MultipleMatchesOR(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "matched")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/health"}},
					{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/ready"}},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	tests := []struct {
		name            string
		path            string
		expectedBackend string
		expectedStatus  int
	}{
		{
			name:            "first match block",
			path:            "/health",
			expectedBackend: "matched",
			expectedStatus:  http.StatusOK,
		},
		{
			name:            "second match block",
			path:            "/ready",
			expectedBackend: "matched",
			expectedStatus:  http.StatusOK,
		},
		{
			name:           "neither match returns 404",
			path:           "/other",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com"+tt.path, nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)

			if tt.expectedBackend != "" {
				assert.Equal(t, tt.expectedBackend, recorder.Header().Get("X-Backend"))
			}
		})
	}
}

func TestHandler_URLRewriteFullPath(t *testing.T) {
	t.Parallel()

	backend := newBackend(t, "rewrite")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/old"}},
				},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterURLRewrite,
						URLRewrite: &proxy.URLRewriteConfig{
							Path: &proxy.URLRewritePath{
								Type:            proxy.URLRewriteFullPath,
								ReplaceFullPath: new("/new/path"),
							},
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

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/old/foo", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "/new/path", recorder.Header().Get("X-Received-Path"))
}

func TestHandler_URLRewritePrefixMatch(t *testing.T) {
	t.Parallel()

	// The handler now automatically sets the matched prefix from the RouteResult,
	// so no wrapper is needed. The prefix is stored in the request context
	// before filters are applied.

	backend := newBackend(t, "prefix-rewrite")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/v1"}},
				},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterURLRewrite,
						URLRewrite: &proxy.URLRewriteConfig{
							Path: &proxy.URLRewritePath{
								Type:               proxy.URLRewritePrefixMatch,
								ReplacePrefixMatch: new("/v2"),
							},
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

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/v1/users/123", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "/v2/users/123", recorder.Header().Get("X-Received-Path"))
}

func TestHandler_RedirectSchemeAndHost(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"old.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterRequestRedirect,
						RequestRedirect: &proxy.RedirectConfig{
							Scheme:     new("https"),
							Hostname:   new("new.example.com"),
							StatusCode: new(http.StatusMovedPermanently),
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

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://old.example.com/path", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusMovedPermanently, recorder.Code)
	assert.Equal(t, "https://new.example.com/path", recorder.Header().Get("Location"))
}

func TestHandler_WildcardHostnameRouting(t *testing.T) {
	t.Parallel()

	wildcardBackend := newBackend(t, "wildcard")
	exactBackend := newBackend(t, "exact")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"*.example.com"},
				Backends:  []proxy.BackendRef{{URL: wildcardBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"api.example.com"},
				Backends:  []proxy.BackendRef{{URL: exactBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	tests := []struct {
		name            string
		host            string
		expectedBackend string
		expectedStatus  int
	}{
		{
			name:            "exact host wins over wildcard",
			host:            "api.example.com",
			expectedBackend: "exact",
			expectedStatus:  http.StatusOK,
		},
		{
			name:            "wildcard matches other subdomains",
			host:            "web.example.com",
			expectedBackend: "wildcard",
			expectedStatus:  http.StatusOK,
		},
		{
			name:           "different domain returns 404",
			host:           "other.com",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+tt.host+"/", nil)
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, req)

			assert.Equal(t, tt.expectedStatus, recorder.Code)

			if tt.expectedBackend != "" {
				assert.Equal(t, tt.expectedBackend, recorder.Header().Get("X-Backend"))
			}
		})
	}
}

func TestHandler_WeightedBackendsDistribution(t *testing.T) {
	t.Parallel()

	backendA := newBackend(t, "A")
	backendB := newBackend(t, "B")

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends: []proxy.BackendRef{
					{URL: backendA.URL, Weight: 80},
					{URL: backendB.URL, Weight: 20},
				},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	const totalRequests = 1000

	counts := map[string]int{}

	for range totalRequests {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
		recorder := httptest.NewRecorder()

		handler.ServeHTTP(recorder, req)

		backend := recorder.Header().Get("X-Backend")
		counts[backend]++
	}

	ratioA := float64(counts["A"]) / float64(totalRequests)
	ratioB := float64(counts["B"]) / float64(totalRequests)

	assert.InDelta(t, 0.80, ratioA, 0.15, "backend A should receive ~80%% of traffic, got %.2f%%", ratioA*100)
	assert.InDelta(t, 0.20, ratioB, 0.15, "backend B should receive ~20%% of traffic, got %.2f%%", ratioB*100)

	// Sanity check: both backends received at least some traffic.
	assert.Greater(t, counts["A"], 0, "backend A should receive at least one request")
	assert.Greater(t, counts["B"], 0, "backend B should receive at least one request")
}

func TestHandler_RequestMirrorDoesNotAffectResponse(t *testing.T) {
	t.Parallel()

	primaryBackend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "primary")
		writer.WriteHeader(http.StatusOK)

		_, err := writer.Write([]byte("primary response"))
		if err != nil {
			t.Errorf("failed to write primary response: %v", err)
		}
	}))
	t.Cleanup(primaryBackend.Close)

	var mirrorReceived atomic.Bool

	mirrorBackend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		// Simulate a slow mirror backend.
		time.Sleep(50 * time.Millisecond)
		mirrorReceived.Store(true)
	}))
	t.Cleanup(mirrorBackend.Close)

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type:          proxy.FilterRequestMirror,
						RequestMirror: &proxy.MirrorConfig{BackendURL: mirrorBackend.URL},
					},
				},
				Backends: []proxy.BackendRef{{URL: primaryBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/test", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	// Client gets the primary response immediately.
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "primary", recorder.Header().Get("X-Backend"))

	body, err := io.ReadAll(recorder.Body)
	require.NoError(t, err)
	assert.Equal(t, "primary response", string(body))

	// Wait for mirror to eventually receive the request.
	assert.Eventually(t, func() bool {
		return mirrorReceived.Load()
	}, 2*time.Second, 10*time.Millisecond, "mirror backend should eventually receive the request")
}

func TestHandler_MultipleFiltersApplied(t *testing.T) {
	t.Parallel()

	backend := newEchoHeadersBackend(t, "X-A", "X-B", "X-Internal")

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
							Set:    []proxy.HeaderValue{{Name: "X-A", Value: "1"}},
							Add:    []proxy.HeaderValue{{Name: "X-B", Value: "2"}},
							Remove: []string{"X-Internal"},
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
	req.Header.Set("X-Internal", "secret-value")

	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)

	// X-A was set by the filter.
	assert.Equal(t, "1", recorder.Header().Get("X-Echo-X-A"))

	// X-B was added by the filter.
	assert.Equal(t, "2", recorder.Header().Get("X-Echo-X-B"))

	// X-Internal was removed by the filter, so the backend should not echo it.
	assert.Empty(t, recorder.Header().Get("X-Echo-X-Internal"))
}
