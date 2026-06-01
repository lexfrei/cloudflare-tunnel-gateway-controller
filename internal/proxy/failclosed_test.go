package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// failClosedConfig builds a single-rule config whose rule is marked
// fail-closed (UnavailableStatus) while still carrying a real backend, so a
// passing assertion proves the rule-level short-circuit wins over backend
// selection rather than coincidentally erroring on a dead backend.
func failClosedConfig() *proxy.Config {
	return &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Hostnames:         []string{"app.example.com"},
				Matches:           []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				UnavailableStatus: http.StatusInternalServerError,
				Backends:          []proxy.BackendRef{{URL: "http://backend.default.svc.cluster.local:8080", Weight: 1}},
			},
		},
	}
}

// TestRuleUnavailableStatusFailsClosed verifies the rule-level fail-closed
// primitive: a matched rule carrying UnavailableStatus returns that status for
// every matched request instead of selecting a backend. The proxy converter
// sets this when a rule cannot be served as written (e.g. it carries an
// unsupported filter type), per the Gateway API requirement that such requests
// MUST receive an HTTP error response rather than be served silently.
func TestRuleUnavailableStatusFailsClosed(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(failClosedConfig()))
	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestRuleUnavailableStatusFailsClosed_TunnelWriter exercises the same
// fail-closed path through the cloudflared HTTP/2 response-writer fake (the
// production tunnel path), not just httptest's HTTP/1.1 writer, per the project
// design principle that any response-flow feature is validated on the real
// transport contract.
func TestRuleUnavailableStatusFailsClosed_TunnelWriter(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(failClosedConfig()))
	handler := proxy.NewHandler(router)

	fake := newFakeCloudflaredRespWriter()
	t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	handler.ServeHTTP(fake, req)

	assert.Equal(t, http.StatusInternalServerError, fake.Status())
}
