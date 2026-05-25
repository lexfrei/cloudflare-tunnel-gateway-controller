package proxy_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

const rewrittenHost = "rewritten.example.com"

func TestHandler_NoMatchReturns404(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://unknown.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusNotFound, recorder.Code)
}

func TestHandler_ProxiesToBackend(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "reached")
		writer.WriteHeader(http.StatusOK)

		_, err := writer.Write([]byte("hello from backend"))
		if err != nil {
			t.Errorf("failed to write response: %v", err)
		}
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/test", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "reached", recorder.Header().Get("X-Backend"))

	body, err := io.ReadAll(recorder.Body)
	require.NoError(t, err)
	assert.Equal(t, "hello from backend", string(body))
}

func TestHandler_RequestHeaderFilter(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		// Echo back the header we expect to be set by the filter.
		writer.Header().Set("X-Received", req.Header.Get("X-Injected"))
		writer.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterRequestHeaderModifier,
						RequestHeaderModifier: &proxy.HeaderModifier{
							Set: []proxy.HeaderValue{{Name: "X-Injected", Value: "filter-value"}},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "filter-value", recorder.Header().Get("X-Received"))
}

func TestHandler_ResponseHeaderFilter(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Internal", "secret")
		writer.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterResponseHeaderModifier,
						ResponseHeaderModifier: &proxy.HeaderModifier{
							Remove: []string{"X-Internal"},
							Set:    []proxy.HeaderValue{{Name: "X-Added", Value: "by-filter"}},
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Empty(t, recorder.Header().Get("X-Internal"))
	assert.Equal(t, "by-filter", recorder.Header().Get("X-Added"))
}

func TestHandler_RedirectFilter(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	scheme := testSchemeHTTPS
	statusCode := http.StatusMovedPermanently

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterRequestRedirect,
						RequestRedirect: &proxy.RedirectConfig{
							Scheme:     &scheme,
							StatusCode: &statusCode,
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: "http://unused:80", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/page", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusMovedPermanently, recorder.Code)
	assert.Equal(t, "https://app.example.com/page", recorder.Header().Get("Location"))
}

func TestHandler_PathMatchRouting(t *testing.T) {
	t.Parallel()

	apiBackend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "api")
		writer.WriteHeader(http.StatusOK)
	}))
	defer apiBackend.Close()

	webBackend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "web")
		writer.WriteHeader(http.StatusOK)
	}))
	defer webBackend.Close()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/api"}},
				},
				Backends: []proxy.BackendRef{{URL: apiBackend.URL, Weight: 1}},
			},
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}},
				},
				Backends: []proxy.BackendRef{{URL: webBackend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	// API path
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/api/users", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	assert.Equal(t, "api", recorder.Header().Get("X-Backend"))

	// Web path
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/index.html", nil)
	recorder = httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	assert.Equal(t, "web", recorder.Header().Get("X-Backend"))
}

func TestHandler_BackendError(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: "http://127.0.0.1:1", Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusBadGateway, recorder.Code)
}

func TestHandler_ClientDisconnectDoesNotReturn504(t *testing.T) {
	t.Parallel()

	// Backend that blocks until context is cancelled.
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	// Create a request with a cancelable context to simulate client disconnect.
	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	done := make(chan struct{})

	go func() {
		handler.ServeHTTP(recorder, req)
		close(done)
	}()

	// Cancel context to simulate client disconnect.
	cancel()
	<-done

	// Should NOT be 504 — client is gone, response doesn't matter much,
	// but it should not be a misleading 504 Gateway Timeout.
	assert.NotEqual(t, http.StatusGatewayTimeout, recorder.Code,
		"client disconnect should not produce 504 Gateway Timeout")
}

func TestHandler_PruneTransportsRemovesStaleHosts(t *testing.T) {
	t.Parallel()

	backendA := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "A")
		writer.WriteHeader(http.StatusOK)
	}))
	defer backendA.Close()

	backendB := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "B")
		writer.WriteHeader(http.StatusOK)
	}))
	defer backendB.Close()

	router := proxy.NewRouter()
	handler := proxy.NewHandler(router)
	router.SetHandler(handler)

	// Configure with backend A and send a request to populate the transport cache.
	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backendA.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	assert.Equal(t, "A", recorder.Header().Get("X-Backend"))

	// Update config to use backend B only. This triggers PruneTransports via SetHandler,
	// which should remove the transport for backend A's host.
	cfg2 := &proxy.Config{
		Version: 2,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backendB.URL, Weight: 1}},
			},
		},
	}

	err = router.UpdateConfig(cfg2)
	require.NoError(t, err)

	// Verify backend B is reachable after the config update.
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder = httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	assert.Equal(t, "B", recorder.Header().Get("X-Backend"))

	// Directly test PruneTransports: prune with no active hosts should not panic.
	handler.PruneTransports(map[string]bool{})
}

func TestHandler_PruneTransportsRemovesStaleHostFromSyncMap(t *testing.T) {
	t.Parallel()

	backendA := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer backendA.Close()

	backendB := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer backendB.Close()

	router := proxy.NewRouter()
	handler := proxy.NewHandler(router)
	router.SetHandler(handler)

	// Configure with backend A and send a request to populate the transport cache.
	cfgA := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backendA.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfgA)
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code)

	// Verify transport for backend A was created.
	hostA := backendA.Listener.Addr().String()
	keyA := proxy.TransportKey(hostA, proxy.BackendProtocolHTTP, nil)
	_, loaded := handler.Transports().Load(keyA)
	require.True(t, loaded, "transport for backend A should exist after request")

	// Push new config with only backend B, removing A.
	cfgB := &proxy.Config{
		Version: 2,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backendB.URL, Weight: 1}},
			},
		},
	}

	err = router.UpdateConfig(cfgB)
	require.NoError(t, err)

	// Verify transport for backend A was pruned.
	_, loaded = handler.Transports().Load(keyA)
	assert.False(t, loaded, "transport for backend A should be pruned after config update removed it")
}

func TestHandler_URLRewriteHostnamePreservedByDirector(t *testing.T) {
	t.Parallel()

	receivedHost := make(chan string, 1)

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		receivedHost <- req.Host
		writer.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	rewrittenHostname := rewrittenHost

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterURLRewrite,
						URLRewrite: &proxy.URLRewriteConfig{
							Hostname: &rewrittenHostname,
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/test", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)

	host := <-receivedHost
	assert.Equal(t, "rewritten.example.com", host,
		"Director should preserve the rewritten hostname, not overwrite it with the backend host")
}

func TestHandler_AllZeroWeightBackendsReturns500(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Matches: []proxy.RouteMatch{
					{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}},
				},
				Backends: []proxy.BackendRef{
					{URL: "http://backend-a:80", Weight: 0},
					{URL: "http://backend-b:80", Weight: 0},
				},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/test", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "no backend available")
}

func TestHandler_PruneTransportsPreservesActiveHosts(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Backend", "active")
		writer.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	router := proxy.NewRouter()
	handler := proxy.NewHandler(router)

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	// Send a request to populate the transport cache.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusOK, recorder.Code)

	// Prune with the backend's host still active. The transport should be preserved
	// and the next request should succeed without creating a new transport.
	activeHosts := map[string]bool{
		backend.Listener.Addr().String(): true,
	}
	handler.PruneTransports(activeHosts)

	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder = httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "active", recorder.Header().Get("X-Backend"))
}

// errBadGatewayShape is the sentinel used by TestErrorHandler_504Surface
// to represent "any error that does NOT satisfy the timeout interface".
// Extracted to satisfy err113 (no dynamic errors at call sites).
var errBadGatewayShape = errors.New("bad gateway shape")

// TestErrorHandler_504Surface pins the contract that errorHandler
// returns 504 for any error that satisfies the net.Error timeout
// interface, not only for context.DeadlineExceeded. The widening
// happened deliberately when per-rule timeouts moved to
// *http.Transport.ResponseHeaderTimeout (which surfaces an internal
// *timeoutError satisfying Timeout() bool but not wrapping
// context.DeadlineExceeded). The same widening also catches dial
// timeouts (*net.OpError with timeout=true) and DNS timeouts
// (*net.DNSError with IsTimeout=true); both are now mapped to 504
// where they previously fell through to 502.
//
// The shift to 504 for dial/DNS timeouts is intentional and
// arguably more correct -- the gateway failed to talk to the
// upstream within the configured budget, which is a timeout, not a
// bad-gateway. Pinning the shape here so a future narrowing (e.g.
// reverting to "only ResponseHeaderTimeout = 504") fails this test
// and is caught by review.
func TestErrorHandler_504Surface(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{
			name:       "context.DeadlineExceeded",
			err:        context.DeadlineExceeded,
			wantStatus: http.StatusGatewayTimeout,
		},
		{
			name:       "wrapped context.DeadlineExceeded",
			err:        fmt.Errorf("wrap: %w", context.DeadlineExceeded),
			wantStatus: http.StatusGatewayTimeout,
		},
		{
			name:       "ResponseHeaderTimeout (Timeout() interface, not wrapped DeadlineExceeded)",
			err:        &timeoutSentinelError{},
			wantStatus: http.StatusGatewayTimeout,
		},
		{
			name:       "net.Error{Timeout=true} dial timeout shape",
			err:        &net.OpError{Op: "dial", Err: &timeoutSentinelError{}},
			wantStatus: http.StatusGatewayTimeout,
		},
		{
			name:       "net.DNSError with IsTimeout=true",
			err:        &net.DNSError{Err: "lookup timed out", IsTimeout: true},
			wantStatus: http.StatusGatewayTimeout,
		},
		{
			name:       "plain non-timeout error falls through to 502",
			err:        errBadGatewayShape,
			wantStatus: http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)

			proxy.ErrorHandlerForTest(recorder, req, tt.err)

			assert.Equal(t, tt.wantStatus, recorder.Code,
				"errorHandler status for %T must be %d", tt.err, tt.wantStatus)
		})
	}
}

// timeoutSentinelError is a minimal error implementation that satisfies
// the interface{ Timeout() bool } contract errorHandler keys on.
// Mirrors the shape of http.Transport's internal *timeoutError that
// surfaces from ResponseHeaderTimeout firings.
type timeoutSentinelError struct{}

func (*timeoutSentinelError) Error() string { return "timeout sentinel" }

func (*timeoutSentinelError) Timeout() bool { return true }

// TestTransportKey_DistinguishesByHeaderTimeout pins the cache-key
// invariant that two routes against the same backend with different
// per-rule timeouts produce distinct transportKey strings.
// ResponseHeaderTimeout is a *http.Transport field and stdlib has no
// per-call override; if the timeout dimension drops out of the key,
// route B silently inherits route A's deadline. Symmetric with
// TestTransportKey_DistinguishesByClientCert.
func TestTransportKey_DistinguishesByHeaderTimeout(t *testing.T) {
	t.Parallel()

	const (
		host     = "backend.example.com:8080"
		protocol = proxy.BackendProtocolHTTP
	)

	keyNoTimeout := proxy.TransportKeyWithTimeout(host, protocol, nil, 0)
	keyOneSec := proxy.TransportKeyWithTimeout(host, protocol, nil, time.Second)
	keyTenSec := proxy.TransportKeyWithTimeout(host, protocol, nil, 10*time.Second)

	assert.NotEqual(t, keyNoTimeout, keyOneSec,
		"adding a header timeout must change the key -- otherwise an unbounded route inherits the cached deadline")
	assert.NotEqual(t, keyOneSec, keyTenSec,
		"different header timeouts must produce different keys -- otherwise route B inherits route A's deadline")
	assert.NotEqual(t, keyNoTimeout, keyTenSec)

	// Determinism.
	assert.Equal(t, keyOneSec, proxy.TransportKeyWithTimeout(host, protocol, nil, time.Second))
}

// TestExtractActiveTransportKeys_IncludesHeaderTimeout pins the
// router-side derivation of the cache key so PruneTransports' eviction
// path covers the timeout dimension end-to-end. Without this, a config
// update that flips a route's timeouts.request from 1s to 10s would
// add the new 10s-keyed transport but leave the 1s entry pinned in the
// cache forever.
func TestExtractActiveTransportKeys_IncludesHeaderTimeout(t *testing.T) {
	t.Parallel()

	const (
		host       = "backend.example.com:8080"
		shortTimer = time.Second
		longTimer  = 5 * time.Second
	)

	backendURL := "http://" + host

	configWithTimeout := func(timeout time.Duration) *proxy.Config {
		return &proxy.Config{
			Version: 1,
			Rules: []proxy.RouteRule{
				{
					Hostnames: []string{"app.example.com"},
					Timeouts:  &proxy.RouteTimeouts{Request: timeout},
					Backends:  []proxy.BackendRef{{URL: backendURL, Weight: 1}},
				},
			},
		}
	}

	keysBefore := proxy.ExtractActiveTransportKeysForTest(configWithTimeout(shortTimer))
	keysAfter := proxy.ExtractActiveTransportKeysForTest(configWithTimeout(longTimer))

	require.Len(t, keysBefore, 1, "single rule must yield exactly one key")
	require.Len(t, keysAfter, 1, "single rule must yield exactly one key")

	for k := range keysBefore {
		assert.NotContains(t, keysAfter, k,
			"flipping the rule's timeouts.request must produce a different key -- otherwise PruneTransports never evicts the stale transport")
	}
}

// TestHandler_RequestTimeoutFiresWhenBackendStallsBeforeHeaders pins the
// pre-headers half of the per-rule Request timeout contract: when the
// backend takes longer than timeouts.request to send response headers,
// the proxy emits 504 promptly. The fix that moved per-rule timeouts to
// ResponseHeaderTimeout intentionally preserved this behavior -- the
// transport sentinel surfaces through errorHandler's Timeout()
// interface check and still produces a 504. Was previously named
// CoversEntireHandler, which became misleading once the deadline
// stopped covering the body phase.
func TestHandler_RequestTimeoutFiresWhenBackendStallsBeforeHeaders(t *testing.T) {
	t.Parallel()

	// Backend that sleeps longer than the request timeout.
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		select {
		case <-time.After(5 * time.Second):
			writer.WriteHeader(http.StatusOK)
		case <-req.Context().Done():
			// Context cancelled by timeout.
		}
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	requestTimeout := 100 * time.Millisecond

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Timeouts:  &proxy.RouteTimeouts{Request: requestTimeout},
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	start := time.Now()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	elapsed := time.Since(start)

	// The request should be cancelled well before the backend's 5s sleep.
	assert.Less(t, elapsed, 2*time.Second, "request timeout should cancel the handler promptly")
	assert.Equal(t, http.StatusGatewayTimeout, recorder.Code)
}

// TestHandler_BackendTimeoutFiresWhenBackendStallsBeforeHeaders is the
// sibling pin for timeouts.backend; same shape, same 504 contract,
// independently asserted so that a future refactor that drops one
// arm of the two knobs gets caught even if the other arm still works.
// Was previously named OnlyAffectsProxyCall, which became misleading
// once both knobs collapsed onto the same transport-level
// ResponseHeaderTimeout via ruleHeaderTimeout's min().
func TestHandler_BackendTimeoutFiresWhenBackendStallsBeforeHeaders(t *testing.T) {
	t.Parallel()

	// Backend that sleeps longer than the backend timeout.
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		select {
		case <-time.After(5 * time.Second):
			writer.WriteHeader(http.StatusOK)
		case <-req.Context().Done():
			// Context cancelled by timeout.
		}
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	backendTimeout := 100 * time.Millisecond

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Timeouts:  &proxy.RouteTimeouts{Backend: backendTimeout},
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	start := time.Now()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	elapsed := time.Since(start)

	// Backend timeout should cancel the proxy call promptly.
	assert.Less(t, elapsed, 2*time.Second, "backend timeout should cancel the proxy call promptly")
	assert.Equal(t, http.StatusGatewayTimeout, recorder.Code)
}

// TestHandler_BothTimeoutsCollapseToStricter pins that when both
// timeouts.request and timeouts.backend are set, the stricter one
// wins. They no longer fire independently -- ruleHeaderTimeout
// collapses them via min() onto a single transport-level
// ResponseHeaderTimeout. The contract this test pins is: with
// Request=2s and Backend=100ms, the 100ms deadline fires first and
// the response lands in well under 2s. Was previously named
// BothTimeoutsAppliedIndependently, which became inaccurate.
func TestHandler_BothTimeoutsCollapseToStricter(t *testing.T) {
	t.Parallel()

	// Backend that sleeps longer than both timeouts.
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		select {
		case <-time.After(5 * time.Second):
			writer.WriteHeader(http.StatusOK)
		case <-req.Context().Done():
			// Context cancelled by timeout.
		}
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	// Request timeout is longer than backend timeout; backend timeout should fire first.
	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Timeouts: &proxy.RouteTimeouts{
					Request: 2 * time.Second,
					Backend: 100 * time.Millisecond,
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	start := time.Now()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)
	elapsed := time.Since(start)

	// Backend timeout (100ms) must fire before the looser Request
	// timeout (2s). Tightened the upper bound to 500ms (5x the
	// stricter deadline) so a regression that flipped the collapse
	// direction (Request winning over Backend) gets caught instead
	// of being absorbed by a loose <1s window. Still leaves
	// enough headroom for httptest scheduling jitter on a loaded
	// CI runner.
	assert.Less(t, elapsed, 500*time.Millisecond,
		"backend timeout (100ms) must collapse to the wire deadline; if elapsed approaches the "+
			"2s request timeout the min() collapse regressed")
	assert.Equal(t, http.StatusGatewayTimeout, recorder.Code)
}

func TestHandler_URLRewriteHostBeatsXOriginalHost(t *testing.T) {
	t.Parallel()

	receivedHost := make(chan string, 1)

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		receivedHost <- req.Host
		writer.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	rewrittenHostname := rewrittenHost

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Filters: []proxy.RouteFilter{
					{
						Type: proxy.FilterURLRewrite,
						URLRewrite: &proxy.URLRewriteConfig{
							Hostname: &rewrittenHostname,
						},
					},
				},
				Backends: []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	// Simulate tunnel transport: X-Original-Host carries the real hostname
	// that was replaced with the edge hostname for Cloudflare routing.
	// When a URL rewrite filter explicitly sets a new host, the filter's
	// host must take precedence over X-Original-Host.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://edge.cloudflare.com/test", nil)
	req.Header.Set("X-Original-Host", "app.example.com")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)

	host := <-receivedHost
	assert.Equal(t, "rewritten.example.com", host,
		"URL rewrite filter host must take precedence over X-Original-Host")
}

func TestHandler_BackendSpecificHeaderModifier(t *testing.T) {
	t.Parallel()

	receivedHeaders := make(chan http.Header, 1)

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		receivedHeaders <- req.Header.Clone()
		writer.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Backends: []proxy.BackendRef{
					{
						URL:    backend.URL,
						Weight: 1,
						Filters: []proxy.RouteFilter{
							{
								Type: proxy.FilterRequestHeaderModifier,
								RequestHeaderModifier: &proxy.HeaderModifier{
									Set: []proxy.HeaderValue{
										{Name: "Backend", Value: "backend-v1"},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusOK, recorder.Code)

	headers := <-receivedHeaders
	assert.Equal(t, "backend-v1", headers.Get("Backend"),
		"backend-specific RequestHeaderModifier should set Backend header")
}

// TestIsHTTPUpgradeRequest pins the RFC 7230 §6.1 contract that drives the
// upgrade-skip in ServeHTTP / proxyToBackend. The integration tests cover
// only the canonical WebSocket shape that golang.org/x/net/websocket emits
// (single-token Connection: Upgrade + Upgrade: websocket); the cases below
// guard the token-parser against regressions on shapes that browsers and
// upstream HTTP/2-to-HTTP/1.1 reverse proxies produce in the wild.
func TestIsHTTPUpgradeRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		headers http.Header
		want    bool
	}{
		{
			name:    "websocket canonical single-token Connection",
			headers: http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}},
			want:    true,
		},
		{
			name:    "comma-separated Connection with upgrade as second token (typical browser shape)",
			headers: http.Header{"Connection": {"keep-alive, Upgrade"}, "Upgrade": {"websocket"}},
			want:    true,
		},
		{
			name:    "case-insensitive Connection token",
			headers: http.Header{"Connection": {"upgrade"}, "Upgrade": {"websocket"}},
			want:    true,
		},
		{
			name:    "extra whitespace around comma-separated tokens",
			headers: http.Header{"Connection": {"keep-alive ,  Upgrade  ,  TE"}, "Upgrade": {"websocket"}},
			want:    true,
		},
		{
			name: "multiple Connection header lines, upgrade in second line",
			headers: http.Header{
				"Connection": {"keep-alive", "Upgrade"},
				"Upgrade":    {"h2c"},
			},
			want: true,
		},
		{
			name:    "Connection: Upgrade but no Upgrade header — not a valid upgrade",
			headers: http.Header{"Connection": {"Upgrade"}},
			want:    false,
		},
		{
			name:    "Upgrade header set but Connection lacks upgrade token",
			headers: http.Header{"Connection": {"keep-alive"}, "Upgrade": {"websocket"}},
			want:    false,
		},
		{
			name:    "neither header present",
			headers: http.Header{},
			want:    false,
		},
		{
			name:    "Upgrade header empty string",
			headers: http.Header{"Connection": {"Upgrade"}, "Upgrade": {""}},
			want:    false,
		},
		{
			name:    "Connection token contains 'upgrade' substring but not as a standalone token",
			headers: http.Header{"Connection": {"upgrade-insecure-requests"}, "Upgrade": {"websocket"}},
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
			req.Header = tc.headers

			assert.Equal(t, tc.want, proxy.IsHTTPUpgradeRequestForTest(req))
		})
	}
}

// TestHandler_StreamingResponseSurvivesRequestTimeout pins the
// contract that timeouts.request bounds time-to-first-byte from the
// backend, NOT the full duration of a streaming response. Without
// this contract, SSE / chunked / large-file / gRPC-server-streaming
// responses get truncated at the timeout boundary regardless of
// whether bytes are still flowing -- the per-rule timeout becomes a
// streaming-response footgun.
//
// Mechanism: a backend that sends Content-Type: text/event-stream,
// flushes headers immediately, then emits four "data:" frames over
// 2s. The route's timeouts.request is 300ms -- well inside the
// stream duration. With the fix, the timeout only bounds the time
// the backend takes to send headers; once headers arrive the body
// streams for as long as the backend takes.
//
// The pre-fix behavior wraps the whole request in
// context.WithTimeout, which cancels the conn 300ms in and
// truncates the response after frame 0 or 1. The new behavior
// transports the timeout via http.Transport.ResponseHeaderTimeout
// so the post-headers stream is unbounded by the route timeout.
func TestHandler_StreamingResponseSurvivesRequestTimeout(t *testing.T) {
	t.Parallel()

	runStreamingTimeoutTest(t, &proxy.RouteTimeouts{Request: 300 * time.Millisecond})
}

// TestHandler_StreamingResponseSurvivesBackendTimeout is the
// sibling pin for timeouts.backend. Both knobs map to the same
// transport-level header timeout, so the same contract must hold.
// Without an explicit test the backend-timeout arm would slip
// through CI if a future refactor reintroduced context.WithTimeout
// on it alone.
func TestHandler_StreamingResponseSurvivesBackendTimeout(t *testing.T) {
	t.Parallel()

	runStreamingTimeoutTest(t, &proxy.RouteTimeouts{Backend: 300 * time.Millisecond})
}

// runStreamingTimeoutTest is the shared scaffold for the two
// streaming-survives-timeout pins. Spins up an SSE-style backend
// that flushes headers immediately, emits four frames over 2s,
// then closes; wires the route with the given per-rule timeouts;
// asserts the full body reaches the client unsharded.
func runStreamingTimeoutTest(t *testing.T, timeouts *proxy.RouteTimeouts) {
	t.Helper()

	const frameCount = 4

	const interFrame = 500 * time.Millisecond

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.WriteHeader(http.StatusOK)

		flusher, ok := writer.(http.Flusher)
		require.True(t, ok, "httptest writer must implement http.Flusher for streaming")

		flusher.Flush()

		for idx := range frameCount {
			fmt.Fprintf(writer, "data: event-%d\n\n", idx)
			flusher.Flush()
			time.Sleep(interFrame)
		}
	}))
	defer backend.Close()

	router := proxy.NewRouter()

	cfg := &proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Hostnames: []string{"app.example.com"},
				Timeouts:  timeouts,
				Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
			},
		},
	}

	err := router.UpdateConfig(cfg)
	require.NoError(t, err)

	// httptest.NewRecorder does not stream -- it buffers the full body
	// and only returns after the handler completes. That is exactly the
	// shape the test needs: if the handler aborts mid-stream because of
	// the timeout, the recorder body will be missing the late frames.
	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/sse", nil)
	recorder := httptest.NewRecorder()

	start := time.Now()

	handler.ServeHTTP(recorder, req)

	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, recorder.Code,
		"streaming response must propagate the backend's 200 -- a 504 here means the timeout fired pre-headers")

	body := recorder.Body.String()
	for idx := range frameCount {
		assert.Contains(t, body, fmt.Sprintf("data: event-%d", idx),
			"frame %d must survive the per-rule timeout; truncated body indicates context.WithTimeout "+
				"is still cancelling the streaming body read", idx)
	}

	// Sanity: the test must take longer than the configured timeout,
	// otherwise it isn't actually exercising the streaming-survives
	// contract.
	assert.Greater(t, elapsed, 1500*time.Millisecond,
		"test must take longer than 1.5s -- if it returned faster, the streaming backend didn't actually run "+
			"to completion and the assertion is not exercising the timeout contract")
}

// TestIsHTTPUpgradeRequest_NilGuards pins the defensive nil-checks in
// isHTTPUpgradeRequest. They exist so a future refactor that calls into the
// helper before req is fully constructed doesn't panic.
func TestIsHTTPUpgradeRequest_NilGuards(t *testing.T) {
	t.Parallel()

	assert.False(t, proxy.IsHTTPUpgradeRequestForTest(nil),
		"nil request must return false, not panic")

	req := &http.Request{Header: nil}
	assert.False(t, proxy.IsHTTPUpgradeRequestForTest(req),
		"request with nil Header must return false, not panic")
}
