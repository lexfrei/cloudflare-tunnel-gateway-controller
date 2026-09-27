package proxy_test

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// newStallingRefusalBackend accepts one connection, reads the upgrade request,
// then sends a refusal whose body stops after sent, short of its
// Content-Length, and never closes the connection.
func newStallingRefusalBackend(t *testing.T, sent string) string {
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
		_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 100\r\n\r\n" + sent))

		<-done
	}()

	return "http://" + listener.Addr().String()
}

// newTricklingRefusalBackend sends a refusal whose body arrives one byte at a
// time, each well inside the bound but all together well past it.
func newTricklingRefusalBackend(t *testing.T, body string, gap time.Duration) string {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		_, _ = http.ReadRequest(bufio.NewReader(conn))
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: %d\r\n\r\n", len(body))

		for idx := range len(body) {
			time.Sleep(gap)

			_, _ = conn.Write([]byte{body[idx]})
		}
	}()

	return "http://" + listener.Addr().String()
}

// TestHandler_WebSocketRefusal_TunnelMode_SteadyBodyIsNotCut pins that the
// bound on a refusal body is a stall bound, not a total one: a body that keeps
// arriving, however slowly, is forwarded whole.
func TestHandler_WebSocketRefusal_TunnelMode_SteadyBodyIsNotCut(t *testing.T) {
	t.Parallel()

	const body = "denied!!"

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
			Backends: []proxy.BackendRef{{
				URL: newTricklingRefusalBackend(t, body, 60*time.Millisecond), Weight: 1,
				Protocol: proxy.BackendProtocolHTTP, WebSocket: true,
			}},
		}},
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

	// 8 bytes 60ms apart take about 480ms, past a 200ms bound on the total.
	proxy.NewHandler(router, proxy.WithWSHandshakeReadTimeout(200*time.Millisecond)).ServeHTTP(fake, req)

	assert.Equal(t, http.StatusForbidden, fake.Status())
	assert.Equal(t, body, string(fake.Body()), "a body that keeps arriving must not be cut off")
}

// TestHandler_WebSocketRefusal_TunnelMode_BodyCopyIsBounded pins that a backend
// refusing the upgrade and then stalling part-way into the refusal body cannot
// hold the handler, the backend connection and the tunnel stream open
// indefinitely. The handshake deadline is cleared for the 101 branch only; the
// refusal body is copied under a bound of its own.
func TestHandler_WebSocketRefusal_TunnelMode_BodyCopyIsBounded(t *testing.T) {
	t.Parallel()

	for name, newWriter := range map[string]func() *fakeCloudflaredRespWriter{
		"http2": newFakeCloudflaredRespWriter,
		"quic":  newFakeCloudflaredQUICRespWriter,
	} {
		// A stall before the first body byte and one part-way through.
		for _, sent := range []string{"", "partial"} {
			t.Run(name+"/sent="+sent, func(t *testing.T) {
				t.Parallel()
				runStallingRefusal(t, newWriter(), sent)
			})
		}
	}
}

func runStallingRefusal(t *testing.T, fake *fakeCloudflaredRespWriter, sent string) {
	t.Helper()

	backendURL := newStallingRefusalBackend(t, sent)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{URL: backendURL, Weight: 1, Protocol: proxy.BackendProtocolHTTP, WebSocket: true},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router, proxy.WithWSHandshakeReadTimeout(200*time.Millisecond))

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

	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the refusal body copy has no bound: the handler is still blocked on a stalled backend")
	}

	assert.Equal(t, http.StatusForbidden, fake.Status(), "the refusal status is forwarded")
	assert.Equal(t, sent, string(fake.Body()), "the bytes the backend did send are forwarded")
	assert.False(t, fake.Hijacked(), "a refusal is a plain response, never a hijack")
}
