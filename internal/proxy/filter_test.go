package proxy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
	statusCode := http.StatusFound

	filter := proxy.NewRequestRedirect(&proxy.RedirectConfig{
		Port: &port,
		Path: &proxy.RedirectPath{
			Type:  proxy.RedirectPathFullReplace,
			Value: "/new-path",
		},
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

func TestRequestRedirect_ReplacePrefixMatch(t *testing.T) {
	t.Parallel()

	statusCode := http.StatusMovedPermanently

	filter := proxy.NewRequestRedirect(&proxy.RedirectConfig{
		Path: &proxy.RedirectPath{
			Type:  proxy.RedirectPathPrefixReplace,
			Value: "/v2",
		},
		StatusCode: &statusCode,
	})

	req := &http.Request{
		Host:   "example.com",
		URL:    &url.URL{Scheme: testSchemeHTTPS, Host: "example.com", Path: "/api/users"},
		Header: http.Header{},
	}

	// Set matched prefix as would happen during route matching.
	req = proxy.SetMatchedPrefix(req, "/api")

	resp := filter.ProcessRequest(req)
	require.NotNil(t, resp, "redirect should short-circuit")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode)
	assert.Equal(t, "https://example.com/v2/users", resp.Header.Get("Location"),
		"prefix replacement should only replace the matched prefix, preserving the suffix")
}

func TestRequestRedirect_PathWithSpecialCharacters(t *testing.T) {
	t.Parallel()

	filter := proxy.NewRequestRedirect(&proxy.RedirectConfig{
		Path: &proxy.RedirectPath{
			Type:  proxy.RedirectPathFullReplace,
			Value: "/new path/with spaces",
		},
	})

	req := &http.Request{
		Host:   "example.com",
		URL:    &url.URL{Scheme: testSchemeHTTPS, Host: "example.com", Path: "/old"},
		Header: http.Header{},
	}

	resp := filter.ProcessRequest(req)
	require.NotNil(t, resp, "redirect should short-circuit")
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	assert.Contains(t, location, "example.com")
	assert.NotContains(t, location, " ", "spaces in path should be percent-encoded")
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

func TestRequestRedirect_PrefixReplaceNoDoubleSlash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		replacement   string
		matchedPrefix string
		requestPath   string
		expectedPath  string
	}{
		{
			name:          "trailing slash on replacement with leading slash on suffix",
			replacement:   "/v2/",
			matchedPrefix: "/api",
			requestPath:   "/api/users",
			expectedPath:  "/v2/users",
		},
		{
			name:          "trailing slash on replacement with no suffix",
			replacement:   "/v2/",
			matchedPrefix: "/api",
			requestPath:   "/api",
			expectedPath:  "/v2/",
		},
		{
			name:          "no trailing slash normal case",
			replacement:   "/v2",
			matchedPrefix: "/api",
			requestPath:   "/api/users",
			expectedPath:  "/v2/users",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			statusCode := http.StatusMovedPermanently

			filter := proxy.NewRequestRedirect(&proxy.RedirectConfig{
				Path: &proxy.RedirectPath{
					Type:  proxy.RedirectPathPrefixReplace,
					Value: tt.replacement,
				},
				StatusCode: &statusCode,
			})

			req := &http.Request{
				Host:   "example.com",
				URL:    &url.URL{Scheme: testSchemeHTTPS, Host: "example.com", Path: tt.requestPath},
				Header: http.Header{},
			}
			req = proxy.SetMatchedPrefix(req, tt.matchedPrefix)

			resp := filter.ProcessRequest(req)
			require.NotNil(t, resp)
			defer resp.Body.Close()

			location := resp.Header.Get("Location")
			parsed, err := url.Parse(location)
			require.NoError(t, err)
			assert.Equal(t, tt.expectedPath, parsed.Path,
				"path should not contain double slashes")
		})
	}
}

func TestURLRewriter_PrefixReplaceNoDoubleSlash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		replacement   string
		matchedPrefix string
		requestPath   string
		expectedPath  string
	}{
		{
			name:          "trailing slash on replacement with leading slash on suffix",
			replacement:   "/v2/",
			matchedPrefix: "/api",
			requestPath:   "/api/users",
			expectedPath:  "/v2/users",
		},
		{
			name:          "trailing slash on replacement with no suffix",
			replacement:   "/v2/",
			matchedPrefix: "/api",
			requestPath:   "/api",
			expectedPath:  "/v2/",
		},
		{
			name:          "no trailing slash normal case",
			replacement:   "/v2",
			matchedPrefix: "/api",
			requestPath:   "/api/users",
			expectedPath:  "/v2/users",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			replacement := tt.replacement

			filter := proxy.NewURLRewriter(&proxy.URLRewriteConfig{
				Path: &proxy.URLRewritePath{
					Type:               proxy.URLRewritePrefixMatch,
					ReplacePrefixMatch: &replacement,
				},
			})

			req := &http.Request{
				URL:    &url.URL{Path: tt.requestPath},
				Header: http.Header{},
			}
			req = proxy.SetMatchedPrefix(req, tt.matchedPrefix)

			resp := filter.ProcessRequest(req) //nolint:bodyclose // rewriter returns nil response
			assert.Nil(t, resp)
			assert.Equal(t, tt.expectedPath, req.URL.Path,
				"path should not contain double slashes")
		})
	}
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

func TestRequestMirror_InvalidBackendURL(t *testing.T) {
	t.Parallel()

	filter := proxy.NewRequestMirror("://invalid\x00url")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/test", nil)

	// Should not panic — must handle invalid URL gracefully.
	resp := filter.ProcessRequest(req) //nolint:bodyclose // mirror returns nil response
	assert.Nil(t, resp)
}

func TestRequestMirror_PostBody(t *testing.T) {
	t.Parallel()

	const body = "hello world"

	// Mirror backend that records the received body.
	mirrorBody := make(chan string, 1)

	mirror := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			mirrorBody <- "read error: " + err.Error()

			return
		}

		mirrorBody <- string(data)
	}))
	defer mirror.Close()

	filter := proxy.NewRequestMirror(mirror.URL)

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"http://example.com/test",
		strings.NewReader(body),
	)
	req.ContentLength = int64(len(body))

	resp := filter.ProcessRequest(req) //nolint:bodyclose // mirror returns nil response
	assert.Nil(t, resp, "mirror should not short-circuit")

	// Verify the mirror received the complete body.
	got := <-mirrorBody
	assert.Equal(t, body, got, "mirror backend should receive the complete request body")

	// Verify the original request body is still fully readable for the main handler.
	remaining, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(remaining),
		"original request body must remain intact for the main handler")
	assert.Equal(t, int64(len(body)), req.ContentLength,
		"content length must reflect the buffered body size")
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

func TestRedirectFilter_PreservesQueryParams(t *testing.T) {
	t.Parallel()

	hostname := "redirect.example.com"
	filters := []proxy.RouteFilter{
		{
			Type: proxy.FilterRequestRedirect,
			RequestRedirect: &proxy.RedirectConfig{
				Hostname: &hostname,
			},
		},
	}

	compiled, err := proxy.CompileFilters(filters)
	require.NoError(t, err)

	req := &http.Request{
		Host: "example.com",
		URL: &url.URL{
			Scheme:   testSchemeHTTPS,
			Host:     "example.com",
			Path:     "/search",
			RawQuery: "q=hello&page=2",
		},
		Header: http.Header{},
	}

	resp := proxy.ApplyRequestFilters(compiled, req)
	require.NotNil(t, resp)
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	assert.Contains(t, location, "q=hello&page=2",
		"redirect should preserve original query parameters")
	assert.Contains(t, location, "redirect.example.com")
}

func TestURLRewriter_HostnameDoesNotCorruptRequest(t *testing.T) {
	t.Parallel()

	const rewrittenHost = "rewritten.example.com"
	hostname := rewrittenHost
	filters := []proxy.RouteFilter{
		{
			Type: proxy.FilterURLRewrite,
			URLRewrite: &proxy.URLRewriteConfig{
				Hostname: &hostname,
			},
		},
	}

	compiled, err := proxy.CompileFilters(filters)
	require.NoError(t, err)

	req := &http.Request{
		Host:   "original.example.com",
		URL:    &url.URL{Scheme: "http", Host: "original.example.com", Path: "/test"},
		Header: http.Header{},
	}

	resp := proxy.ApplyRequestFilters(compiled, req)
	if resp != nil {
		defer resp.Body.Close()
	}

	assert.Nil(t, resp, "URL rewrite should not short-circuit")
	assert.Equal(t, rewrittenHost, req.Host)
}

func TestRequestMirror_BodyReadError_RestoresPartialBody(t *testing.T) {
	t.Parallel()

	// Create a reader that returns data then errors.
	partialData := "partial-data"
	failingBody := io.NopCloser(io.MultiReader(
		strings.NewReader(partialData),
		&errorReader{err: io.ErrUnexpectedEOF},
	))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com/test", failingBody)

	filter := proxy.NewRequestMirror("http://mirror-backend:8080")
	resp := filter.ProcessRequest(req)

	if resp != nil {
		defer resp.Body.Close()
	}

	assert.Nil(t, resp, "mirror should not short-circuit on body read error")

	// The partial body data should be restored for the main handler.
	restoredBody, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Contains(t, string(restoredBody), partialData,
		"partial body data should be restored after read error")
}

func TestRequestMirror_OversizeBody_SetsUnknownContentLength(t *testing.T) {
	t.Parallel()

	// Create a body larger than maxMirrorBodySize (1 MiB).
	oversizeBody := strings.Repeat("x", 1<<20+100)
	req := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "http://example.com/test",
		strings.NewReader(oversizeBody),
	)
	req.ContentLength = int64(len(oversizeBody))

	filter := proxy.NewRequestMirror("http://mirror-backend:8080")
	resp := filter.ProcessRequest(req)

	if resp != nil {
		defer resp.Body.Close()
	}

	assert.Nil(t, resp, "mirror should not short-circuit on oversize body")
	assert.Equal(t, int64(-1), req.ContentLength,
		"ContentLength should be -1 (unknown) for reassembled oversize body")

	// Verify the body is still readable.
	restoredBody, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.NotEmpty(t, restoredBody, "body should still be readable after oversize skip")
}

// errorReader is a reader that always returns the specified error.
type errorReader struct {
	err error
}

func (r *errorReader) Read(_ []byte) (int, error) {
	return 0, r.err
}
