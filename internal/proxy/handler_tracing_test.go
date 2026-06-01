package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// onlyServerSpan returns the single recorded SpanKindServer span, failing if
// there is not exactly one.
func onlyServerSpan(t *testing.T, recorder *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()

	var servers []sdktrace.ReadOnlySpan

	for _, span := range recorder.Ended() {
		if span.SpanKind() == trace.SpanKindServer {
			servers = append(servers, span)
		}
	}

	require.Len(t, servers, 1, "expected exactly one server span")

	return servers[0]
}

// traceparentParts splits a W3C traceparent header into its trace-id and
// span-id fields.
func traceparentParts(t *testing.T, header string) (string, string) {
	t.Helper()

	parts := strings.Split(header, "-")
	require.Len(t, parts, 4, "traceparent must have 4 hyphen-separated fields")

	return parts[1], parts[2]
}

// w3cTraceparent is the W3C trace-context example header. trace-id =
// 0af7651916cd43dd8448eb211c80319c, parent span-id = b7ad6b7169203331, sampled.
const w3cTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

// installRecordingTracer sets a global TracerProvider backed by an in-memory
// SpanRecorder plus a W3C TraceContext propagator, restoring the prior globals
// on cleanup. Tests using it must NOT run in parallel — they mutate
// process-global OTel state.
func installRecordingTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))

	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	t.Cleanup(func() {
		_ = provider.Shutdown(t.Context())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	return recorder
}

// TestHandler_Tracing_ServerSpanParentedFromTraceparent proves that with
// tracing enabled the handler extracts the inbound W3C trace context and emits
// a single SpanKindServer span whose trace-id and parent span-id match the
// incoming traceparent header.
func TestHandler_Tracing_ServerSpanParentedFromTraceparent(t *testing.T) {
	recorder := installRecordingTracer(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	handler := proxy.NewHandler(router, proxy.WithTracing("proxy"))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/api", nil)
	req.Header.Set("Traceparent", w3cTraceparent)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	// The backend call also emits a client span; select the server span by kind.
	var serverSpans []sdktrace.ReadOnlySpan

	for _, span := range recorder.Ended() {
		if span.SpanKind() == trace.SpanKindServer {
			serverSpans = append(serverSpans, span)
		}
	}

	require.Len(t, serverSpans, 1, "exactly one server span must be recorded")

	span := serverSpans[0]
	assert.Equal(t, "0af7651916cd43dd8448eb211c80319c", span.SpanContext().TraceID().String(),
		"server span must inherit the inbound trace-id")
	assert.Equal(t, "b7ad6b7169203331", span.Parent().SpanID().String(),
		"server span's parent must be the inbound traceparent span-id")
}

// TestHandler_Tracing_NoServerSpanWhenDisabled confirms the disabled path
// records nothing even when a recording provider is installed globally — the
// handler must not call into the tracer when WithTracing was not set.
func TestHandler_Tracing_NoServerSpanWhenDisabled(t *testing.T) {
	recorder := installRecordingTracer(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	// No WithTracing option.
	handler := proxy.NewHandler(router)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/api", nil)
	req.Header.Set("Traceparent", w3cTraceparent)

	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Empty(t, recorder.Ended(), "a handler without WithTracing must not create spans")
}

// TestHandler_Tracing_ServerSpanRecords5xxAsError pins the most operationally
// valuable branch of endServerSpan: a 5xx backend response sets the span status
// to Error and records the status_code attribute, so failing requests surface
// in trace-backend error views.
func TestHandler_Tracing_ServerSpanRecords5xxAsError(t *testing.T) {
	recorder := installRecordingTracer(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(backend.Close)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	handler := proxy.NewHandler(router, proxy.WithTracing("proxy"))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/api", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	span := onlyServerSpan(t, recorder)
	assert.Equal(t, codes.Error, span.Status().Code, "a 5xx response must mark the server span as an error")
	assert.Contains(t, span.Attributes(), attribute.Int("http.response.status_code", http.StatusInternalServerError),
		"the server span must record the 5xx status code")
}

// TestHandler_Tracing_ServerSpanRecords2xxStatus pins that a non-error response
// records the status_code attribute and leaves the span status Unset (not
// flagged as an error).
func TestHandler_Tracing_ServerSpanRecords2xxStatus(t *testing.T) {
	recorder := installRecordingTracer(t)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	handler := proxy.NewHandler(router, proxy.WithTracing("proxy"))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/api", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	span := onlyServerSpan(t, recorder)
	assert.Equal(t, codes.Unset, span.Status().Code, "a 2xx response must not mark the span as an error")
	assert.Contains(t, span.Attributes(), attribute.Int("http.response.status_code", http.StatusOK))
}

// TestHandler_Tracing_ServerSpanTagsWebSocketUpgrade pins the 101 carve-out and
// proves the branch is reachable: pipeWebSocket calls WriteHeader(101) on the
// countingResponseWriter, which records 101 in its own counter (the underlying
// cloudflared writer translates that to 200 on the wire). endServerSpan then
// tags the span as an upgrade instead of recording a latency-shaped status code.
func TestHandler_Tracing_ServerSpanTagsWebSocketUpgrade(t *testing.T) {
	recorder := installRecordingTracer(t)

	backend := newWSEchoBackend(t, false)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
			Backends: []proxy.BackendRef{
				{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolHTTP, WebSocket: true},
			},
		}},
	}))

	handler := proxy.NewHandler(router, proxy.WithTracing("proxy"))
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

	require.Eventually(t, fake.Hijacked, 5*time.Second, 25*time.Millisecond, "handler must reach the post-101 hijack")

	// Closing the client unblocks the bidirectional copy; ServeHTTP returns and
	// the deferred endServerSpan ends the span, after which it appears in Ended().
	_ = fake.HijackedClient().Close()
	<-handlerDone

	span := onlyServerSpan(t, recorder)
	assert.Contains(t, span.Attributes(), attribute.Bool("http.connection.upgrade", true),
		"a WebSocket upgrade must tag the server span as an upgrade")
	assert.NotContains(t, span.Attributes(), attribute.Int("http.response.status_code", http.StatusSwitchingProtocols),
		"the upgrade branch must not record a latency-shaped status code")
}

// TestHandler_Tracing_WebSocketHijackStillWorks pins that enabling tracing —
// which inserts a countingResponseWriter into the writer chain — does NOT break
// the tunnel-mode WebSocket hijack. The countingResponseWriter must remain
// transparent to the direct w.(http.Hijacker) assertion in the upgrade path.
// Uses the fakeCloudflaredRespWriter per the "validate the tunnel transport"
// principle; a no-op global tracer is sufficient (we assert the WS path, not
// the span).
func TestHandler_Tracing_WebSocketHijackStillWorks(t *testing.T) {
	t.Parallel()

	backend := newWSEchoBackend(t, false)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{
			{
				Matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{Type: proxy.PathMatchPathPrefix, Value: "/"}}},
				Backends: []proxy.BackendRef{
					{URL: backend.URL, Weight: 1, Protocol: proxy.BackendProtocolHTTP, WebSocket: true},
				},
			},
		},
	}))

	handler := proxy.NewHandler(router, proxy.WithTracing("proxy"))
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

	require.Eventually(t, fake.Hijacked, 5*time.Second, 25*time.Millisecond,
		"tracing-enabled handler must still reach the post-101 hijack; the countingResponseWriter "+
			"inserted for span status must delegate Hijack to the inner cloudflared writer")

	assert.Equal(t, http.StatusOK, fake.Status(),
		"recorded status must be 200 — WriteHeader(101) translated, proving status written before hijack")

	_ = fake.HijackedClient().Close()
	<-handlerDone
}

// TestHandler_Tracing_BackendRequestInjectsChildContext proves the outbound
// path: with tracing on, the backend receives a traceparent whose trace-id
// matches the server span and whose span-id is the proxy's client span — i.e.
// otelhttp.NewTransport emits a SpanKindClient span parented on the server span
// and injects its context. The incoming request has no traceparent, so the
// server span is the root and the hierarchy is unambiguous.
func TestHandler_Tracing_BackendRequestInjectsChildContext(t *testing.T) {
	recorder := installRecordingTracer(t)

	var gotTraceparent string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("Traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	handler := proxy.NewHandler(router, proxy.WithTracing("proxy"))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/api", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	require.NotEmpty(t, gotTraceparent, "backend must receive a traceparent injected by otelhttp.NewTransport")

	traceID, spanID := traceparentParts(t, gotTraceparent)

	spans := recorder.Ended()
	require.Len(t, spans, 2, "a server span and a client span must be recorded")

	var serverSpan, clientSpan sdktrace.ReadOnlySpan

	for _, span := range spans {
		if span.SpanKind() == trace.SpanKindServer {
			serverSpan = span
		}

		if span.SpanKind() == trace.SpanKindClient {
			clientSpan = span
		}
	}

	require.NotNil(t, serverSpan, "a SpanKindServer span must exist")
	require.NotNil(t, clientSpan, "a SpanKindClient span (backend call) must exist")

	assert.Equal(t, serverSpan.SpanContext().SpanID(), clientSpan.Parent().SpanID(),
		"client span must be a child of the server span")
	assert.Equal(t, serverSpan.SpanContext().TraceID().String(), traceID,
		"backend traceparent trace-id must match the server span")
	assert.Equal(t, clientSpan.SpanContext().SpanID().String(), spanID,
		"backend traceparent span-id must be the proxy's client span — Inject overwrites, not appends")
}

// TestHandler_Tracing_DisabledForwardsTraceparentVerbatim guards against a
// header-forwarding regression: with tracing OFF, an inbound traceparent must
// reach the backend unchanged. httputil.ReverseProxy forwards it and, because
// the transport is not wrapped when disabled, nothing rewrites it.
func TestHandler_Tracing_DisabledForwardsTraceparentVerbatim(t *testing.T) {
	t.Parallel()

	var gotTraceparent string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("Traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	handler := proxy.NewHandler(router) // tracing disabled

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/api", nil)
	req.Header.Set("Traceparent", w3cTraceparent)

	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, w3cTraceparent, gotTraceparent,
		"with tracing disabled the inbound traceparent must reach the backend verbatim")
}

// TestHandler_Tracing_PrunableTransportPool pins the PruneTransports trap fix:
// the per-backend transport pool must cache the INNER transport (which exposes
// CloseIdleConnections), not the otelhttp wrapper. otelhttp.NewTransport is
// applied at the use site, so PruneTransports can still evict and close idle
// connections with tracing enabled.
func TestHandler_Tracing_PrunableTransportPool(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Hostnames: []string{"app.example.com"},
			Backends:  []proxy.BackendRef{{URL: backend.URL, Weight: 1}},
		}},
	}))

	handler := proxy.NewHandler(router, proxy.WithTracing("proxy"))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.com/api", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	pooled := 0

	handler.Transports().Range(func(_, value any) bool {
		pooled++

		_, ok := value.(interface{ CloseIdleConnections() })
		assert.True(t, ok,
			"pooled transport must expose CloseIdleConnections so PruneTransports can evict it — "+
				"the otelhttp wrapper must NOT be what is cached")

		return true
	})
	require.Equal(t, 1, pooled, "one backend transport must be cached after the request")

	// Eviction with no active keys must drain the pool.
	handler.PruneTransports(map[string]bool{})

	remaining := 0

	handler.Transports().Range(func(_, _ any) bool {
		remaining++

		return true
	})
	assert.Zero(t, remaining, "PruneTransports must evict the stale transport with tracing enabled")
}
