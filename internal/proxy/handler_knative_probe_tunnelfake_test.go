package proxy_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestHandler_KnativeReadinessProbe_InClusterListener_Fake validates the
// body-less 200 + K-Network-Hash probe ack against the production cloudflared
// HTTP/2 response-writer contract (fakeCloudflaredRespWriter), not just
// httptest. Per CLAUDE.md "Validate the tunnel transport, not just httptest":
// the ack must never Hijack (the writer rejects Hijack before WriteHeader) and
// never emit 101 (translated to 200). A header-then-WriteHeader(200) with no
// body satisfies both.
func TestHandler_KnativeReadinessProbe_InClusterListener_Fake(t *testing.T) {
	t.Parallel()

	const host = "abcd.foobar76.example.com"
	const path = "/.well-known/knative/revision/default/static-sws-site77"
	const concreteHash = "tr-deadbeefcafef00d"

	backend, hits := knativeProbeBackend(t)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(knativeEndpointProbeConfig(host, path, concreteHash, backend.URL)))

	handler := proxy.InClusterProbeMiddleware(proxy.NewHandler(router))

	fake := newFakeCloudflaredRespWriter()
	handler.ServeHTTP(fake, newKnativeProbeRequest(t, host, path))

	assert.Equal(t, http.StatusOK, fake.Status(), "the probe ack must be 200 on the cloudflared writer")
	assert.Equal(t, concreteHash, fake.Header().Get("K-Network-Hash"))
	assert.Empty(t, fake.Body(), "the probe ack must be body-less")
	assert.False(t, fake.Hijacked(), "the probe ack must never hijack the connection")
	assert.Equal(t, int64(0), hits.Load(), "the probe must not be forwarded to the backend")
}

// TestHandler_KnativeReadinessProbe_TunnelEdge_NotAnswered_Fake locks the
// security scope on the production writer: a forged probe arriving on the
// tunnel/edge path (bare handler, no in-cluster flag) is forwarded like ordinary
// traffic — the edge can never emit the authoritative ack.
func TestHandler_KnativeReadinessProbe_TunnelEdge_NotAnswered_Fake(t *testing.T) {
	t.Parallel()

	const host = "abcd.foobar76.example.com"
	const path = "/.well-known/knative/revision/default/static-sws-site77"

	backend, hits := knativeProbeBackend(t)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(knativeEndpointProbeConfig(host, path, "ep-abc", backend.URL)))

	// Bare handler == the cloudflared tunnel/edge path.
	handler := proxy.NewHandler(router)

	fake := newFakeCloudflaredRespWriter()
	handler.ServeHTTP(fake, newKnativeProbeRequest(t, host, path))

	assert.Equal(t, int64(1), hits.Load(), "edge probes must be forwarded, not answered authoritatively")
	assert.Equal(t, http.StatusNotFound, fake.Status(), "the edge path returns the backend's response verbatim")
	assert.Empty(t, fake.Header().Get("K-Network-Hash"), "the edge path must not synthesize the hash header")
}
