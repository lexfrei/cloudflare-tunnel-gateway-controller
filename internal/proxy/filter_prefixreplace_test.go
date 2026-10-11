package proxy_test

import (
	"net/http"
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

// emptyElementRows cover request paths the spec's table does not list: an
// empty path element survives, because a substitution does not collapse it.
func emptyElementRows() []prefixReplaceCase {
	return []prefixReplaceCase{
		{"empty path element survives", "/public//x", "/public", "/internal", "/internal//x"},
		{"empty element directly after the prefix", "/public//", "/public", "/internal", "/internal//"},
	}
}

func TestURLRewriter_ReplacePrefixMatchFollowsSpecTable(t *testing.T) {
	t.Parallel()

	for _, tt := range append(specTableRows(), emptyElementRows()...) {
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

	for _, tt := range append(specTableRows(), emptyElementRows()...) {
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
