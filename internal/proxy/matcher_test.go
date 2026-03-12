package proxy_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func TestExactPathMatcher(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{name: "exact match", pattern: "/api/v1/users", path: "/api/v1/users", want: true},
		{name: "no match prefix", pattern: "/api/v1", path: "/api/v1/users", want: false},
		{name: "no match suffix", pattern: "/api/v1/users", path: "/api/v1", want: false},
		{name: "root path", pattern: "/", path: "/", want: true},
		{name: "root no match", pattern: "/", path: "/foo", want: false},
		{name: "trailing slash matters", pattern: "/api/", path: "/api", want: false},
		{name: "case sensitive", pattern: "/API", path: "/api", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			matcher := proxy.NewExactPathMatcher(tt.pattern)
			req := &http.Request{URL: &url.URL{Path: tt.path}}
			assert.Equal(t, tt.want, matcher.Match(req))
		})
	}
}

func TestPrefixPathMatcher(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prefix string
		path   string
		want   bool
	}{
		{name: "exact match", prefix: "/api", path: "/api", want: true},
		{name: "prefix match", prefix: "/api", path: "/api/v1/users", want: true},
		{name: "no match", prefix: "/api", path: "/other", want: false},
		{name: "root matches all", prefix: "/", path: "/anything", want: true},
		{name: "root matches root", prefix: "/", path: "/", want: true},
		{name: "partial word no match", prefix: "/app", path: "/application", want: false},
		{name: "case sensitive", prefix: "/API", path: "/api/v1", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			matcher := proxy.NewPrefixPathMatcher(tt.prefix)
			req := &http.Request{URL: &url.URL{Path: tt.path}}
			assert.Equal(t, tt.want, matcher.Match(req))
		})
	}
}

func TestRegexPathMatcher(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{name: "simple regex", pattern: `/api/v\d+/users`, path: "/api/v1/users", want: true},
		{name: "no match", pattern: `/api/v\d+/users`, path: "/api/vX/users", want: false},
		{name: "anchored start", pattern: `^/api`, path: "/api/v1", want: true},
		{name: "anchored no match", pattern: `^/api$`, path: "/api/v1", want: false},
		{name: "extension match", pattern: `\.(jpg|png|css)$`, path: "/img/logo.png", want: true},
		{name: "extension no match", pattern: `\.(jpg|png|css)$`, path: "/img/logo.svg", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			matcher, err := proxy.NewRegexPathMatcher(tt.pattern)
			require.NoError(t, err)

			req := &http.Request{URL: &url.URL{Path: tt.path}}
			assert.Equal(t, tt.want, matcher.Match(req))
		})
	}
}

func TestRegexPathMatcher_InvalidRegex(t *testing.T) {
	t.Parallel()

	_, err := proxy.NewRegexPathMatcher(`[invalid`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compile regex")
}

func TestExactHeaderMatcher(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		header    string
		value     string
		reqHeader http.Header
		want      bool
	}{
		{
			name: "exact match", header: "X-Env", value: "prod",
			reqHeader: http.Header{"X-Env": {"prod"}}, want: true,
		},
		{
			name: "no match", header: "X-Env", value: "prod",
			reqHeader: http.Header{"X-Env": {"staging"}}, want: false,
		},
		{
			name: "missing header", header: "X-Env", value: "prod",
			reqHeader: http.Header{}, want: false,
		},
		{
			name: "case insensitive header name", header: "x-env", value: "prod",
			reqHeader: http.Header{"X-Env": {"prod"}}, want: true,
		},
		{
			name: "case sensitive value", header: "X-Env", value: "Prod",
			reqHeader: http.Header{"X-Env": {"prod"}}, want: false,
		},
		{
			name: "multiple values first match", header: "Accept", value: "text/html",
			reqHeader: http.Header{"Accept": {"text/html", "application/json"}}, want: true,
		},
		{
			name: "multiple values second match", header: "Accept", value: "application/json",
			reqHeader: http.Header{"Accept": {"text/html", "application/json"}}, want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			matcher := proxy.NewExactHeaderMatcher(tt.header, tt.value)
			req := &http.Request{Header: tt.reqHeader, URL: &url.URL{}}
			assert.Equal(t, tt.want, matcher.Match(req))
		})
	}
}

func TestRegexHeaderMatcher(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		header    string
		pattern   string
		reqHeader http.Header
		want      bool
	}{
		{
			name: "regex match", header: "User-Agent", pattern: `Mozilla.*Firefox`,
			reqHeader: http.Header{"User-Agent": {"Mozilla/5.0 Firefox/100"}}, want: true,
		},
		{
			name: "no match", header: "User-Agent", pattern: `Mozilla.*Firefox`,
			reqHeader: http.Header{"User-Agent": {"Chrome/100"}}, want: false,
		},
		{
			name: "missing header", header: "X-Custom", pattern: `.*`,
			reqHeader: http.Header{}, want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			matcher, err := proxy.NewRegexHeaderMatcher(tt.header, tt.pattern)
			require.NoError(t, err)

			req := &http.Request{Header: tt.reqHeader, URL: &url.URL{}}
			assert.Equal(t, tt.want, matcher.Match(req))
		})
	}
}

func TestExactQueryMatcher(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		param string
		value string
		query string
		want  bool
	}{
		{name: "exact match", param: "format", value: "json", query: "format=json", want: true},
		{name: "no match", param: "format", value: "json", query: "format=xml", want: false},
		{name: "missing param", param: "format", value: "json", query: "other=val", want: false},
		{name: "empty query", param: "format", value: "json", query: "", want: false},
		{name: "multiple params", param: "b", value: "2", query: "a=1&b=2&c=3", want: true},
		{name: "multiple values", param: "tag", value: "v2", query: "tag=v1&tag=v2", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			matcher := proxy.NewExactQueryMatcher(tt.param, tt.value)
			req := &http.Request{URL: &url.URL{RawQuery: tt.query}}
			assert.Equal(t, tt.want, matcher.Match(req))
		})
	}
}

func TestRegexQueryMatcher(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		param   string
		pattern string
		query   string
		want    bool
	}{
		{name: "regex match", param: "version", pattern: `v\d+`, query: "version=v42", want: true},
		{name: "no match", param: "version", pattern: `v\d+`, query: "version=latest", want: false},
		{name: "missing param", param: "version", pattern: `.*`, query: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			matcher, err := proxy.NewRegexQueryMatcher(tt.param, tt.pattern)
			require.NoError(t, err)

			req := &http.Request{URL: &url.URL{RawQuery: tt.query}}
			assert.Equal(t, tt.want, matcher.Match(req))
		})
	}
}

func TestMethodMatcher(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		req    string
		want   bool
	}{
		{name: "GET match", method: http.MethodGet, req: http.MethodGet, want: true},
		{name: "POST match", method: http.MethodPost, req: http.MethodPost, want: true},
		{name: "no match", method: http.MethodGet, req: http.MethodPost, want: false},
		{name: "case sensitive", method: "get", req: http.MethodGet, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			matcher := proxy.NewMethodMatcher(tt.method)
			req := &http.Request{Method: tt.req, URL: &url.URL{}}
			assert.Equal(t, tt.want, matcher.Match(req))
		})
	}
}

func TestCompileMatch(t *testing.T) {
	t.Parallel()

	t.Run("empty match matches everything", func(t *testing.T) {
		t.Parallel()

		compiled, err := proxy.CompileMatch(proxy.RouteMatch{})
		require.NoError(t, err)

		req := &http.Request{
			Method: http.MethodGet,
			URL:    &url.URL{Path: "/anything"},
			Header: http.Header{},
		}
		assert.True(t, compiled.Match(req))
	})

	t.Run("all conditions ANDed", func(t *testing.T) {
		t.Parallel()

		compiled, err := proxy.CompileMatch(proxy.RouteMatch{
			Path:   &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/api"},
			Method: http.MethodGet,
			Headers: []proxy.HeaderMatch{
				{Type: proxy.HeaderMatchExact, Name: "X-Env", Value: "prod"},
			},
		})
		require.NoError(t, err)

		// All match
		req := &http.Request{
			Method: http.MethodGet,
			URL:    &url.URL{Path: "/api"},
			Header: http.Header{"X-Env": {"prod"}},
		}
		assert.True(t, compiled.Match(req))

		// Path mismatch
		req = &http.Request{
			Method: http.MethodGet,
			URL:    &url.URL{Path: "/other"},
			Header: http.Header{"X-Env": {"prod"}},
		}
		assert.False(t, compiled.Match(req))

		// Method mismatch
		req = &http.Request{
			Method: http.MethodPost,
			URL:    &url.URL{Path: "/api"},
			Header: http.Header{"X-Env": {"prod"}},
		}
		assert.False(t, compiled.Match(req))

		// Header mismatch
		req = &http.Request{
			Method: http.MethodGet,
			URL:    &url.URL{Path: "/api"},
			Header: http.Header{"X-Env": {"staging"}},
		}
		assert.False(t, compiled.Match(req))
	})

	t.Run("invalid regex returns error", func(t *testing.T) {
		t.Parallel()

		_, err := proxy.CompileMatch(proxy.RouteMatch{
			Path: &proxy.PathMatch{Type: proxy.PathMatchRegularExpression, Value: "[invalid"},
		})
		require.Error(t, err)
	})
}
