package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestConfigAPI_MetricsEndpoint_NoAuthRequired pins the /metrics contract:
// the endpoint is served when a metrics handler is installed and is
// deliberately exempt from Bearer auth (Prometheus scrapes carry no
// credentials; the endpoint exposes no secrets), exactly like /healthz and
// /readyz.
func TestConfigAPI_MetricsEndpoint_NoAuthRequired(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	// Register the proxy instruments; the plain gauges (requests_in_flight,
	// websocket_active_sessions) appear in the exposition even with no
	// samples, which is what the body assertion below relies on.
	proxy.NewMetrics(reg)

	router := proxy.NewRouter()
	api := proxy.NewConfigAPI(router, "secret-token",
		proxy.WithMetricsHandler(promhttp.HandlerFor(reg, promhttp.HandlerOpts{})))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "GET /metrics must not require Authorization")
	assert.Contains(t, rec.Body.String(), "cftunnel_proxy_", "exposition must carry the proxy metric families")
}

// TestConfigAPI_MetricsEndpoint_AbsentWithoutHandler pins that a ConfigAPI
// constructed without WithMetricsHandler serves no /metrics route at all.
func TestConfigAPI_MetricsEndpoint_AbsentWithoutHandler(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	api := proxy.NewConfigAPI(router, "")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}
