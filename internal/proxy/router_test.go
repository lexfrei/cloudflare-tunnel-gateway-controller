package proxy_test

import (
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func TestRouter_EmptyConfig(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	req := &http.Request{
		Method: http.MethodGet,
		Host:   "example.com",
		URL:    &url.URL{Path: "/"},
		Header: http.Header{},
	}

	result := router.Route(req)
	assert.Nil(t, result)
}

func TestRouter_UpdateConfig(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://backend:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	req := &http.Request{
		Method: http.MethodGet,
		Host:   "example.com",
		URL:    &url.URL{Path: "/"},
		Header: http.Header{},
	}

	result := router.Route(req)
	require.NotNil(t, result)
	assert.GreaterOrEqual(t, result.BackendIdx, 0)
}

func TestRouter_ExactHostMatch(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://app:80", Weight: 1}},
			},
			{
				Hostnames: []string{"api.example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://api:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	// Match app.example.com
	req := &http.Request{
		Method: http.MethodGet,
		Host:   "app.example.com",
		URL:    &url.URL{Path: "/"},
		Header: http.Header{},
	}

	result := router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://app:80", result.Rule.Backends[0].URL)

	// Match api.example.com
	req.Host = "api.example.com"

	result = router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://api:80", result.Rule.Backends[0].URL)

	// No match for unknown host
	req.Host = "unknown.example.com"

	result = router.Route(req)
	assert.Nil(t, result)
}

func TestRouter_WildcardHostMatch(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"*.example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://wildcard:80", Weight: 1}},
			},
			{
				Hostnames: []string{"specific.example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://specific:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	// Exact host takes precedence over wildcard
	req := &http.Request{
		Method: http.MethodGet,
		Host:   "specific.example.com",
		URL:    &url.URL{Path: "/"},
		Header: http.Header{},
	}

	result := router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://specific:80", result.Rule.Backends[0].URL)

	// Wildcard matches other subdomains
	req.Host = "other.example.com"

	result = router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://wildcard:80", result.Rule.Backends[0].URL)

	// Wildcard does not match bare domain
	req.Host = "example.com"

	result = router.Route(req)
	assert.Nil(t, result)
}

func TestRouter_DefaultRules(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"specific.example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://specific:80", Weight: 1}},
			},
			{
				// No hostnames = default/catch-all rule
				Backends: []proxy.BackendRef{{URL: "http://default:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	// Specific host matches specific rule
	req := &http.Request{
		Method: http.MethodGet,
		Host:   "specific.example.com",
		URL:    &url.URL{Path: "/"},
		Header: http.Header{},
	}

	result := router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://specific:80", result.Rule.Backends[0].URL)

	// Unknown host falls through to default
	req.Host = "anything.example.com"

	result = router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://default:80", result.Rule.Backends[0].URL)
}

func TestRouter_PathPrecedence(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}},
				},
				Backends: []proxy.BackendRef{{URL: "http://catch-all:80", Weight: 1}},
			},
			{
				Hostnames: []string{"example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/api"}},
				},
				Backends: []proxy.BackendRef{{URL: "http://api:80", Weight: 1}},
			},
			{
				Hostnames: []string{"example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/api/v1/users"}},
				},
				Backends: []proxy.BackendRef{{URL: "http://users:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	// Exact match has highest precedence
	req := &http.Request{
		Method: http.MethodGet,
		Host:   "example.com",
		URL:    &url.URL{Path: "/api/v1/users"},
		Header: http.Header{},
	}

	result := router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://users:80", result.Rule.Backends[0].URL)

	// Longer prefix match has higher precedence
	req.URL.Path = "/api/v2/items"

	result = router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://api:80", result.Rule.Backends[0].URL)

	// Short prefix as fallback
	req.URL.Path = "/unmatched"

	result = router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://catch-all:80", result.Rule.Backends[0].URL)
}

func TestRouter_MethodAndHeaderPrecedence(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Matches: []proxy.RouteMatch{
					{
						Path:   &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/api"},
						Method: http.MethodGet,
						Headers: []proxy.HeaderMatch{
							{Type: proxy.HeaderMatchExact, Name: "X-Env", Value: "prod"},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: "http://specific:80", Weight: 1}},
			},
			{
				Hostnames: []string{"example.com"},
				Matches: []proxy.RouteMatch{
					{
						Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/api"},
					},
				},
				Backends: []proxy.BackendRef{{URL: "http://general:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	// Request with method + header matches more specific rule
	req := &http.Request{
		Method: http.MethodGet,
		Host:   "example.com",
		URL:    &url.URL{Path: "/api/test"},
		Header: http.Header{"X-Env": {"prod"}},
	}

	result := router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://specific:80", result.Rule.Backends[0].URL)

	// Request without header matches general rule
	req.Header = http.Header{}

	result = router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://general:80", result.Rule.Backends[0].URL)
}

func TestRouter_MultipleMatchesOR(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/health"}},
					{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/ready"}},
				},
				Backends: []proxy.BackendRef{{URL: "http://health:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	// First match
	req := &http.Request{
		Method: http.MethodGet,
		Host:   "example.com",
		URL:    &url.URL{Path: "/health"},
		Header: http.Header{},
	}

	result := router.Route(req)
	require.NotNil(t, result)

	// Second match (OR)
	req.URL.Path = "/ready"

	result = router.Route(req)
	require.NotNil(t, result)

	// Neither match
	req.URL.Path = "/neither-health-nor-ready"

	result = router.Route(req)
	assert.Nil(t, result)
}

func TestRouter_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://backend:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	var waitGroup sync.WaitGroup

	// Concurrent reads
	for range 100 {
		waitGroup.Go(func() {
			req := &http.Request{
				Method: http.MethodGet,
				Host:   "example.com",
				URL:    &url.URL{Path: "/"},
				Header: http.Header{},
			}

			result := router.Route(req)
			assert.NotNil(t, result)
		})
	}

	// Concurrent write

	waitGroup.Go(func() {
		newCfg := &proxy.Config{
			Version: 2,
			Rules: []proxy.RouteRule{
				{
					Hostnames: []string{"example.com"},
					Backends:  []proxy.BackendRef{{URL: "http://backend-v2:80", Weight: 1}},
				},
			},
		}
		_ = router.UpdateConfig(newCfg)
	})

	waitGroup.Wait()
}

func TestRouter_WeightedBackendSelection(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Backends: []proxy.BackendRef{
					{URL: "http://primary:80", Weight: 80},
					{URL: "http://canary:80", Weight: 20},
				},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	req := &http.Request{
		Method: http.MethodGet,
		Host:   "example.com",
		URL:    &url.URL{Path: "/"},
		Header: http.Header{},
	}

	// Run many iterations and check distribution
	counts := make(map[int]int)
	iterations := 10000

	for range iterations {
		result := router.Route(req)
		counts[result.BackendIdx]++
	}

	// With 80/20 weights, primary should get roughly 80% of traffic
	primaryRatio := float64(counts[0]) / float64(iterations)
	assert.InDelta(t, 0.8, primaryRatio, 0.05, "primary backend should get ~80%% of traffic")
}

func TestRouter_HostWithPort(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://backend:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	// Host header with port should still match
	req := &http.Request{
		Method: http.MethodGet,
		Host:   "example.com:443",
		URL:    &url.URL{Path: "/"},
		Header: http.Header{},
	}

	result := router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://backend:80", result.Rule.Backends[0].URL)
}

func TestRouter_ConfigVersion(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	assert.Equal(t, int64(0), router.ConfigVersion())

	cfg := &proxy.Config{
		Version: 42,
		Rules: []proxy.RouteRule{
			{Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}}},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, int64(42), router.ConfigVersion())
}

func TestRouter_InvalidConfig(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchRegularExpression, Value: "[invalid"}},
				},
				Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compile")
}

func TestRouter_NoMatchesMatchesAll(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				// No matches = matches any request to this host
				Backends: []proxy.BackendRef{{URL: "http://backend:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	req := &http.Request{
		Method: http.MethodPost,
		Host:   "example.com",
		URL:    &url.URL{Path: "/anything/at/all"},
		Header: http.Header{},
	}

	result := router.Route(req)
	require.NotNil(t, result)
}

func TestRouter_EmptyBackends_ReturnsMinusOne(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/redirect"}},
				},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterRequestRedirect,
						RequestRedirect: &proxy.RedirectConfig{
							Hostname:   new("other.example.com"),
							StatusCode: new(301),
						},
					},
				},
				// No backends — redirect-only rule.
			},
		},
	}
	require.NoError(t, router.UpdateConfig(cfg))

	req := &http.Request{
		Method: http.MethodGet,
		Host:   "example.com",
		URL:    &url.URL{Path: "/redirect"},
		Header: http.Header{},
	}

	result := router.Route(req)
	require.NotNil(t, result, "redirect-only rule should match")
	assert.Equal(t, -1, result.BackendIdx, "should return -1 for empty backends")
	assert.NotEmpty(t, result.Rule.Filters, "rule should have redirect filter")
}

func TestRouter_PrefixMatchSegmentAware(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/foo"}},
				},
				Backends: []proxy.BackendRef{{URL: "http://backend:80", Weight: 1}},
			},
		},
	}

	require.NoError(t, router.UpdateConfig(cfg))

	tests := []struct {
		path    string
		matches bool
	}{
		{"/foo", true},
		{"/foo/bar", true},
		{"/foo/", true},
		{"/foobar", false},
		{"/foob", false},
		{"/fo", false},
		{"/bar", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()

			req := &http.Request{
				Method: http.MethodGet,
				Host:   "example.com",
				URL:    &url.URL{Path: tt.path},
				Header: http.Header{},
			}

			result := router.Route(req)
			if tt.matches {
				require.NotNil(t, result, "expected match for %s", tt.path)
			} else {
				assert.Nil(t, result, "expected no match for %s", tt.path)
			}
		})
	}
}

func TestRouter_StaleVersionRejected(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg1 := &proxy.Config{
		Version: 100,
		Rules: []proxy.RouteRule{
			{Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}}},
		},
	}
	require.NoError(t, router.UpdateConfig(cfg1))

	cfg2 := &proxy.Config{
		Version: 50,
		Rules: []proxy.RouteRule{
			{Backends: []proxy.BackendRef{{URL: "http://svc:80", Weight: 1}}},
		},
	}

	err := router.UpdateConfig(cfg2)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale config version")
	assert.Equal(t, int64(100), router.ConfigVersion(), "version should remain at 100")
}

func TestRouter_ReadyzBeforeConfig(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	assert.Equal(t, int64(0), router.ConfigVersion(), "new router should have version 0")
}
