package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

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

	spans := recorder.Ended()
	require.Len(t, spans, 1, "exactly one server span must be recorded")

	span := spans[0]
	assert.Equal(t, trace.SpanKindServer, span.SpanKind())
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
