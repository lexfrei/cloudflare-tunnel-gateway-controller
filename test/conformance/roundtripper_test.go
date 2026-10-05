//go:build conformance

package conformance

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/gateway-api/conformance/utils/roundtripper"
)

// TestBuildEdgeRequestCarriesIntendedScheme pins that the scheme the suite
// asked for reaches the proxy: the request to the edge is always HTTPS, and
// the edge sets X-Forwarded-Proto to match, so a request meant for an HTTP
// listener would otherwise be matched as HTTPS on port 443.
func TestBuildEdgeRequestCarriesIntendedScheme(t *testing.T) {
	t.Parallel()

	for scheme, want := range map[string]string{"http": "http", "https": "https", "": "http"} {
		request := &roundtripper.Request{
			URL:  url.URL{Scheme: scheme, Host: "gw.cfargotunnel.example:8080", Path: "/p"},
			Host: "foo.com:1234",
		}

		req, err := buildEdgeRequest(t.Context(), request, "edge.example.com", "GET")
		require.NoError(t, err)

		assert.Equal(t, "https", req.URL.Scheme)
		assert.Equal(t, "edge.example.com", req.Host)
		assert.Equal(t, "foo.com:1234", req.Header.Get(originalHostHeader))
		assert.Equal(t, want, req.Header.Get(originalProtoHeader), "suite scheme %q", scheme)
		assert.Equal(t, "8080", req.Header.Get(originalPortHeader), "the connection port, not the one in Host")
	}

	for scheme, want := range map[string]string{"http": "80", "https": "443"} {
		request := &roundtripper.Request{URL: url.URL{Scheme: scheme, Host: "gw.cfargotunnel.example", Path: "/p"}}

		req, err := buildEdgeRequest(t.Context(), request, "edge.example.com", "GET")
		require.NoError(t, err)

		assert.Equal(t, want, req.Header.Get(originalPortHeader), "default port of %q", scheme)
	}
}
