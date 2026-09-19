package proxy_test

import (
	"bufio"
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

// newRawUpgradeEchoBackend serves one WebSocket upgrade and then echoes
// raw bytes with no frame parsing, so a test can drive the post-101
// stream directly. The 101 is written onto the hijacked conn rather
// than through WriteHeader because that is the shape
// proxyWebSocketUpgrade reads back with http.ReadResponse.
func newRawUpgradeEchoBackend(t *testing.T) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n")

		if buf.Flush() != nil {
			return
		}

		_, _ = io.Copy(conn, conn)
	}))

	t.Cleanup(server.Close)

	return server
}

// newOneWayUpgradeBackend serves one WebSocket upgrade and then talks in
// a single direction: it drains whatever the client sends and never
// replies, or it pushes a byte every interval and never reads. Either
// way only one of the two copies carries bytes, which is what separates
// a bound that counts both directions from one that counts one.
func newOneWayUpgradeBackend(t *testing.T, push time.Duration) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n")

		if buf.Flush() != nil {
			return
		}

		if push <= 0 {
			_, _ = io.Copy(io.Discard, conn)

			return
		}

		// Writing stops on the first error, which is what the proxy
		// closing the conn produces once the session ends.
		for {
			_, writeErr := conn.Write([]byte("tick"))
			if writeErr != nil {
				return
			}

			time.Sleep(push)
		}
	}))

	t.Cleanup(server.Close)

	return server
}

// newIdleWSHandler wires a route whose only backend is an upgrade-capable
// echo server, with the supplied idle bound on the handler.
func newIdleWSHandler(t *testing.T, backendURL string, idle time.Duration) *proxy.Handler {
	t.Helper()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
			Backends: []proxy.BackendRef{
				{URL: backendURL, Weight: 1, Protocol: proxy.BackendProtocolHTTP, WebSocket: true},
			},
		}},
	}))

	return proxy.NewHandler(router, proxy.WithWSIdleTimeout(idle))
}

func newWSUpgradeRequest(t *testing.T) *http.Request {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", rfc6455SampleWSKey)

	return req
}

// TestHandler_WebSocket_TunnelMode_SilentSessionIsTornDown runs the
// upgrade over the cloudflared HTTP/2 writer, which is where the bound
// has to hold: the conn that writer hands the hijack is a
// localProxyConnection whose SetReadDeadline and Close are both no-ops,
// so a bound actuated on the client side would satisfy an HTTP/1.1
// test and leave the real session running forever.
func TestHandler_WebSocket_TunnelMode_SilentSessionIsTornDown(t *testing.T) {
	t.Parallel()

	backend := newRawUpgradeEchoBackend(t)
	handler := newIdleWSHandler(t, backend.URL, 200*time.Millisecond)

	fake := newFakeCloudflaredRespWriter()
	t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

	done := make(chan struct{})

	go func() {
		defer close(done)
		handler.ServeHTTP(fake, newWSUpgradeRequest(t))
	}()

	require.Eventually(t, fake.Hijacked, 5*time.Second, 25*time.Millisecond,
		"the upgrade must reach the post-101 hijack before the idle bound can be observed")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a session with no bytes in either direction must be torn down after the idle bound; " +
			"it is still open 25 idle windows later, so nothing is bounding it")
	}
}

// TestHandler_WebSocket_TunnelMode_TrafficKeepsSessionOpen is the other
// half of the bound: it must reclaim abandoned sessions without
// reaching one that is still carrying bytes. A lifetime cap fails here.
//
// It does NOT distinguish a bound that counts both directions from one
// that counts either — the echo backend makes the two directions carry
// bytes together. The two one-way tests below are what separate them.
func TestHandler_WebSocket_TunnelMode_TrafficKeepsSessionOpen(t *testing.T) {
	t.Parallel()

	backend := newRawUpgradeEchoBackend(t)
	handler := newIdleWSHandler(t, backend.URL, 300*time.Millisecond)

	fake := newFakeCloudflaredRespWriter()
	t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

	done := make(chan struct{})

	go func() {
		defer close(done)
		handler.ServeHTTP(fake, newWSUpgradeRequest(t))
	}()

	require.Eventually(t, fake.Hijacked, 5*time.Second, 25*time.Millisecond,
		"the upgrade must reach the post-101 hijack before traffic can be driven through it")

	client := fake.HijackedClient()
	echoed := make(chan int64, 1)

	// The pipe behind the fake is unbuffered, so the echo has to be
	// drained or the handler's write back to the client blocks and the
	// session stalls for a reason that has nothing to do with the bound.
	go func() {
		count, _ := io.Copy(io.Discard, bufio.NewReader(client))
		echoed <- count
	}()

	driveClientFor(t, client, 900*time.Millisecond)

	select {
	case <-done:
		t.Fatal("a session carrying bytes throughout must outlive the idle bound; " +
			"tearing it down here means the bound measures session age, not silence")
	default:
	}

	_ = client.Close()
	<-done

	assert.Positive(t, <-echoed,
		"the backend's echo must have reached the client for the traffic to count as activity")
}

// driveClientFor writes to the hijacked client end for the given span.
// Each write carries its own deadline so that a torn-down session — whose
// backend conn is gone and whose pipe therefore has no reader — fails the
// test by name instead of blocking until the package timeout takes every
// other test's result down with it. The hijacked conn's Close is a no-op
// by design (it models cloudflared's), so nothing else unblocks the write.
func driveClientFor(t *testing.T, client net.Conn, span time.Duration) {
	t.Helper()

	until := time.Now().Add(span)
	for time.Now().Before(until) {
		require.NoError(t, client.SetWriteDeadline(time.Now().Add(500*time.Millisecond)))

		_, err := client.Write([]byte("ping"))
		require.NoError(t, err, "the session must still be accepting client bytes; "+
			"a write timeout here means it was torn down while it was carrying traffic")

		time.Sleep(75 * time.Millisecond)
	}

	require.NoError(t, client.SetWriteDeadline(time.Time{}))
}

// TestHandler_WebSocket_TunnelMode_ClientOnlyTrafficKeepsSessionOpen pins
// that bytes flowing from the client count as activity on their own. The
// backend here never replies, so the backend-to-client copy reads nothing
// for the whole test: a guard that instrumented only that direction would
// let the deadline expire under continuous client traffic and cut off a
// one-way upload — a log shipper, a telemetry stream — mid-flight.
func TestHandler_WebSocket_TunnelMode_ClientOnlyTrafficKeepsSessionOpen(t *testing.T) {
	t.Parallel()

	backend := newOneWayUpgradeBackend(t, 0)
	handler := newIdleWSHandler(t, backend.URL, 300*time.Millisecond)

	fake := newFakeCloudflaredRespWriter()
	t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

	done := make(chan struct{})

	go func() {
		defer close(done)
		handler.ServeHTTP(fake, newWSUpgradeRequest(t))
	}()

	require.Eventually(t, fake.Hijacked, 5*time.Second, 25*time.Millisecond,
		"the upgrade must reach the post-101 hijack before traffic can be driven through it")

	driveClientFor(t, fake.HijackedClient(), 900*time.Millisecond)

	select {
	case <-done:
		t.Fatal("a session the client is still sending on must outlive the idle bound")
	default:
	}

	_ = fake.HijackedClient().Close()
	<-done
}

// TestHandler_WebSocket_TunnelMode_BackendOnlyTrafficKeepsSessionOpen is
// the mirror: bytes flowing from the backend count on their own. The
// client here sends nothing after the handshake, which is the shape of
// every server-push socket — a live feed, a progress stream, a tail.
func TestHandler_WebSocket_TunnelMode_BackendOnlyTrafficKeepsSessionOpen(t *testing.T) {
	t.Parallel()

	backend := newOneWayUpgradeBackend(t, 75*time.Millisecond)
	handler := newIdleWSHandler(t, backend.URL, 300*time.Millisecond)

	fake := newFakeCloudflaredRespWriter()
	t.Cleanup(func() { _ = fake.serverSide.Close(); _ = fake.clientSide.Close() })

	done := make(chan struct{})

	go func() {
		defer close(done)
		handler.ServeHTTP(fake, newWSUpgradeRequest(t))
	}()

	require.Eventually(t, fake.Hijacked, 5*time.Second, 25*time.Millisecond,
		"the upgrade must reach the post-101 hijack before the backend's bytes can flow")

	// The pipe is unbuffered, so the backend's bytes only move while
	// something is reading the client end.
	received := make(chan int64, 1)

	go func() {
		count, _ := io.Copy(io.Discard, bufio.NewReader(fake.HijackedClient()))
		received <- count
	}()

	time.Sleep(900 * time.Millisecond)

	select {
	case <-done:
		t.Fatal("a session the backend is still pushing on must outlive the idle bound; " +
			"tearing it down here means client silence alone expires the window")
	default:
	}

	_ = fake.HijackedClient().Close()
	<-done

	assert.Positive(t, <-received,
		"the backend's bytes must have reached the client for the traffic to count as activity")
}
