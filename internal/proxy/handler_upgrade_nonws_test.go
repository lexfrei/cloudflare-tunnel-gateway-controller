package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http/httpguts"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

const plainUpgradeBackendBody = "plain response"

// upgradeWillingBackend switches protocols whenever a request asks it to
// and answers plain HTTP otherwise. It records the Upgrade and Connection
// headers of every request so a test can see what the proxy forwarded.
type upgradeWillingBackend struct {
	*httptest.Server

	mu         sync.Mutex
	upgrades   []string
	connection []string
}

// newUpgradingBackend returns an upgradeWillingBackend; with always set it
// answers 101 to every request, asked or not.
func newUpgradingBackend(t *testing.T, always bool) *upgradeWillingBackend {
	t.Helper()

	backend := &upgradeWillingBackend{}
	backend.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		backend.mu.Lock()
		backend.upgrades = append(backend.upgrades, req.Header.Get("Upgrade"))
		backend.connection = append(backend.connection, req.Header.Get("Connection"))
		backend.mu.Unlock()

		if !always && req.Header.Get("Upgrade") == "" {
			_, _ = io.WriteString(writer, plainUpgradeBackendBody)

			return
		}

		conn, buf, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
		_, _ = io.Copy(io.Discard, conn)
	}))

	t.Cleanup(backend.Close)

	return backend
}

// seen returns the Upgrade and Connection values of every request, in order.
func (b *upgradeWillingBackend) seen() ([]string, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.upgrades...), append([]string(nil), b.connection...)
}

func newNonWSUpgradeHandler(t *testing.T, backendURL string) *proxy.Handler {
	t.Helper()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{URL: backendURL, Weight: 1, Protocol: proxy.BackendProtocolHTTP},
				},
			},
		},
	}))

	return proxy.NewHandler(router)
}

func setWebSocketUpgradeHeaders(req *http.Request) {
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", rfc6455SampleWSKey)
}

// serveBounded runs the handler against the fake writer. A hijacked stream
// would keep ServeHTTP copying until the pipe closes, so the pipe is
// closed after five seconds and the test fails on its assertions instead
// of hanging.
func serveBounded(handler http.Handler, fake *fakeCloudflaredRespWriter, req *http.Request) {
	done := make(chan struct{})

	go func() {
		defer close(done)
		handler.ServeHTTP(fake, req)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = fake.HijackedClient().Close()
		<-done
	}
}

func assertBackendSawNoUpgrade(t *testing.T, backend *upgradeWillingBackend) {
	t.Helper()

	upgrades, connection := backend.seen()
	require.Len(t, upgrades, 1, "the backend must receive exactly one request")
	assert.Empty(t, upgrades[0], "the Upgrade header must not reach a backend without WebSocket enabled")
	assert.False(t, httpguts.HeaderValuesContainsToken(connection[:1], "upgrade"),
		"the upgrade token of Connection must not reach a backend without WebSocket enabled")
}

// TestHandler_NonWSBackend_UpgradeForwardedAsPlainHTTP drives an upgrade
// request through a real HTTP/1.1 server to a backend that has no
// WebSocket opt-in but would switch protocols if asked. The request is
// forwarded as plain HTTP, so the client gets the backend's ordinary
// response instead of a 101.
func TestHandler_NonWSBackend_UpgradeForwardedAsPlainHTTP(t *testing.T) {
	t.Parallel()

	backend := newUpgradingBackend(t, false)

	proxySrv := httptest.NewServer(newNonWSUpgradeHandler(t, backend.URL))
	t.Cleanup(proxySrv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, proxySrv.URL+"/ws", nil)
	require.NoError(t, err)
	setWebSocketUpgradeHeaders(req)

	// A client of its own: httptest.Server.Close in a parallel test closes
	// idle connections on http.DefaultTransport.
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)

	resp, err := client.Do(req)
	require.NoError(t, err)

	t.Cleanup(func() { _ = resp.Body.Close() })

	// Checked before reading: a 101 body is the upgraded stream and
	// never reaches EOF.
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, plainUpgradeBackendBody, string(body))
	assertBackendSawNoUpgrade(t, backend)
}

// TestHandler_NonWSBackend_UpgradeForwardedAsPlainHTTP_TunnelMode runs the
// same request through both cloudflared response-writer contracts. The
// HTTP/2 writer refuses a Hijack before a status is written; the QUIC
// writer hands one out unconditionally. With the upgrade forwarded as
// plain HTTP neither writer is hijacked and both record the backend's
// ordinary response.
func TestHandler_NonWSBackend_UpgradeForwardedAsPlainHTTP_TunnelMode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		writer func() *fakeCloudflaredRespWriter
	}{
		{name: "http2", writer: newFakeCloudflaredRespWriter},
		{name: "quic", writer: newFakeCloudflaredQUICRespWriter},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			backend := newUpgradingBackend(t, false)
			handler := newNonWSUpgradeHandler(t, backend.URL)

			fake := tc.writer()
			t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/ws", nil)
			setWebSocketUpgradeHeaders(req)

			serveBounded(handler, fake, req)

			assert.False(t, fake.Hijacked(), "a backend without WebSocket enabled must never get a hijacked stream")
			assert.Equal(t, http.StatusOK, fake.Status())
			assert.Equal(t, plainUpgradeBackendBody, string(fake.Body()))
			assertBackendSawNoUpgrade(t, backend)
		})
	}
}

// TestHandler_NonWSBackend_UnrequestedSwitch_TunnelMode covers a backend
// that answers 101 to the plain request it receives. ReverseProxy rejects
// a protocol switch the outbound request did not ask for, so even the
// QUIC writer, which would hand out a stream without a status, is never
// hijacked and the client gets a 502.
func TestHandler_NonWSBackend_UnrequestedSwitch_TunnelMode(t *testing.T) {
	t.Parallel()

	backend := newUpgradingBackend(t, true)
	handler := newNonWSUpgradeHandler(t, backend.URL)

	fake := newFakeCloudflaredQUICRespWriter()
	t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/ws", nil)
	setWebSocketUpgradeHeaders(req)

	serveBounded(handler, fake, req)

	assert.False(t, fake.Hijacked(), "a backend without WebSocket enabled must never get a hijacked stream")
	assert.Equal(t, http.StatusBadGateway, fake.Status())
	assertBackendSawNoUpgrade(t, backend)
}
