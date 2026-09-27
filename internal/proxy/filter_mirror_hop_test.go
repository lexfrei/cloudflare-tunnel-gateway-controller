package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestRequestMirror_DropsHopByHopHeaders pins that the mirror copy leaves the
// client's hop-by-hop headers behind, as the primary leg does: the fixed
// RFC 7230 set and every header the client's Connection names. A TE of
// "trailers" survives, matching what httputil.ReverseProxy forwards.
func TestRequestMirror_DropsHopByHopHeaders(t *testing.T) {
	t.Parallel()

	seen := make(chan http.Header, 1)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	mirror := proxy.NewRequestMirror(backend.URL, nil, nil, "", nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/x", nil)
	req.Header.Set("Connection", "Upgrade, X-Named-Hop")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("X-Named-Hop", "1")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Proxy-Connection", "keep-alive")
	req.Header.Set("TE", "trailers")
	req.Header.Set("X-End-To-End", "kept")

	require.Nil(t, mirror.ProcessRequest(req)) //nolint:bodyclose // mirror returns nil response

	var got http.Header

	select {
	case got = <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the mirror copy never arrived")
	}

	for _, name := range []string{"Connection", "Upgrade", "X-Named-Hop", "Keep-Alive", "Proxy-Connection"} {
		assert.Empty(t, got.Values(name), "%s must not reach the mirror backend", name)
	}

	assert.Equal(t, "trailers", got.Get("TE"))
	assert.Equal(t, "kept", got.Get("X-End-To-End"))

	assert.Equal(t, "websocket", req.Header.Get("Upgrade"), "the primary request keeps its own headers")
}

// TestRequestMirror_KeepsForwardingHeadersNamedInConnection pins that a client
// naming the forwarding headers in Connection cannot strip them from the
// mirror copy. The primary leg restores them after its own hop-by-hop pass,
// and the mirror copy must match.
func TestRequestMirror_KeepsForwardingHeadersNamedInConnection(t *testing.T) {
	t.Parallel()

	seen := make(chan http.Header, 1)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	mirror := proxy.NewRequestMirror(backend.URL, nil, nil, "", nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/x", nil)
	req.Header.Set("Connection", "X-Forwarded-For, X-Forwarded-Proto, Forwarded, X-Forwarded-Host")
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Forwarded", "for=203.0.113.7")
	req.Header.Set("X-Forwarded-Host", "app.example.com")

	require.Nil(t, mirror.ProcessRequest(req)) //nolint:bodyclose // mirror returns nil response

	var got http.Header

	select {
	case got = <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the mirror copy never arrived")
	}

	assert.Equal(t, "203.0.113.7, 192.0.2.1", got.Get("X-Forwarded-For"))
	assert.Equal(t, "https", got.Get("X-Forwarded-Proto"))
	assert.Equal(t, "for=203.0.113.7", got.Get("Forwarded"))
	assert.Equal(t, "app.example.com", got.Get("X-Forwarded-Host"))
	assert.Empty(t, got.Values("Connection"))
}

// TestRequestMirror_AppendsPeerToXForwardedFor pins that the mirror copy
// carries the same X-Forwarded-For as the primary leg: the inbound chain with
// the immediate peer appended, the peer alone when there is no inbound chain,
// and the chain unchanged when RemoteAddr has no parseable host.
func TestRequestMirror_AppendsPeerToXForwardedFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		inbound    string
		remoteAddr string
		want       string
	}{
		{name: "chain plus peer", inbound: "203.0.113.7", remoteAddr: "192.0.2.1:1234", want: "203.0.113.7, 192.0.2.1"},
		{name: "peer only", inbound: "", remoteAddr: "192.0.2.1:1234", want: "192.0.2.1"},
		{name: "unparseable peer", inbound: "203.0.113.7", remoteAddr: "", want: "203.0.113.7"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			seen := make(chan http.Header, 1)

			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Clone()

				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(backend.Close)

			mirror := proxy.NewRequestMirror(backend.URL, nil, nil, "", nil)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/x", nil)
			if tt.inbound != "" {
				req.Header.Set("X-Forwarded-For", tt.inbound)
			}

			req.RemoteAddr = tt.remoteAddr

			require.Nil(t, mirror.ProcessRequest(req)) //nolint:bodyclose // mirror returns nil response

			select {
			case got := <-seen:
				assert.Equal(t, tt.want, got.Get("X-Forwarded-For"))
			case <-time.After(5 * time.Second):
				t.Fatal("the mirror copy never arrived")
			}

			assert.Equal(t, tt.inbound, req.Header.Get("X-Forwarded-For"), "the primary request keeps its own header")
		})
	}
}
