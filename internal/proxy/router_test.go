package proxy_test

import (
	"net/http"
	"net/url"
	"strings"
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

func TestRouter_MatchedPrefixFromCorrectMatch(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/api"}},
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/v2"}},
				},
				Backends: []proxy.BackendRef{{URL: "http://backend:80", Weight: 1}},
			},
		},
	}

	require.NoError(t, router.UpdateConfig(cfg))

	// Request matches second match (/v2), not first (/api).
	// MatchedPrefix must be /v2, not /api.
	req := &http.Request{
		Method: http.MethodGet,
		Host:   "example.com",
		URL:    &url.URL{Path: "/v2/users"},
		Header: http.Header{},
	}

	result := router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "/v2", result.MatchedPrefix, "should return prefix from the match that fired")
}

func TestRouter_ZeroWeightBackendsReturnNoBackend(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Backends: []proxy.BackendRef{
					{URL: "http://a:80", Weight: 0},
					{URL: "http://b:80", Weight: 0},
				},
			},
		},
	}

	require.NoError(t, router.UpdateConfig(cfg))

	req := &http.Request{
		Method: http.MethodGet,
		Host:   "example.com",
		URL:    &url.URL{Path: "/"},
		Header: http.Header{},
	}

	// All backends have zero weight — should return -1 per Gateway API spec.
	for range 100 {
		result := router.Route(req)
		require.NotNil(t, result)
		assert.Equal(t, -1, result.BackendIdx, "zero-weight backends should not receive traffic")
	}
}

func TestRouter_LargeWeightsNoOverflow(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Backends: []proxy.BackendRef{
					{URL: "http://a:80", Weight: 1_500_000_000},
					{URL: "http://b:80", Weight: 1_500_000_000},
				},
			},
		},
	}

	require.NoError(t, router.UpdateConfig(cfg))

	req := &http.Request{
		Method: http.MethodGet,
		Host:   "example.com",
		URL:    &url.URL{Path: "/"},
		Header: http.Header{},
	}

	// With int32, 1.5B + 1.5B = 3B overflows (max int32 ~2.1B).
	// With int64, both backends should receive traffic.
	selectedA, selectedB := false, false

	for range 1000 {
		result := router.Route(req)
		require.NotNil(t, result)
		require.GreaterOrEqual(t, result.BackendIdx, 0, "valid backend should be selected")

		if result.BackendIdx == 0 {
			selectedA = true
		} else {
			selectedB = true
		}

		if selectedA && selectedB {
			break
		}
	}

	assert.True(t, selectedA && selectedB, "both backends should receive traffic with large weights")
}

func TestRouter_ExactPathAlwaysBeatsPrefix(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	// Create a prefix match with a very long path (600+ chars)
	// and an exact match with a short path. Per Gateway API spec,
	// exact match ALWAYS wins regardless of path length.
	longPrefix := "/" + strings.Repeat("a", 600)

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: longPrefix}},
				},
				Backends: []proxy.BackendRef{{URL: "http://prefix-backend:80", Weight: 1}},
			},
			{
				Hostnames: []string{"example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: longPrefix + "/sub"}},
				},
				Backends: []proxy.BackendRef{{URL: "http://exact-backend:80", Weight: 1}},
			},
		},
	}

	require.NoError(t, router.UpdateConfig(cfg))

	req := &http.Request{
		Method: http.MethodGet,
		Host:   "example.com",
		URL:    &url.URL{Path: longPrefix + "/sub"},
		Header: http.Header{},
	}

	result := router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, 0, result.BackendIdx)
	assert.Contains(t, result.Rule.Backends[0].URL, "exact-backend",
		"exact path should always beat prefix path regardless of length")
}

func TestRouter_HostnameCaseInsensitive(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"Example.COM"},
				Backends:  []proxy.BackendRef{{URL: "http://backend:80", Weight: 1}},
			},
			{
				Hostnames: []string{"*.UPPER.Example.Com"},
				Backends:  []proxy.BackendRef{{URL: "http://wildcard:80", Weight: 1}},
			},
		},
	}

	require.NoError(t, router.UpdateConfig(cfg))

	// Lowercase request host should match uppercase config hostname.
	req := &http.Request{
		Method: http.MethodGet,
		Host:   "example.com",
		URL:    &url.URL{Path: "/"},
		Header: http.Header{},
	}

	result := router.Route(req)
	require.NotNil(t, result, "lowercase host should match uppercase config")
	assert.Equal(t, "http://backend:80", result.Rule.Backends[0].URL)

	// Wildcard with mixed-case config should match lowercase host.
	req.Host = "app.upper.example.com"

	result = router.Route(req)
	require.NotNil(t, result, "lowercase host should match uppercase wildcard config")
	assert.Equal(t, "http://wildcard:80", result.Rule.Backends[0].URL)
}

func TestRouter_XOriginalHostOverridesHost(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://example-backend:80", Weight: 1}},
			},
			{
				Hostnames: []string{"edge.tunnel.example"},
				Backends:  []proxy.BackendRef{{URL: "http://edge-backend:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	// When X-Original-Host is set, it should be used for routing
	// instead of the Host header (which may be the edge hostname).
	req := &http.Request{
		Method: http.MethodGet,
		Host:   "edge.tunnel.example",
		URL:    &url.URL{Path: "/"},
		Header: http.Header{
			"X-Original-Host": []string{"example.com"},
		},
	}

	result := router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://example-backend:80", result.Rule.Backends[0].URL,
		"should route based on X-Original-Host, not Host header")

	// Without X-Original-Host, should fall back to Host header.
	req.Header.Del("X-Original-Host")

	result = router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://edge-backend:80", result.Rule.Backends[0].URL,
		"should route based on Host header when X-Original-Host is absent")
}

func TestRouter_LongerPrefixWins(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	// Reproduces HTTPRouteMatchingAcrossRoutes conformance test:
	// matching-part1: hostnames [example.com, example.net], path "/" → v1
	// matching-part2: hostnames [example.com], path "/v2" → v2
	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"example.com", "example.net"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}},
					{Headers: []proxy.HeaderMatch{{Name: "version", Value: "one", Type: proxy.HeaderMatchExact}}},
				},
				Backends: []proxy.BackendRef{{URL: "http://v1:80", Weight: 1}},
			},
			{
				Hostnames: []string{"example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/v2"}},
					{Headers: []proxy.HeaderMatch{{Name: "version", Value: "two", Type: proxy.HeaderMatchExact}}},
				},
				Backends: []proxy.BackendRef{{URL: "http://v2:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	tests := []struct {
		name     string
		host     string
		path     string
		headers  map[string]string
		expected string
	}{
		{
			name:     "catch-all / goes to v1",
			host:     "example.com",
			path:     "/",
			expected: "http://v1:80",
		},
		{
			name:     "/v2 prefix goes to v2",
			host:     "example.com",
			path:     "/v2",
			expected: "http://v2:80",
		},
		{
			name:     "/v2/example goes to v2",
			host:     "example.com",
			path:     "/v2/example",
			expected: "http://v2:80",
		},
		{
			name:     "header version:two goes to v2",
			host:     "example.com",
			path:     "/",
			headers:  map[string]string{"version": "two"},
			expected: "http://v2:80",
		},
		{
			name:     "example.net /v2 goes to v1 (v2 only on example.com)",
			host:     "example.net",
			path:     "/v2",
			expected: "http://v1:80",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			header := http.Header{}
			for k, v := range tt.headers {
				header.Set(k, v)
			}

			req := &http.Request{
				Method: http.MethodGet,
				Host:   tt.host,
				URL:    &url.URL{Path: tt.path},
				Header: header,
			}

			result := router.Route(req)
			require.NotNil(t, result, "expected a route match")
			assert.Equal(t, tt.expected, result.Rule.Backends[0].URL)
		})
	}
}

// TestRouter_DefaultRulesNoHostname reproduces the HTTPRouteMatching conformance test.
// Routes with no hostnames go to defaultRules and should match any host.
func TestRouter_DefaultRulesNoHostname(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	// Single HTTPRoute, no hostnames, two rules:
	// Rule 0: path / OR header version:one → v1
	// Rule 1: path /v2 OR header version:two → v2
	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				// No hostnames — goes to defaultRules
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}},
					{Headers: []proxy.HeaderMatch{{Name: "version", Value: "one", Type: proxy.HeaderMatchExact}}},
				},
				Backends: []proxy.BackendRef{{URL: "http://v1:80", Weight: 1}},
			},
			{
				// No hostnames — goes to defaultRules
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/v2"}},
					{Headers: []proxy.HeaderMatch{{Name: "version", Value: "two", Type: proxy.HeaderMatchExact}}},
				},
				Backends: []proxy.BackendRef{{URL: "http://v2:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	tests := []struct {
		name     string
		host     string
		path     string
		headers  map[string]string
		expected string
	}{
		{
			name:     "/ goes to v1",
			host:     "random-tunnel-id.cfargotunnel.com",
			path:     "/",
			expected: "http://v1:80",
		},
		{
			name:     "/example goes to v1",
			host:     "random-tunnel-id.cfargotunnel.com",
			path:     "/example",
			expected: "http://v1:80",
		},
		{
			name:     "/v2 goes to v2",
			host:     "random-tunnel-id.cfargotunnel.com",
			path:     "/v2",
			expected: "http://v2:80",
		},
		{
			name:     "/v2/example goes to v2",
			host:     "random-tunnel-id.cfargotunnel.com",
			path:     "/v2/example",
			expected: "http://v2:80",
		},
		{
			name:     "header version:two goes to v2",
			host:     "random-tunnel-id.cfargotunnel.com",
			path:     "/",
			headers:  map[string]string{"version": "two"},
			expected: "http://v2:80",
		},
		{
			name:     "/v2example goes to v1 (not segment prefix)",
			host:     "random-tunnel-id.cfargotunnel.com",
			path:     "/v2example",
			expected: "http://v1:80",
		},
		{
			name:     "/foo/v2/example goes to v1 (prefix must be at start)",
			host:     "random-tunnel-id.cfargotunnel.com",
			path:     "/foo/v2/example",
			expected: "http://v1:80",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			header := http.Header{}
			for k, v := range tt.headers {
				header.Set(k, v)
			}

			req := &http.Request{
				Method: http.MethodGet,
				Host:   tt.host,
				URL:    &url.URL{Path: tt.path},
				Header: header,
			}

			result := router.Route(req)
			require.NotNil(t, result, "expected a route match for %s %s", tt.host, tt.path)
			assert.Equal(t, tt.expected, result.Rule.Backends[0].URL)
		})
	}
}

// TestRouter_DefaultRulesWithDefaultedPaths reproduces the exact config that
// Gateway API webhook defaulting produces: header-only matches get an implicit
// PathPrefix "/" added. This previously caused incorrect priority calculation
// where both rules had the same max per-match score, making Rule 0 always win.
func TestRouter_DefaultRulesWithDefaultedPaths(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	// Mirrors httproute-matching.yaml AFTER Gateway API defaulting:
	// Rule 0: [PathPrefix /, PathPrefix / + header:one] → v1
	// Rule 1: [PathPrefix /v2, PathPrefix / + header:two] → v2
	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}},
					{
						Path:    &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"},
						Headers: []proxy.HeaderMatch{{Name: "version", Value: "one", Type: proxy.HeaderMatchExact}},
					},
				},
				Backends: []proxy.BackendRef{{URL: "http://v1:80", Weight: 1}},
			},
			{
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/v2"}},
					{
						Path:    &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"},
						Headers: []proxy.HeaderMatch{{Name: "version", Value: "two", Type: proxy.HeaderMatchExact}},
					},
				},
				Backends: []proxy.BackendRef{{URL: "http://v2:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	tests := []struct {
		name     string
		path     string
		headers  map[string]string
		expected string
	}{
		{name: "/ goes to v1", path: "/", expected: "http://v1:80"},
		{name: "/example goes to v1", path: "/example", expected: "http://v1:80"},
		{name: "/v2 goes to v2", path: "/v2", expected: "http://v2:80"},
		{name: "/v2/ goes to v2", path: "/v2/", expected: "http://v2:80"},
		{name: "/v2/example goes to v2", path: "/v2/example", expected: "http://v2:80"},
		{name: "header version:one goes to v1", path: "/", headers: map[string]string{"version": "one"}, expected: "http://v1:80"},
		{name: "header version:two goes to v2", path: "/", headers: map[string]string{"version": "two"}, expected: "http://v2:80"},
		{name: "/v2example goes to v1", path: "/v2example", expected: "http://v1:80"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			header := http.Header{}
			for k, v := range tt.headers {
				header.Set(k, v)
			}

			req := &http.Request{
				Method: http.MethodGet,
				Host:   "any-host.example.com",
				URL:    &url.URL{Path: tt.path},
				Header: header,
			}

			result := router.Route(req)
			require.NotNil(t, result, "expected a route match for %s", tt.path)
			assert.Equal(t, tt.expected, result.Rule.Backends[0].URL)
		})
	}
}

// TestRouter_PathLengthDominatesMethod verifies that a longer path prefix
// always takes precedence over a method match per GEP-1722 ordering.
func TestRouter_PathLengthDominatesMethod(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	// Rule 0 (index 6 in conformance): PathPrefix /path5 → v1
	// Rule 1 (index 7 in conformance): method PATCH (defaulted path /) → v2
	// Per spec: path length > method, so /path5 wins for PATCH /path5.
	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/path5"}},
				},
				Backends: []proxy.BackendRef{{URL: "http://v1:80", Weight: 1}},
			},
			{
				Matches: []proxy.RouteMatch{
					{
						Path:   &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"},
						Method: "PATCH",
					},
				},
				Backends: []proxy.BackendRef{{URL: "http://v2:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	tests := []struct {
		name     string
		method   string
		path     string
		expected string
	}{
		{name: "PATCH /path5 goes to v1 (path wins over method)", method: "PATCH", path: "/path5", expected: "http://v1:80"},
		{name: "GET /path5 goes to v1", method: "GET", path: "/path5", expected: "http://v1:80"},
		{name: "PATCH / goes to v2 (method match)", method: "PATCH", path: "/", expected: "http://v2:80"},
		{name: "PATCH /other goes to v2 (method match, no path5)", method: "PATCH", path: "/other", expected: "http://v2:80"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := &http.Request{
				Method: tt.method,
				Host:   "any.example.com",
				URL:    &url.URL{Path: tt.path},
				Header: http.Header{},
			}

			result := router.Route(req)
			require.NotNil(t, result, "expected a route match for %s %s", tt.method, tt.path)
			assert.Equal(t, tt.expected, result.Rule.Backends[0].URL)
		})
	}
}

func TestRouter_QueryParamCountDominatesRuleIndex(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	// Reproduces the conformance test HTTPRouteQueryParamMatching:
	// Rule 1 (index 1): 1 query param (animal=dolphin) → v2
	// Rule 2 (index 2): 2 query params (animal=dolphin AND color=blue) → v3
	// Per GEP-1722: more query params = higher precedence, regardless of rule index.
	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Matches: []proxy.RouteMatch{
					{QueryParams: []proxy.QueryParamMatch{
						{Type: proxy.QueryParamMatchExact, Name: "animal", Value: "cat"},
					}},
				},
				Backends: []proxy.BackendRef{{URL: "http://v1:80", Weight: 1}},
			},
			{
				Matches: []proxy.RouteMatch{
					{QueryParams: []proxy.QueryParamMatch{
						{Type: proxy.QueryParamMatchExact, Name: "animal", Value: "dolphin"},
					}},
				},
				Backends: []proxy.BackendRef{{URL: "http://v2:80", Weight: 1}},
			},
			{
				Matches: []proxy.RouteMatch{
					{QueryParams: []proxy.QueryParamMatch{
						{Type: proxy.QueryParamMatchExact, Name: "animal", Value: "dolphin"},
						{Type: proxy.QueryParamMatchExact, Name: "color", Value: "blue"},
					}},
					{QueryParams: []proxy.QueryParamMatch{
						{Type: proxy.QueryParamMatchExact, Name: "ANIMAL", Value: "Whale"},
					}},
				},
				Backends: []proxy.BackendRef{{URL: "http://v3:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	tests := []struct {
		name     string
		query    string
		expected string
	}{
		{
			name:     "single param matches rule 1",
			query:    "animal=cat",
			expected: "http://v1:80",
		},
		{
			name:     "single param matches rule 2",
			query:    "animal=dolphin",
			expected: "http://v2:80",
		},
		{
			name:     "two params match more-specific rule 3",
			query:    "animal=dolphin&color=blue",
			expected: "http://v3:80",
		},
		{
			name:     "case-sensitive param matches rule 3 second match",
			query:    "ANIMAL=Whale",
			expected: "http://v3:80",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := &http.Request{
				Method: http.MethodGet,
				Host:   "any.example.com",
				URL:    &url.URL{Path: "/", RawQuery: tt.query},
				Header: http.Header{},
			}

			result := router.Route(req)
			require.NotNil(t, result, "expected a route match for ?%s", tt.query)
			assert.Equal(t, tt.expected, result.Rule.Backends[0].URL)
		})
	}
}
