package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// knativeProbeBackend is an httptest backend that counts hits and 404s every
// request, standing in for the user app / activator that the OLD forwarding
// path reached (and that 404'd or hung the probe).
func knativeProbeBackend(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writer.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	return server, &hits
}

// knativeEndpointProbeConfig builds a config mirroring a net-gateway-api
// DomainMapping endpoint-probe rule: header-match K-Network-Hash: override +
// path /.well-known/knative/revision/<ns>/<svc>, RequestHeaderModifier setting
// K-Network-Hash to the concrete (ep-/tr- prefixed) version, backend = the
// ksvc Service (here, the test backend that would 404 the probe).
func knativeEndpointProbeConfig(host, path, concreteHash, backendURL string) *proxy.Config {
	return &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{host},
			Matches: []proxy.RouteMatch{{
				Path:    &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: path},
				Headers: []proxy.HeaderMatch{{Type: proxy.HeaderMatchExact, Name: "K-Network-Hash", Value: "override"}},
			}},
			Filters: []proxy.RouteFilter{{
				Type:                  proxy.FilterRequestHeaderModifier,
				RequestHeaderModifier: &proxy.HeaderModifier{Set: []proxy.HeaderValue{{Name: "K-Network-Hash", Value: concreteHash}}},
			}},
			Backends: []proxy.BackendRef{{URL: backendURL, Weight: 1}},
		}},
	}
}

func newKnativeProbeRequest(t *testing.T, host, path string) *http.Request {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+host+path, nil)
	req.Host = host
	req.Header.Set("User-Agent", "Knative-Ingress-Probe")
	req.Header.Set("K-Network-Probe", "probe")
	req.Header.Set("K-Network-Hash", "override")

	return req
}

// TestHandler_KnativeReadinessProbe_InCluster_AnsweredAuthoritatively is the
// core fix: an in-cluster probe to the endpoint-probe path is answered 200 with
// the rule's concrete hash, and the backend is NEVER dialed (so scale-to-zero
// and the ExternalName loop cannot break it).
func TestHandler_KnativeReadinessProbe_InCluster_AnsweredAuthoritatively(t *testing.T) {
	t.Parallel()

	const host = "abcd.foobar76.example.com"
	const path = "/.well-known/knative/revision/default/static-sws-site77"
	const concreteHash = "ep-4d38b8fcfb28f82a830f349e6073eea79af03222262be5785b2a9216d39f63d9"

	backend, hits := knativeProbeBackend(t)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(knativeEndpointProbeConfig(host, path, concreteHash, backend.URL)))

	handler := proxy.InClusterProbeMiddleware(proxy.NewHandler(router))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, newKnativeProbeRequest(t, host, path))

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, concreteHash, recorder.Header().Get("K-Network-Hash"),
		"the gateway must echo the matched rule's concrete hash so the prober converges")
	assert.Equal(t, 0, recorder.Body.Len(), "the probe ack body must be empty")
	assert.Equal(t, int64(0), hits.Load(), "the probe MUST NOT be forwarded to the backend")
}

// TestHandler_KnativeReadinessProbe_NotInCluster_Forwarded proves the security
// scope: the SAME probe arriving on the tunnel/edge path (no in-cluster flag) is
// NOT short-circuited — it is forwarded like ordinary traffic, so an external
// client cannot forge the headers to extract the hash or get a synthetic 200.
func TestHandler_KnativeReadinessProbe_NotInCluster_Forwarded(t *testing.T) {
	t.Parallel()

	const host = "abcd.foobar76.example.com"
	const path = "/.well-known/knative/revision/default/static-sws-site77"

	backend, hits := knativeProbeBackend(t)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(knativeEndpointProbeConfig(host, path, "ep-abc", backend.URL)))

	// Bare handler (no InClusterProbeMiddleware) == the tunnel/edge path.
	handler := proxy.NewHandler(router)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, newKnativeProbeRequest(t, host, path))

	assert.Equal(t, int64(1), hits.Load(), "edge probes MUST be forwarded, not answered authoritatively")
	assert.Empty(t, recorder.Header().Get("K-Network-Hash"),
		"the edge path must not synthesize the hash header")
}

// TestHandler_KnativeProbe_NonProbeRequestForwarded confirms ordinary traffic on
// the in-cluster listener (no probe headers) is untouched and proxied.
func TestHandler_KnativeProbe_NonProbeRequestForwarded(t *testing.T) {
	t.Parallel()

	const host = "abcd.foobar76.example.com"

	backend, hits := knativeProbeBackend(t)

	// Catch-all rule that WOULD be answerable (carries a K-Network-Hash Set
	// filter), so the only thing keeping ordinary traffic from being spoofed is
	// the probe-header gate.
	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{host},
			Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
			Filters: []proxy.RouteFilter{{
				Type:                  proxy.FilterRequestHeaderModifier,
				RequestHeaderModifier: &proxy.HeaderModifier{Set: []proxy.HeaderValue{{Name: "K-Network-Hash", Value: "ep-abc"}}},
			}},
			Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	handler := proxy.InClusterProbeMiddleware(proxy.NewHandler(router))

	// A request WITHOUT the probe headers must be forwarded, never spoofed.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+host+"/", nil)
	req.Host = host
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	assert.Equal(t, int64(1), hits.Load(), "non-probe traffic must be forwarded to the backend")
	assert.Empty(t, recorder.Header().Get("K-Network-Hash"),
		"non-probe traffic must never receive a synthesized hash")
}

// TestHandler_KnativeProbe_NoSetFilter_FallsThrough guards the boundary: a probe
// matching a rule that has NO K-Network-Hash Set filter must NOT be 200-spoofed;
// it falls through to forwarding. Real net-gateway-api output always stamps the
// Set, so this only protects against accidental short-circuits.
func TestHandler_KnativeProbe_NoSetFilter_FallsThrough(t *testing.T) {
	t.Parallel()

	const host = "plain.example.com"

	backend, hits := knativeProbeBackend(t)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{host},
			Matches: []proxy.RouteMatch{{
				Headers: []proxy.HeaderMatch{{Type: proxy.HeaderMatchExact, Name: "K-Network-Probe", Value: "probe"}},
			}},
			Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	handler := proxy.InClusterProbeMiddleware(proxy.NewHandler(router))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, newKnativeProbeRequest(t, host, "/"))

	assert.Equal(t, int64(1), hits.Load(), "a probe with no hash-set filter must be forwarded, not spoofed")
	assert.Empty(t, recorder.Header().Get("K-Network-Hash"))
}

// TestHandler_KnativeProbe_DomainMappingRootRule_AlsoAnswered covers the "/"
// DomainMapping probe rule, which additionally carries a URLRewrite. It must be
// answered authoritatively WITHOUT running the rewrite or dialing the backend.
func TestHandler_KnativeProbe_DomainMappingRootRule_AlsoAnswered(t *testing.T) {
	t.Parallel()

	const host = "abcd.foobar76.example.com"
	const concreteHash = "ep-rootrule"

	backend, hits := knativeProbeBackend(t)

	rewriteHost := "static-sws-site77.default.svc.cluster.local"
	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{host},
			Matches: []proxy.RouteMatch{{
				Path:    &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"},
				Headers: []proxy.HeaderMatch{{Type: proxy.HeaderMatchExact, Name: "K-Network-Hash", Value: "override"}},
			}},
			Filters: []proxy.RouteFilter{
				{Type: proxy.FilterRequestHeaderModifier, RequestHeaderModifier: &proxy.HeaderModifier{Set: []proxy.HeaderValue{{Name: "K-Network-Hash", Value: concreteHash}}}},
				{Type: proxy.FilterURLRewrite, URLRewrite: &proxy.URLRewriteConfig{Hostname: &rewriteHost}},
			},
			Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	handler := proxy.InClusterProbeMiddleware(proxy.NewHandler(router))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, newKnativeProbeRequest(t, host, "/"))

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, concreteHash, recorder.Header().Get("K-Network-Hash"))
	assert.Equal(t, int64(0), hits.Load(), "the root probe rule must be answered before URLRewrite/forward")
}
