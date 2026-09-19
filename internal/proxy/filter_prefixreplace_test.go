package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// prefixReplaceCase is one row of the ReplacePrefixMatch table in the
// Gateway API spec (HTTPPathModifier.ReplacePrefixMatch, vendored at
// vendor/sigs.k8s.io/gateway-api/apis/v1/httproute_types.go). The
// URLRewrite filter and the RequestRedirect filter implement that same
// table, so the rows are shared between the two tests below.
type prefixReplaceCase struct {
	name        string
	requestPath string
	prefix      string
	replacement string
	want        string
}

// specTableRows is the spec's table verbatim, in its published order.
func specTableRows() []prefixReplaceCase {
	return []prefixReplaceCase{
		{"suffix with bare prefix and bare replacement", "/foo/bar", "/foo", "/xyz", "/xyz/bar"},
		{"suffix with bare prefix and trailing-slash replacement", "/foo/bar", "/foo", "/xyz/", "/xyz/bar"},
		{"suffix with trailing-slash prefix and bare replacement", "/foo/bar", "/foo/", "/xyz", "/xyz/bar"},
		{"suffix with trailing-slash prefix and replacement", "/foo/bar", "/foo/", "/xyz/", "/xyz/bar"},
		{"whole path is the prefix", "/foo", "/foo", "/xyz", "/xyz"},
		{"trailing slash on the request path is preserved", "/foo/", "/foo", "/xyz", "/xyz/"},
		{"empty replacement keeps the suffix", "/foo/bar", "/foo", "", "/bar"},
		{"empty replacement with trailing-slash request path", "/foo/", "/foo", "", "/"},
		{"empty replacement with no suffix", "/foo", "/foo", "", "/"},
		{"root replacement with trailing-slash request path", "/foo/", "/foo", "/", "/"},
		{"root replacement with no suffix", "/foo", "/foo", "/", "/"},
	}
}

// unnormalisedRows cover request paths the spec's table does not list
// but whose handling follows from the same rule it states: the filter
// substitutes the matched prefix and leaves the rest alone. Dot
// segments stay unresolved, because resolving them moves the request
// outside the prefix the route selected it by, and an empty path
// element survives, because a route with no rewrite filter forwards one
// to the backend as sent.
func unnormalisedRows() []prefixReplaceCase {
	return []prefixReplaceCase{
		{"parent segment stays inside the replacement", "/public/../admin", "/public", "/internal", "/internal/../admin"},
		{"parent segment at the tail", "/public/docs/..", "/public", "/internal", "/internal/docs/.."},
		{"current-directory segment is preserved", "/public/./x", "/public", "/internal", "/internal/./x"},
		{"repeated parent segments stay inside", "/public/a/../../etc", "/public", "/internal", "/internal/a/../../etc"},
		{"empty path element survives", "/public//x", "/public", "/internal", "/internal//x"},
		{"empty element directly after the prefix", "/public//", "/public", "/internal", "/internal//"},
	}
}

func TestURLRewriter_ReplacePrefixMatchFollowsSpecTable(t *testing.T) {
	t.Parallel()

	for _, tt := range append(specTableRows(), unnormalisedRows()...) {
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
			req = proxy.SetMatchedPrefix(req, tt.prefix)

			resp := filter.ProcessRequest(req) //nolint:bodyclose // rewriter returns nil response
			assert.Nil(t, resp)
			assert.Equal(t, tt.want, req.URL.Path)
		})
	}
}

func TestRequestRedirect_ReplacePrefixMatchFollowsSpecTable(t *testing.T) {
	t.Parallel()

	for _, tt := range append(specTableRows(), unnormalisedRows()...) {
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
			req = proxy.SetMatchedPrefix(req, tt.prefix)

			resp := filter.ProcessRequest(req)
			require.NotNil(t, resp)

			defer resp.Body.Close()

			location, err := url.Parse(resp.Header.Get("Location"))
			require.NoError(t, err)
			assert.Equal(t, tt.want, location.Path)
		})
	}
}

// TestHandler_ReplacePrefixMatch_TunnelMode_ForwardsDotSegmentsVerbatim
// drives the rewrite through the cloudflared HTTP/2 response writer
// rather than httptest's HTTP/1.1 one, and reads what the backend
// received off the wire instead of trusting the filter's own view of
// req.URL. Either half alone would miss a regression: a filter that
// resolves the dot segments hands the backend a request outside the
// route's replacement prefix, and only the backend's RequestURI shows
// that.
func TestHandler_ReplacePrefixMatch_TunnelMode_ForwardsDotSegmentsVerbatim(t *testing.T) {
	t.Parallel()

	received := make(chan string, 1)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.RequestURI
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Matches: []proxy.RouteMatch{
				{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/public"}},
			},
			Filters: []proxy.RouteFilter{{
				Type: proxy.FilterURLRewrite,
				URLRewrite: &proxy.URLRewriteConfig{
					Path: &proxy.URLRewritePath{
						Type:               proxy.URLRewritePrefixMatch,
						ReplacePrefixMatch: new("/internal"),
					},
				},
			}},
			Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	fake := newFakeCloudflaredRespWriter()
	t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://app.example.com/public/../admin", nil)

	proxy.NewHandler(router).ServeHTTP(fake, req)

	assert.Equal(t, http.StatusOK, fake.Status())
	assert.Equal(t, "/internal/../admin", <-received,
		"the backend must see the client's dot segments spliced under the replacement prefix; "+
			"resolving them in the proxy delivers /admin, outside the prefix the route matched on")
}
