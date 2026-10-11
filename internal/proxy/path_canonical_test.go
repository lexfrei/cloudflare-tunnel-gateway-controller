package proxy_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// recordingBackend answers 200 and reports the request-target it received
// on the wire, tagged with its own name.
func recordingBackend(t *testing.T, name string, received chan<- string) *httptest.Server {
	t.Helper()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- name + " " + r.RequestURI

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	return backend
}

// canonicalPathRouter serves app.example.com with prefix rules /app, /b%c3%a9
// and /u@ rewriting to /v2, /v3 and /v4, exact rules /caf%C3%A9 and
// /users/@me, a regex rule matching a decoded space, and a catch-all.
func canonicalPathRouter(t *testing.T, received chan<- string) *proxy.Router {
	t.Helper()

	app := recordingBackend(t, "app", received)
	exact := recordingBackend(t, "exact", received)
	regex := recordingBackend(t, "regex", received)
	root := recordingBackend(t, "root", received)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/app"}}},
				Filters: []proxy.RouteFilter{{
					Type: proxy.FilterURLRewrite,
					URLRewrite: &proxy.URLRewriteConfig{Path: &proxy.URLRewritePath{
						Type:               proxy.URLRewritePrefixMatch,
						ReplacePrefixMatch: new("/v2"),
					}},
				}},
				Backends: []proxy.BackendRef{{URL: app.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/b%c3%a9"}}},
				Filters: []proxy.RouteFilter{{
					Type: proxy.FilterURLRewrite,
					URLRewrite: &proxy.URLRewriteConfig{Path: &proxy.URLRewritePath{
						Type:               proxy.URLRewritePrefixMatch,
						ReplacePrefixMatch: new("/v3"),
					}},
				}},
				Backends: []proxy.BackendRef{{URL: app.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/u@"}}},
				Filters: []proxy.RouteFilter{{
					Type: proxy.FilterURLRewrite,
					URLRewrite: &proxy.URLRewriteConfig{Path: &proxy.URLRewritePath{
						Type:               proxy.URLRewritePrefixMatch,
						ReplacePrefixMatch: new("/v4"),
					}},
				}},
				Backends: []proxy.BackendRef{{URL: app.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/caf%C3%A9"}},
					{Path: &proxy.PathMatch{Type: proxy.PathMatchExact, Value: "/users/@me"}},
				},
				Backends: []proxy.BackendRef{{URL: exact.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchRegularExpression, Value: "^/files/a b$"}}},
				Backends:  []proxy.BackendRef{{URL: regex.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: root.URL, Weight: 1}},
			},
		},
	}))

	return router
}

func TestHandler_MatchesAndForwardsCanonicalPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "prefix rewrite", path: "/app/x", want: "app /v2/x"},
		{name: "current-directory segment removed", path: "/app/./x", want: "app /v2/x"},
		{name: "parent segment resolved before matching", path: "/app/../other", want: "root /other"},
		{name: "parent segment resolved within the prefix", path: "/app/a/../x", want: "app /v2/x"},
		{name: "percent-encoded dot segment resolved", path: "/app/%2e%2e/other", want: "root /other"},
		{name: "percent-encoded unreserved decoded before matching", path: "/%61pp/x", want: "app /v2/x"},
		{name: "encoded slash kept through a rewrite", path: "/app/a%2Fb", want: "app /v2/a%2Fb"},
		{name: "encoded slash is part of its segment", path: "/app%2Fx", want: "root /app%2Fx"},
		{name: "encoded slash kept without a rewrite", path: "/other/a%2fb", want: "root /other/a%2Fb"},
		{name: "exact value with escapes matches either hex case", path: "/caf%c3%a9", want: "exact /caf%C3%A9"},
		{name: "regex sees the decoded path", path: "/files/x/../a%20b", want: "regex /files/a%20b"},
		{name: "empty segments preserved", path: "/app//x", want: "app /v2//x"},
		{name: "prefix value with lower-case escapes is replaced", path: "/b%C3%A9/x", want: "app /v3/x"},
		{name: "escaped reserved character matches its literal", path: "/users/%40me", want: "exact /users/%40me"},
		{name: "escaped reserved character in the prefix is replaced", path: "/u%40/x", want: "app /v4/x"},
		{name: "escaped reserved character in the suffix is kept", path: "/app/%40x", want: "app /v2/%40x"},
		{name: "escaped percent is not decoded for matching", path: "/users/%2540me", want: "root /users/%2540me"},
	}

	writers := map[string]func(t *testing.T) (http.ResponseWriter, func() int){
		"httptest": func(_ *testing.T) (http.ResponseWriter, func() int) {
			rec := httptest.NewRecorder()

			return rec, func() int { return rec.Code }
		},
		"tunnel": func(t *testing.T) (http.ResponseWriter, func() int) {
			t.Helper()

			fake := newFakeCloudflaredRespWriter()
			t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

			return fake, fake.Status
		},
	}

	for writerName, newWriter := range writers {
		for _, tt := range tests {
			t.Run(writerName+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				received := make(chan string, 1)
				router := canonicalPathRouter(t, received)

				writer, status := newWriter(t)
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
					"http://app.example.com"+tt.path, nil)

				proxy.NewHandler(router).ServeHTTP(writer, req)

				require.Equal(t, http.StatusOK, status())
				assert.Equal(t, tt.want, <-received)
			})
		}
	}
}

// escapedRequest builds a request whose URL is parsed from target, so an
// escaped path keeps its RawPath as it does off the wire.
func escapedRequest(t *testing.T, target, matchedPrefix string) *http.Request {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com"+target, nil)

	return proxy.SetMatchedPrefix(req, matchedPrefix)
}

func TestURLRewriter_KeepsEscapedPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rewrite proxy.URLRewritePath
		target  string
		want    string
	}{
		{
			name:    "full path value is an escaped path",
			rewrite: proxy.URLRewritePath{Type: proxy.URLRewriteFullPath, ReplaceFullPath: new("/caf%C3%A9")},
			target:  "/any",
			want:    "/caf%C3%A9",
		},
		{
			name:    "full path value that does not unescape is literal",
			rewrite: proxy.URLRewritePath{Type: proxy.URLRewriteFullPath, ReplaceFullPath: new("/50%off")},
			target:  "/any",
			want:    "/50%25off",
		},
		{
			name:    "prefix replacement keeps an escaped slash in the suffix",
			rewrite: proxy.URLRewritePath{Type: proxy.URLRewritePrefixMatch, ReplacePrefixMatch: new("/v2")},
			target:  "/app/a%2Fb",
			want:    "/v2/a%2Fb",
		},
		{
			name:    "prefix replacement that does not unescape keeps the suffix escapes",
			rewrite: proxy.URLRewritePath{Type: proxy.URLRewritePrefixMatch, ReplacePrefixMatch: new("/50%off")},
			target:  "/app/caf%C3%A9",
			want:    "/50%25off/caf%C3%A9",
		},
		{
			name:    "prefix replacement consumes an escape inside the prefix",
			rewrite: proxy.URLRewritePath{Type: proxy.URLRewritePrefixMatch, ReplacePrefixMatch: new("/v2")},
			target:  "/%61pp/x",
			want:    "/v2/x",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := escapedRequest(t, tt.target, "/app")

			resp := proxy.NewURLRewriter(&proxy.URLRewriteConfig{Path: &tt.rewrite}).ProcessRequest(req) //nolint:bodyclose // rewriter returns nil response
			assert.Nil(t, resp)
			assert.Equal(t, tt.want, req.URL.EscapedPath())
		})
	}
}

func TestRequestRedirect_KeepsEscapedPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		path   proxy.RedirectPath
		target string
		want   string
	}{
		{
			name:   "full path value is an escaped path",
			path:   proxy.RedirectPath{Type: proxy.RedirectPathFullReplace, Value: "/caf%C3%A9"},
			target: "/any",
			want:   "https://example.com/caf%C3%A9",
		},
		{
			name:   "full path value that does not unescape is literal",
			path:   proxy.RedirectPath{Type: proxy.RedirectPathFullReplace, Value: "/50%off"},
			target: "/any",
			want:   "https://example.com/50%25off",
		},
		{
			name:   "prefix replacement keeps an escaped slash in the suffix",
			path:   proxy.RedirectPath{Type: proxy.RedirectPathPrefixReplace, Value: "/v2"},
			target: "/app/a%2Fb",
			want:   "https://example.com/v2/a%2Fb",
		},
		{
			name:   "prefix replacement that does not unescape keeps the suffix escapes",
			path:   proxy.RedirectPath{Type: proxy.RedirectPathPrefixReplace, Value: "/50%off"},
			target: "/app/caf%C3%A9",
			want:   "https://example.com/50%25off/caf%C3%A9",
		},
		{
			name:   "prefix replacement consumes an escape inside the prefix",
			path:   proxy.RedirectPath{Type: proxy.RedirectPathPrefixReplace, Value: "/v2"},
			target: "/%61pp/x",
			want:   "https://example.com/v2/x",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			resp := proxy.NewRequestRedirect(&proxy.RedirectConfig{Path: &tt.path}).ProcessRequest(escapedRequest(t, tt.target, "/app"))
			require.NotNil(t, resp)

			defer resp.Body.Close()

			assert.Equal(t, tt.want, resp.Header.Get("Location"))
		})
	}
}

func TestHandler_AccessLogRecordsPathAsSent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	received := make(chan string, 1)
	handler := proxy.NewHandler(canonicalPathRouter(t, received),
		proxy.WithAccessLog(slog.New(slog.NewJSONHandler(&buf, nil)), 1.0))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/app/./x", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, "app /v2/x", <-received)
	assert.Contains(t, buf.String(), `"path":"/app/./x"`)
}

func TestRouter_PrefixPrecedenceUsesCanonicalLength(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/%61%61%61"}}},
				Backends:  []proxy.BackendRef{{URL: "http://short.example", Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/aaa/b"}}},
				Backends:  []proxy.BackendRef{{URL: "http://long.example", Weight: 1}},
			},
		},
	}))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/aaa/b/c", nil)

	result := router.Route(req)
	require.NotNil(t, result)
	assert.Equal(t, "http://long.example", result.Rule.Backends[result.BackendIdx].URL)
}
