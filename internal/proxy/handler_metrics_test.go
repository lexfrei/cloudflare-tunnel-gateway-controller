package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// newMetricsHandler builds a Router + Handler pair instrumented with a fresh
// registry and returns all three. The config routes app.example.com to the
// given backend URL.
func newMetricsHandler(t *testing.T, backendURL string) (*proxy.Router, *proxy.Handler, *prometheus.Registry) {
	t.Helper()

	reg := prometheus.NewRegistry()
	metrics := proxy.NewMetrics(reg)

	router := proxy.NewRouter()
	handler := proxy.NewHandler(router, proxy.WithMetrics(metrics))
	router.SetHandler(handler)

	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{URL: backendURL, Weight: 1, Protocol: proxy.BackendProtocolHTTP},
				},
			},
		},
	}))

	return router, handler, reg
}

// gatherValue reads a single sample value (counter or gauge) for the metric
// family name whose labels are a superset match of want. Returns 0 when the
// series does not exist.
func gatherValue(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()

	families, err := reg.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != name {
			continue
		}

		for _, metric := range family.GetMetric() {
			if !labelsMatch(metric, want) {
				continue
			}

			switch {
			case metric.GetCounter() != nil:
				return metric.GetCounter().GetValue()
			case metric.GetGauge() != nil:
				return metric.GetGauge().GetValue()
			}
		}
	}

	return 0
}

// gatherHistogramCount reads the sample count of a histogram series.
func gatherHistogramCount(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) uint64 {
	t.Helper()

	families, err := reg.Gather()
	require.NoError(t, err)

	for _, family := range families {
		if family.GetName() != name {
			continue
		}

		for _, metric := range family.GetMetric() {
			if labelsMatch(metric, want) && metric.GetHistogram() != nil {
				return metric.GetHistogram().GetSampleCount()
			}
		}
	}

	return 0
}

func labelsMatch(metric *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(metric.GetLabel()))
	for _, pair := range metric.GetLabel() {
		got[pair.GetName()] = pair.GetValue()
	}

	for key, val := range want {
		if got[key] != val {
			return false
		}
	}

	return true
}

// TestHandlerMetrics_StatusClassesAndDuration pins the per-request counting
// contract: one requests_total increment in the right status class, one
// duration observation, response bytes accumulated — all labelled with the
// MATCHED config hostname, not the raw request Host.
func TestHandlerMetrics_StatusClassesAndDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		wantClass string
	}{
		{name: "200 counts as 2xx", status: http.StatusOK, wantClass: "2xx"},
		{name: "301 counts as 3xx", status: http.StatusMovedPermanently, wantClass: "3xx"},
		{name: "404 counts as 4xx", status: http.StatusNotFound, wantClass: "4xx"},
		{name: "503 counts as 5xx", status: http.StatusServiceUnavailable, wantClass: "5xx"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte("payload"))
			}))
			t.Cleanup(backend.Close)

			_, handler, reg := newMetricsHandler(t, backend.URL)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/x", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			labels := map[string]string{"hostname": "app.example.com", "status_class": tt.wantClass}
			assert.InDelta(t, 1.0, gatherValue(t, reg, "cftunnel_proxy_requests_total", labels), 0.001)

			hostOnly := map[string]string{"hostname": "app.example.com"}
			assert.Equal(t, uint64(1), gatherHistogramCount(t, reg, "cftunnel_proxy_request_duration_seconds", hostOnly))
			assert.Greater(t, gatherValue(t, reg, "cftunnel_proxy_response_bytes_total", hostOnly), 0.0)
			assert.InDelta(t, 0.0, gatherValue(t, reg, "cftunnel_proxy_requests_in_flight", nil), 0.001,
				"in-flight must return to zero after the request completes")
		})
	}
}

// TestHandlerMetrics_InFlightGauge pins the saturation signal: the gauge is 1
// while a request is being served and 0 after.
func TestHandlerMetrics_InFlightGauge(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	entered := make(chan struct{})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	_, handler, reg := newMetricsHandler(t, backend.URL)

	done := make(chan struct{})

	go func() {
		defer close(done)

		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/slow", nil)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}()

	<-entered
	assert.InDelta(t, 1.0, gatherValue(t, reg, "cftunnel_proxy_requests_in_flight", nil), 0.001,
		"gauge must be 1 mid-request")

	close(release)
	<-done

	assert.InDelta(t, 0.0, gatherValue(t, reg, "cftunnel_proxy_requests_in_flight", nil), 0.001,
		"gauge must drop back to 0")
}

// TestHandlerMetrics_UnmatchedRouteUsesEmptyHostname pins the cardinality
// guard: an arbitrary client Host that matches no route must NOT mint a new
// hostname series — it lands on the "" label.
func TestHandlerMetrics_UnmatchedRouteUsesEmptyHostname(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(backend.Close)

	_, handler, reg := newMetricsHandler(t, backend.URL)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://evil-cardinality-bomb.example.net/x", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)

	labels := map[string]string{"hostname": "", "status_class": "4xx"}
	assert.InDelta(t, 1.0, gatherValue(t, reg, "cftunnel_proxy_requests_total", labels), 0.001)

	bomb := map[string]string{"hostname": "evil-cardinality-bomb.example.net"}
	assert.InDelta(t, 0.0, gatherValue(t, reg, "cftunnel_proxy_requests_total", bomb), 0.001,
		"raw client Host must never appear as a label value")
}

// TestHandlerMetrics_WildcardHostnameLabel pins that a wildcard-routed request
// is labelled with the configured PATTERN (*.example.com), not the concrete
// per-request host — the other half of the cardinality guard.
func TestHandlerMetrics_WildcardHostnameLabel(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	reg := prometheus.NewRegistry()
	metrics := proxy.NewMetrics(reg)
	router := proxy.NewRouter()
	handler := proxy.NewHandler(router, proxy.WithMetrics(metrics))
	router.SetHandler(handler)

	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"*.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolHTTP}},
			},
		},
	}))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://tenant-12345.example.com/x", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	pattern := map[string]string{"hostname": "*.example.com", "status_class": "2xx"}
	assert.InDelta(t, 1.0, gatherValue(t, reg, "cftunnel_proxy_requests_total", pattern), 0.001)

	concrete := map[string]string{"hostname": "tenant-12345.example.com"}
	assert.InDelta(t, 0.0, gatherValue(t, reg, "cftunnel_proxy_requests_total", concrete), 0.001)
}

// TestHandlerMetrics_BackendDialErrorCounted pins the backend-error counter:
// a dead backend increments backend_errors_total{reason="dial"} and the
// request lands in the 5xx class.
func TestHandlerMetrics_BackendDialErrorCounted(t *testing.T) {
	t.Parallel()

	// Reserve a port, then close the listener so the dial is refused.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	_, handler, reg := newMetricsHandler(t, deadURL)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/x", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code)

	errLabels := map[string]string{"hostname": "app.example.com", "reason": "dial"}
	assert.InDelta(t, 1.0, gatherValue(t, reg, "cftunnel_proxy_backend_errors_total", errLabels), 0.001)

	classLabels := map[string]string{"hostname": "app.example.com", "status_class": "5xx"}
	assert.InDelta(t, 1.0, gatherValue(t, reg, "cftunnel_proxy_requests_total", classLabels), 0.001)
}

// TestHandlerMetrics_RequestBodyBytesCounted pins request_bytes_total: bytes
// actually read from the client body are accumulated.
func TestHandlerMetrics_RequestBodyBytesCounted(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = http.MaxBytesReader(w, r.Body, 1<<20).Read(make([]byte, 1<<10))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	_, handler, reg := newMetricsHandler(t, backend.URL)

	body := strings.NewReader("0123456789")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.com/upload", body)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	hostOnly := map[string]string{"hostname": "app.example.com"}
	assert.InDelta(t, 10.0, gatherValue(t, reg, "cftunnel_proxy_request_bytes_total", hostOnly), 0.001)
}

// TestHandlerMetrics_WebSocketDialFailure_InFlightReturnsToZero pins that a WS
// upgrade whose backend dial fails releases the in-flight gauge and never
// increments the session gauge — the one WS path where a future refactor could
// leak the gauge (no onUpgrade fires, so finish() must run the non-upgraded
// release).
func TestHandlerMetrics_WebSocketDialFailure_InFlightReturnsToZero(t *testing.T) {
	t.Parallel()

	// Reserve a port, then close the listener so the WS backend dial is refused.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	reg := prometheus.NewRegistry()
	metrics := proxy.NewMetrics(reg)
	router := proxy.NewRouter()
	handler := proxy.NewHandler(router, proxy.WithMetrics(metrics))
	router.SetHandler(handler)

	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{URL: deadURL, Weight: 1, Protocol: proxy.BackendProtocolHTTP, WebSocket: true},
				},
			},
		},
	}))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", rfc6455SampleWSKey)

	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.InDelta(t, 0.0, gatherValue(t, reg, "cftunnel_proxy_requests_in_flight", nil), 0.001,
		"a failed WS dial must release the in-flight gauge — no upgrade fired")
	assert.InDelta(t, 0.0, gatherValue(t, reg, "cftunnel_proxy_websocket_active_sessions", nil), 0.001,
		"a failed WS dial must not enter the session gauge")
	assert.InDelta(t, 1.0, gatherValue(t, reg, "cftunnel_proxy_backend_errors_total",
		map[string]string{"hostname": "app.example.com", "reason": "ws_dial"}), 0.001)
}

// TestHandlerMetrics_WebSocketUpgrade_TunnelMode pins the hijack-path
// accounting against the cloudflared HTTP/2 writer contract: at upgrade time
// the request leaves the in-flight gauge, enters the websocket session gauge,
// counts as 1xx, and observes time-to-upgrade; when the session ends the
// session gauge returns to zero. Driven through fakeCloudflaredRespWriter so
// the tunnel-mode WriteHeader-before-Hijack semantics are part of the pinned
// behaviour.
func TestHandlerMetrics_WebSocketUpgrade_TunnelMode(t *testing.T) {
	t.Parallel()

	backend := newWSEchoBackend(t, false)

	reg := prometheus.NewRegistry()
	metrics := proxy.NewMetrics(reg)
	router := proxy.NewRouter()
	handler := proxy.NewHandler(router, proxy.WithMetrics(metrics))
	router.SetHandler(handler)

	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches:   []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolHTTP, WebSocket: true},
				},
			},
		},
	}))

	fake := newFakeCloudflaredRespWriter()
	t.Cleanup(func() {
		_ = fake.serverSide.Close()
		_ = fake.clientSide.Close()
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", rfc6455SampleWSKey)

	handlerDone := make(chan struct{})

	go func() {
		defer close(handlerDone)
		handler.ServeHTTP(fake, req)
	}()

	require.Eventually(t, fake.Hijacked, 5*time.Second, 25*time.Millisecond)

	hostOnly := map[string]string{"hostname": "app.example.com"}

	assert.InDelta(t, 1.0, gatherValue(t, reg, "cftunnel_proxy_websocket_active_sessions", nil), 0.001,
		"session gauge must be 1 while the WS session is live")
	assert.InDelta(t, 0.0, gatherValue(t, reg, "cftunnel_proxy_requests_in_flight", nil), 0.001,
		"in-flight must hand off to the session gauge at upgrade time")
	assert.InDelta(t, 1.0, gatherValue(t, reg, "cftunnel_proxy_requests_total",
		map[string]string{"hostname": "app.example.com", "status_class": "1xx"}), 0.001,
		"the upgrade counts as a completed 1xx exchange at hijack time")
	assert.Equal(t, uint64(1), gatherHistogramCount(t, reg, "cftunnel_proxy_request_duration_seconds", hostOnly),
		"duration must observe time-to-upgrade, exactly once")

	_ = fake.HijackedClient().Close()
	<-handlerDone

	assert.InDelta(t, 0.0, gatherValue(t, reg, "cftunnel_proxy_websocket_active_sessions", nil), 0.001,
		"session gauge must return to zero after the session ends")
	assert.Equal(t, uint64(1), gatherHistogramCount(t, reg, "cftunnel_proxy_request_duration_seconds", hostOnly),
		"session end must NOT observe a second duration sample")
}
