package proxy_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// tunnelWriterModes enumerates the cloudflared response-writer contracts the
// production path can hand to the handler.
var tunnelWriterModes = []struct {
	name string
	new  func() *fakeCloudflaredRespWriter
}{
	{name: "http2", new: newFakeCloudflaredRespWriter},
	{name: "quic", new: newFakeCloudflaredQUICRespWriter},
}

// newH2COnlyBackend starts an httptest server that speaks HTTP/1.1 and h2c
// with prior knowledge, so a request arriving as HTTP/1.1 is visible to the
// handler instead of failing the connection.
func newH2COnlyBackend(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()

	var protocols http.Protocols

	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	server := httptest.NewUnstartedServer(handler)
	server.Config.Protocols = &protocols
	server.Start()
	t.Cleanup(server.Close)

	return server
}

func newH2CHandler(t *testing.T, backendURL string, timeouts *proxy.RouteTimeouts) *proxy.Handler {
	t.Helper()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Matches:  []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Timeouts: timeouts,
				Backends: []proxy.BackendRef{{URL: backendURL, Weight: 1, Protocol: proxy.BackendProtocolH2C}},
			},
		},
	}))

	return proxy.NewHandler(router)
}

func TestNewTransport_H2C_IsNativeHTTP2Transport(t *testing.T) {
	t.Parallel()

	const headerTimeout = 7 * time.Second

	rt := proxy.NewTransportForTestWithTimeout(proxy.BackendProtocolH2C, nil, headerTimeout)

	tr, ok := rt.(*http.Transport)
	require.True(t, ok, "h2c transport must be the stdlib *http.Transport, got %T", rt)
	require.NotNil(t, tr.Protocols)
	assert.True(t, tr.Protocols.UnencryptedHTTP2(), "h2c transport must speak HTTP/2 over cleartext TCP")
	assert.False(t, tr.Protocols.HTTP1(), "HTTP/1 in the set makes http:// URLs use HTTP/1.1 instead of h2c")
	assert.Equal(t, headerTimeout, tr.ResponseHeaderTimeout)
	assert.Nil(t, tr.Proxy, "h2c prior knowledge cannot traverse an HTTP forward proxy from the environment")
	require.NotNil(t, tr.HTTP2)
	assert.NotZero(t, tr.HTTP2.SendPingTimeout,
		"a PING after idle time evicts dead TCP connections from the multiplexed pool")
	assert.NotZero(t, tr.HTTP2.PingTimeout, "bounds how long an unanswered PING keeps a connection")
	assert.NotNil(t, tr.DialContext, "the h2c dialer bounds TCP SYN waits")
}

// grpcTrailerBackend answers like a unary gRPC server: headers, one body
// frame, then grpc-status and grpc-message as HTTP/2 trailers. It records
// the protocol the request arrived on.
func grpcTrailerBackend(t *testing.T, proto *atomic.Value) *httptest.Server {
	t.Helper()

	return newH2COnlyBackend(t, http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		proto.Store(req.Proto)

		_, _ = io.Copy(io.Discard, req.Body)

		writer.Header().Set("Content-Type", "application/grpc")
		writer.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte{0, 0, 0, 0, 0})

		writer.Header().Set("Grpc-Status", "5")
		writer.Header().Set("Grpc-Message", "not found")
	}))
}

func TestHandler_H2C_GRPCTrailers_TunnelMode(t *testing.T) {
	t.Parallel()

	for _, mode := range tunnelWriterModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			var proto atomic.Value

			backend := grpcTrailerBackend(t, &proto)
			handler := newH2CHandler(t, backend.URL, &proxy.RouteTimeouts{Request: 5 * time.Second})
			fake := mode.new()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
				"http://app.example.com/pkg.Svc/Get", http.NoBody)
			req.ProtoMajor, req.ProtoMinor, req.Proto = 2, 0, "HTTP/2.0"
			req.Header.Set("Content-Type", "application/grpc")
			req.Header.Set("TE", "trailers")

			handler.ServeHTTP(fake, req)

			assert.Equal(t, http.StatusOK, fake.Status())
			assert.Equal(t, "HTTP/2.0", proto.Load(), "the backend leg must be h2c, not HTTP/1.1")
			assert.Equal(t, "5", fake.Header().Get("Grpc-Status"), "grpc-status trailer must reach the tunnel writer")
			assert.Equal(t, "not found", fake.Header().Get("Grpc-Message"))
		})
	}
}

func TestHandler_H2C_GRPCTrailers_HTTPTest(t *testing.T) {
	t.Parallel()

	var proto atomic.Value

	backend := grpcTrailerBackend(t, &proto)
	handler := newH2CHandler(t, backend.URL, nil)

	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/pkg.Svc/Get", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")

	resp, err := server.Client().Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "HTTP/2.0", proto.Load())
	assert.Equal(t, "5", resp.Trailer.Get("Grpc-Status"))
	assert.Equal(t, "not found", resp.Trailer.Get("Grpc-Message"))
}

func TestHandler_H2C_HeaderTimeout_TunnelMode(t *testing.T) {
	t.Parallel()

	for _, mode := range tunnelWriterModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			backend := newSlowHeadersH2CBackend(t, 800*time.Millisecond)
			handler := newH2CHandler(t, backend.URL, &proxy.RouteTimeouts{Backend: 100 * time.Millisecond})
			fake := mode.new()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)

			start := time.Now()

			handler.ServeHTTP(fake, req)

			assert.Equal(t, http.StatusGatewayTimeout, fake.Status())
			assert.Less(t, time.Since(start), 500*time.Millisecond,
				"the header deadline must fire near 100ms, not wait out the 800ms backend stall")
		})
	}
}

func TestHandler_H2C_StreamingSurvivesHeaderTimeout_TunnelMode(t *testing.T) {
	t.Parallel()

	for _, mode := range tunnelWriterModes {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			const frameCount = 4

			backend := newStreamingH2CBackend(t, frameCount, 250*time.Millisecond)
			handler := newH2CHandler(t, backend.URL, &proxy.RouteTimeouts{Request: 150 * time.Millisecond})
			fake := mode.new()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/sse", nil)

			start := time.Now()

			handler.ServeHTTP(fake, req)

			assert.Equal(t, http.StatusOK, fake.Status())
			assert.Contains(t, string(fake.Body()), "data: event-3", "the body must stream past the header deadline")
			assert.Greater(t, time.Since(start), 750*time.Millisecond,
				"a shorter run means the stream was cut before the backend finished")
		})
	}
}

// A client that goes away while the h2c backend has not answered must
// cancel the backend stream and must not be reported as a gateway timeout.
func TestHandler_H2C_ClientCancelAbortsBackendStream(t *testing.T) {
	t.Parallel()

	backendSawCancel := make(chan struct{})

	backend := newH2COnlyBackend(t, http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		select {
		case <-req.Context().Done():
			close(backendSawCancel)
		case <-time.After(5 * time.Second):
		}
	}))
	handler := newH2CHandler(t, backend.URL, &proxy.RouteTimeouts{Request: 5 * time.Second})

	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://app.example.com/", nil)
	fake := newFakeCloudflaredRespWriter()

	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()

	handler.ServeHTTP(fake, req)

	assert.Less(t, time.Since(start), 2*time.Second, "the handler must return once the client is gone")
	assert.NotEqual(t, http.StatusGatewayTimeout, fake.Status())

	select {
	case <-backendSawCancel:
	case <-time.After(2 * time.Second):
		t.Fatal("the backend h2c stream was not cancelled after the client went away")
	}
}

// PruneTransports must reach the pooled h2c connection: once the backend
// drops out of the config, its idle multiplexed connection is closed.
func TestHandler_PruneTransports_ClosesIdleH2CConnection(t *testing.T) {
	t.Parallel()

	closed := make(chan struct{}, 1)

	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))

	var protocols http.Protocols

	protocols.SetUnencryptedHTTP2(true)
	backend.Config.Protocols = &protocols
	backend.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	backend.Start()
	t.Cleanup(backend.Close)

	handler := newH2CHandler(t, backend.URL, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	select {
	case <-closed:
		t.Fatal("the h2c connection closed before PruneTransports ran")
	case <-time.After(100 * time.Millisecond):
	}

	handler.PruneTransports(map[string]bool{})

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("PruneTransports did not close the idle h2c connection")
	}
}
