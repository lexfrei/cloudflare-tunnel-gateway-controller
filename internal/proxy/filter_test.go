package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

const testSchemeHTTPS = "https"

func TestRequestHeaderModifier_Set(t *testing.T) {
	t.Parallel()

	filter := proxy.NewRequestHeaderModifier(&proxy.HeaderModifier{
		Set: []proxy.HeaderValue{
			{Name: "X-Forwarded-Proto", Value: "https"},
			{Name: "X-Existing", Value: "new-value"},
		},
	})

	req := &http.Request{
		Header: http.Header{
			"X-Existing": {"old-value"},
		},
		URL: &url.URL{},
	}

	resp := filter.ProcessRequest(req) //nolint:bodyclose // ProcessRequest returns nil for header modifiers
	assert.Nil(t, resp, "request header modifier should not short-circuit")
	assert.Equal(t, testSchemeHTTPS, req.Header.Get("X-Forwarded-Proto"))
	assert.Equal(t, "new-value", req.Header.Get("X-Existing"))
}

func TestRequestHeaderModifier_Add(t *testing.T) {
	t.Parallel()

	filter := proxy.NewRequestHeaderModifier(&proxy.HeaderModifier{
		Add: []proxy.HeaderValue{
			{Name: "X-Custom", Value: "added"},
		},
	})

	req := &http.Request{
		Header: http.Header{
			"X-Custom": {"existing"},
		},
		URL: &url.URL{},
	}

	filter.ProcessRequest(req) //nolint:bodyclose // ProcessRequest returns nil for header modifiers
	assert.Equal(t, []string{"existing", "added"}, req.Header.Values("X-Custom"))
}

func TestRequestHeaderModifier_Remove(t *testing.T) {
	t.Parallel()

	filter := proxy.NewRequestHeaderModifier(&proxy.HeaderModifier{
		Remove: []string{"X-Internal", "X-Debug"},
	})

	req := &http.Request{
		Header: http.Header{
			"X-Internal": {"secret"},
			"X-Debug":    {"true"},
			"X-Keep":     {"value"},
		},
		URL: &url.URL{},
	}

	filter.ProcessRequest(req) //nolint:bodyclose // ProcessRequest returns nil for header modifiers
	assert.Empty(t, req.Header.Get("X-Internal"))
	assert.Empty(t, req.Header.Get("X-Debug"))
	assert.Equal(t, "value", req.Header.Get("X-Keep"))
}

func TestResponseHeaderModifier_Set(t *testing.T) {
	t.Parallel()

	filter := proxy.NewResponseHeaderModifier(&proxy.HeaderModifier{
		Set: []proxy.HeaderValue{
			{Name: "Cache-Control", Value: "no-cache"},
		},
	})

	resp := &http.Response{
		Header: http.Header{
			"Cache-Control": {"max-age=3600"},
		},
	}

	filter.ProcessResponse(resp)
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
}

func TestResponseHeaderModifier_Add(t *testing.T) {
	t.Parallel()

	filter := proxy.NewResponseHeaderModifier(&proxy.HeaderModifier{
		Add: []proxy.HeaderValue{
			{Name: "X-Custom", Value: "added"},
		},
	})

	resp := &http.Response{
		Header: http.Header{
			"X-Custom": {"existing"},
		},
	}

	filter.ProcessResponse(resp)
	assert.Equal(t, []string{"existing", "added"}, resp.Header.Values("X-Custom"))
}

func TestResponseHeaderModifier_Remove(t *testing.T) {
	t.Parallel()

	filter := proxy.NewResponseHeaderModifier(&proxy.HeaderModifier{
		Remove: []string{"X-Internal"},
	})

	resp := &http.Response{
		Header: http.Header{
			"X-Internal": {"secret"},
			"X-Keep":     {"value"},
		},
	}

	filter.ProcessResponse(resp)
	assert.Empty(t, resp.Header.Get("X-Internal"))
	assert.Equal(t, "value", resp.Header.Get("X-Keep"))
}

func TestRequestRedirect_Basic(t *testing.T) {
	t.Parallel()

	scheme := testSchemeHTTPS
	hostname := "new.example.com"
	statusCode := http.StatusMovedPermanently

	filter := proxy.NewRequestRedirect(&proxy.RedirectConfig{
		Scheme:     &scheme,
		Hostname:   &hostname,
		StatusCode: &statusCode,
	})

	req := &http.Request{
		Host:   "old.example.com",
		URL:    &url.URL{Scheme: "http", Host: "old.example.com", Path: "/path"},
		Header: http.Header{},
	}

	resp := filter.ProcessRequest(req)
	require.NotNil(t, resp, "redirect should short-circuit")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode)
	assert.Equal(t, "https://new.example.com/path", resp.Header.Get("Location"))
}

func TestRequestRedirect_PortAndPath(t *testing.T) {
	t.Parallel()

	port := int32(8443)
	path := "/new-path"
	statusCode := http.StatusFound

	filter := proxy.NewRequestRedirect(&proxy.RedirectConfig{
		Port:       &port,
		Path:       &path,
		StatusCode: &statusCode,
	})

	req := &http.Request{
		Host:   "example.com",
		URL:    &url.URL{Scheme: testSchemeHTTPS, Host: "example.com", Path: "/old-path"},
		Header: http.Header{},
	}

	resp := filter.ProcessRequest(req)
	require.NotNil(t, resp)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, "https://example.com:8443/new-path", resp.Header.Get("Location"))
}

func TestRequestRedirect_DefaultStatusCode(t *testing.T) {
	t.Parallel()

	scheme := testSchemeHTTPS

	filter := proxy.NewRequestRedirect(&proxy.RedirectConfig{
		Scheme: &scheme,
	})

	req := &http.Request{
		Host:   "example.com",
		URL:    &url.URL{Scheme: "http", Host: "example.com", Path: "/"},
		Header: http.Header{},
	}

	resp := filter.ProcessRequest(req)
	require.NotNil(t, resp)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
}

func TestURLRewriter_ReplaceFullPath(t *testing.T) {
	t.Parallel()

	newPath := "/new/path"

	filter := proxy.NewURLRewriter(&proxy.URLRewriteConfig{
		Path: &proxy.URLRewritePath{
			Type:            proxy.URLRewriteFullPath,
			ReplaceFullPath: &newPath,
		},
	})

	req := &http.Request{
		URL:    &url.URL{Path: "/old/path"},
		Header: http.Header{},
	}

	resp := filter.ProcessRequest(req) //nolint:bodyclose // rewriter returns nil response
	assert.Nil(t, resp, "rewrite should not short-circuit")
	assert.Equal(t, "/new/path", req.URL.Path)
}

func TestURLRewriter_ReplacePrefixMatch(t *testing.T) {
	t.Parallel()

	replacement := "/v2"

	filter := proxy.NewURLRewriter(&proxy.URLRewriteConfig{
		Path: &proxy.URLRewritePath{
			Type:               proxy.URLRewritePrefixMatch,
			ReplacePrefixMatch: &replacement,
		},
	})

	req := &http.Request{
		URL:    &url.URL{Path: "/api/v1/users"},
		Header: http.Header{},
	}

	// Need to set the matched prefix for prefix replacement to work.
	req = proxy.SetMatchedPrefix(req, "/api/v1")

	resp := filter.ProcessRequest(req) //nolint:bodyclose // rewriter returns nil response
	assert.Nil(t, resp)
	assert.Equal(t, "/v2/users", req.URL.Path)
}

func TestURLRewriter_Hostname(t *testing.T) {
	t.Parallel()

	hostname := "new-host.example.com"

	filter := proxy.NewURLRewriter(&proxy.URLRewriteConfig{
		Hostname: &hostname,
	})

	req := &http.Request{
		Host:   "old-host.example.com",
		URL:    &url.URL{Path: "/path"},
		Header: http.Header{},
	}

	resp := filter.ProcessRequest(req) //nolint:bodyclose // rewriter returns nil response
	assert.Nil(t, resp)
	assert.Equal(t, "new-host.example.com", req.Host)
}

func TestRequestMirror(t *testing.T) {
	t.Parallel()

	// Set up a mirror backend that records requests.
	received := make(chan struct{}, 1)

	mirror := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		received <- struct{}{}
	}))
	defer mirror.Close()

	filter := proxy.NewRequestMirror(mirror.URL)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/test", nil)

	resp := filter.ProcessRequest(req) //nolint:bodyclose // mirror returns nil response
	assert.Nil(t, resp, "mirror should not short-circuit")

	// Wait for the mirror request to be received.
	<-received
}

func TestCompileFilters(t *testing.T) {
	t.Parallel()

	t.Run("all filter types", func(t *testing.T) {
		t.Parallel()

		scheme := testSchemeHTTPS

		filters := []proxy.RouteFilter{
			{
				Type:                  proxy.FilterRequestHeaderModifier,
				RequestHeaderModifier: &proxy.HeaderModifier{Set: []proxy.HeaderValue{{Name: "X-Test", Value: "v"}}},
			},
			{
				Type:                   proxy.FilterResponseHeaderModifier,
				ResponseHeaderModifier: &proxy.HeaderModifier{Remove: []string{"X-Internal"}},
			},
			{
				Type:            proxy.FilterRequestRedirect,
				RequestRedirect: &proxy.RedirectConfig{Scheme: &scheme},
			},
			{
				Type:       proxy.FilterURLRewrite,
				URLRewrite: &proxy.URLRewriteConfig{Hostname: &scheme},
			},
			{
				Type:          proxy.FilterRequestMirror,
				RequestMirror: &proxy.MirrorConfig{BackendURL: "http://mirror:80"},
			},
		}

		compiled, err := proxy.CompileFilters(filters)
		require.NoError(t, err)
		assert.Len(t, compiled, len(filters))
	})

	t.Run("unknown filter type returns error", func(t *testing.T) {
		t.Parallel()

		filters := []proxy.RouteFilter{
			{Type: "Unknown"},
		}

		_, err := proxy.CompileFilters(filters)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown filter type")
	})
}

func TestApplyRequestFilters_ShortCircuit(t *testing.T) {
	t.Parallel()

	scheme := testSchemeHTTPS

	filters := []proxy.RouteFilter{
		{
			Type:                  proxy.FilterRequestHeaderModifier,
			RequestHeaderModifier: &proxy.HeaderModifier{Set: []proxy.HeaderValue{{Name: "X-Before", Value: "yes"}}},
		},
		{
			Type:            proxy.FilterRequestRedirect,
			RequestRedirect: &proxy.RedirectConfig{Scheme: &scheme},
		},
		{
			Type:                  proxy.FilterRequestHeaderModifier,
			RequestHeaderModifier: &proxy.HeaderModifier{Set: []proxy.HeaderValue{{Name: "X-After", Value: "yes"}}},
		},
	}

	compiled, err := proxy.CompileFilters(filters)
	require.NoError(t, err)

	req := &http.Request{
		Host:   "example.com",
		URL:    &url.URL{Scheme: "http", Host: "example.com", Path: "/"},
		Header: http.Header{},
	}

	resp := proxy.ApplyRequestFilters(compiled, req)
	require.NotNil(t, resp, "redirect should short-circuit")
	defer resp.Body.Close()
	assert.Equal(t, "yes", req.Header.Get("X-Before"), "filters before redirect should run")
	assert.Empty(t, req.Header.Get("X-After"), "filters after redirect should not run")
}
