package proxy

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errMirrorUnreachable = errors.New("mirror backend unreachable")

// errMirrorTransport fails every mirror dispatch without touching the network.
type errMirrorTransport struct{}

func (errMirrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errMirrorUnreachable
}

// TestMetrics_ContainedEventCountersAreNilSafe pins that the counters can be
// bumped from code that runs with metrics disabled.
func TestMetrics_ContainedEventCountersAreNilSafe(t *testing.T) {
	t.Parallel()

	var metrics *Metrics

	assert.NotPanics(t, func() {
		metrics.containedPanic(panicSiteRequest)
		metrics.mirrorDropped()
	})
}

// TestNewMetrics_PanicSeriesStartAtZero pins that every panic site's series
// exists from the start. A series born at 1 on the first panic makes
// increase() and rate() read 0, so the first panic per site would never alert.
func TestNewMetrics_PanicSeriesStartAtZero(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	NewMetrics(reg)

	families, err := reg.Gather()
	require.NoError(t, err)

	sites := map[string]float64{}

	for _, family := range families {
		if family.GetName() != "cftunnel_proxy_handler_panics_total" {
			continue
		}

		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "site" {
					sites[label.GetValue()] = metric.GetCounter().GetValue()
				}
			}
		}
	}

	assert.Equal(t, map[string]float64{
		panicSiteRequest:       0,
		panicSiteWebSocketCopy: 0,
		panicSiteMirror:        0,
	}, sites)
}

// TestHandler_RecordContainedPanic pins the hook the tunnel adapter calls when
// it contains a panic in the request handler.
func TestHandler_RecordContainedPanic(t *testing.T) {
	t.Parallel()

	metrics := NewMetrics(prometheus.NewRegistry())
	handler := NewHandler(NewRouter(), WithMetrics(metrics))

	handler.RecordContainedPanic()

	assert.InDelta(t, 1, testutil.ToFloat64(metrics.handlerPanics.WithLabelValues(panicSiteRequest)), 0)
	assert.NotPanics(t, NewHandler(NewRouter()).RecordContainedPanic, "metrics disabled")
}

// TestRequestMirror_DropIsCounted pins the metric that replaces the sampled
// drop log as the signal: every refused dispatch counts.
func TestRequestMirror_DropIsCounted(t *testing.T) {
	t.Parallel()

	metrics := NewMetrics(prometheus.NewRegistry())
	filter := &requestMirror{backendURL: "http://mirror.example/", metrics: metrics}
	filter.live.Store(mirrorMaxLiveDispatches)

	for range 3 {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/x", nil)
		require.Nil(t, filter.ProcessRequest(req)) //nolint:bodyclose // mirror returns nil response
	}

	assert.InDelta(t, 3, testutil.ToFloat64(metrics.mirrorDrops), 0)
}

// TestRequestMirror_LimitOverridesTheDefault pins the operator knob: a filter
// with a limit refuses at that limit, not at the built-in one.
func TestRequestMirror_LimitOverridesTheDefault(t *testing.T) {
	t.Parallel()

	newReq := func() *http.Request {
		return httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/x", nil)
	}

	t.Run("at the configured limit the copy is dropped", func(t *testing.T) {
		t.Parallel()

		filter := &requestMirror{backendURL: "http://mirror.example/", limit: 2}
		filter.live.Store(2)

		require.Nil(t, filter.ProcessRequest(newReq())) //nolint:bodyclose // mirror returns nil response
		assert.Equal(t, uint64(1), filter.dropped.Load())
	})

	t.Run("above the built-in cap the copy is dispatched", func(t *testing.T) {
		t.Parallel()

		filter := &requestMirror{
			backendURL: "http://mirror.example/",
			limit:      mirrorMaxLiveDispatches + 10,
			client:     &http.Client{Transport: errMirrorTransport{}},
		}
		filter.live.Store(mirrorMaxLiveDispatches)

		require.Nil(t, filter.ProcessRequest(newReq())) //nolint:bodyclose // mirror returns nil response
		assert.Equal(t, uint64(0), filter.dropped.Load())
	})
}

// TestRequestMirror_DispatchPanicIsCounted pins that a panic contained in the
// mirror goroutine counts under its own site.
func TestRequestMirror_DispatchPanicIsCounted(t *testing.T) {
	t.Parallel()

	metrics := NewMetrics(prometheus.NewRegistry())
	mirror := &requestMirror{
		backendURL: "http://mirror.example.invalid",
		client:     &http.Client{Transport: &panicRoundTripper{reached: make(chan struct{})}},
		metrics:    metrics,
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/mirrored", nil)
	require.Nil(t, mirror.ProcessRequest(req)) //nolint:bodyclose // mirror returns nil response

	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.handlerPanics.WithLabelValues(panicSiteMirror)) == 1
	}, 5*time.Second, 10*time.Millisecond)
}

// newSwitchingBackend accepts one connection, answers its upgrade request with
// 101 and then holds the connection open until the test ends.
func newSwitchingBackend(t *testing.T) string {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	done := make(chan struct{})

	t.Cleanup(func() {
		close(done)
		_ = listener.Close()
	})

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		_, _ = http.ReadRequest(bufio.NewReader(conn))
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))

		<-done
	}()

	return "http://" + listener.Addr().String()
}

// TestHandler_WebSocketCopyPanicIsCounted pins that the Handler hands its
// metrics down to the copy goroutines of a session it upgrades.
func TestHandler_WebSocketCopyPanicIsCounted(t *testing.T) {
	t.Parallel()

	metrics := NewMetrics(prometheus.NewRegistry())
	router := NewRouter()
	require.NoError(t, router.UpdateConfig(&Config{
		Version: 1,
		Rules: []RouteRule{{
			Backends: []BackendRef{{URL: newSwitchingBackend(t), Weight: 1, WebSocket: true}},
		}},
	}))

	clientEnd, clientPeer := net.Pipe()
	t.Cleanup(func() { _ = clientPeer.Close() })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	done := make(chan struct{})

	go func() {
		defer close(done)
		NewHandler(router, WithMetrics(metrics)).ServeHTTP(newHijackToConn(panicOnRead{Conn: clientEnd}), req)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the session did not end after its copy goroutine panicked")
	}

	assert.InDelta(t, 1, testutil.ToFloat64(metrics.handlerPanics.WithLabelValues(panicSiteWebSocketCopy)), 0)
}

// TestRouter_MirrorFiltersInheritHandlerSettings pins the plumbing: a mirror
// filter compiled by UpdateConfig carries the Handler's dispatch limit and
// metrics, at rule and at backend level alike.
func TestRouter_MirrorFiltersInheritHandlerSettings(t *testing.T) {
	t.Parallel()

	metrics := NewMetrics(prometheus.NewRegistry())
	router := NewRouter()
	router.SetHandler(NewHandler(router, WithMetrics(metrics), WithMirrorMaxInFlight(7)))

	mirror := RouteFilter{Type: FilterRequestMirror, RequestMirror: &MirrorConfig{BackendURL: "http://mirror:80"}}

	require.NoError(t, router.UpdateConfig(&Config{
		Version: 1,
		Rules: []RouteRule{{
			Filters:  []RouteFilter{mirror},
			Backends: []BackendRef{{URL: "http://svc:80", Weight: 1, Filters: []RouteFilter{mirror}}},
		}},
	}))

	rules := router.table.Load().defaultRules
	require.Len(t, rules, 1)

	for _, filter := range []Filter{rules[0].filters[0], rules[0].backendFilters[0][0]} {
		compiled, ok := filter.(*requestMirror)
		require.True(t, ok)
		assert.Equal(t, int64(7), compiled.limit)
		assert.Same(t, metrics, compiled.metrics)
	}
}

// TestRouter_MirrorFiltersWithoutHandlerKeepDefaults pins the other side of
// the plumbing: a Router never given a Handler compiles mirror filters with
// the built-in limit and no metrics.
func TestRouter_MirrorFiltersWithoutHandlerKeepDefaults(t *testing.T) {
	t.Parallel()

	router := NewRouter()

	require.NoError(t, router.UpdateConfig(&Config{
		Version: 1,
		Rules: []RouteRule{{
			Filters: []RouteFilter{{
				Type:          FilterRequestMirror,
				RequestMirror: &MirrorConfig{BackendURL: "http://mirror:80"},
			}},
			Backends: []BackendRef{{URL: "http://svc:80", Weight: 1}},
		}},
	}))

	rules := router.table.Load().defaultRules
	require.Len(t, rules, 1)

	compiled, ok := rules[0].filters[0].(*requestMirror)
	require.True(t, ok)
	assert.Equal(t, int64(mirrorMaxLiveDispatches), compiled.maxLive())
	assert.Nil(t, compiled.metrics)
}

// TestWithMirrorMaxInFlight_IgnoresNonPositive pins that zero or a negative
// value keeps the built-in cap rather than disabling mirroring or the cap.
func TestWithMirrorMaxInFlight_IgnoresNonPositive(t *testing.T) {
	t.Parallel()

	for _, value := range []int{0, -3} {
		assert.Zero(t, NewHandler(NewRouter(), WithMirrorMaxInFlight(value)).mirrorMaxInFlight, value)
	}

	assert.Equal(t, int64(5), NewHandler(NewRouter(), WithMirrorMaxInFlight(5)).mirrorMaxInFlight)
}
