package tracing

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// saveGlobals captures the process-global TracerProvider and propagator so a
// test can restore them afterwards. These tests mutate process-global OTel
// state and therefore intentionally do NOT call t.Parallel().
func saveGlobals(t *testing.T) {
	t.Helper()

	tp := otel.GetTracerProvider()
	prop := otel.GetTextMapPropagator()

	t.Cleanup(func() {
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(prop)
	})
}

func TestSetup_Disabled_LeavesGlobalsUntouched(t *testing.T) {
	saveGlobals(t)

	before := otel.GetTextMapPropagator().Fields()

	shutdown, err := Setup(context.Background(), Config{Enabled: false})
	require.NoError(t, err)
	require.NotNil(t, shutdown, "shutdown must be non-nil even when disabled")

	// Disabled setup must not install a propagator.
	assert.Equal(t, before, otel.GetTextMapPropagator().Fields())

	// The no-op shutdown returns nil.
	assert.NoError(t, shutdown(context.Background()))
}

func TestSetup_Enabled_InstallsCompositePropagator(t *testing.T) {
	saveGlobals(t)

	shutdown, err := Setup(context.Background(), Config{
		Enabled:     true,
		ServiceName: "proxy",
		Version:     "test",
		SampleRate:  1.0,
	})
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	})

	fields := otel.GetTextMapPropagator().Fields()
	assert.Contains(t, fields, "traceparent", "W3C TraceContext propagator must be installed")
	assert.Contains(t, fields, "baggage", "Baggage propagator must be installed")
}

// TestNewTracerProvider_SamplesAndTagsResource exercises the provider builder
// against an in-memory exporter — no live collector — and proves that the
// resource carries service.name and that ParentBased(TraceIDRatioBased(1.0))
// records a root span.
func TestNewTracerProvider_SamplesAndTagsResource(t *testing.T) {
	t.Parallel()

	exporter := tracetest.NewInMemoryExporter()

	provider, err := newTracerProvider(exporter, Config{
		Enabled:     true,
		ServiceName: "proxy",
		Version:     "v9.9.9",
		SampleRate:  1.0,
	})
	require.NoError(t, err)

	_, span := provider.Tracer("test").Start(context.Background(), "root")
	span.End()

	require.NoError(t, provider.ForceFlush(context.Background()))

	spans := exporter.GetSpans()
	require.Len(t, spans, 1, "rate 1.0 must record the root span")

	res := spans[0].Resource
	require.NotNil(t, res)
	assert.Contains(t, res.Attributes(), attribute.String("service.name", "proxy"))
	assert.Contains(t, res.Attributes(), attribute.String("service.version", "v9.9.9"))

	require.NoError(t, provider.Shutdown(context.Background()))
}

// TestClampSampleRate pins the out-of-range guard: 0 and valid fractions pass
// through; a negative rate (only ever a typo — 0 is the valid "sample nothing"
// setting) and any rate above 1 collapse to full sampling, so a malformed
// --tracing-sample-rate / PROXY_TRACING_SAMPLE_RATE never silently drops every
// trace.
func TestClampSampleRate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{name: "zero is valid (sample nothing)", in: 0, want: 0},
		{name: "half passes through", in: 0.5, want: 0.5},
		{name: "one passes through", in: 1, want: 1},
		{name: "negative typo defaults to full sampling", in: -0.1, want: 1},
		{name: "above one collapses to full sampling", in: 50, want: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.InDelta(t, tc.want, clampSampleRate(tc.in), 1e-9)
		})
	}
}

// TestNewTracerProvider_NegativeRateFullySamples proves the guard end-to-end: a
// negative rate records the root span (full sampling) rather than dropping it
// the way bare TraceIDRatioBased(<0) would.
func TestNewTracerProvider_NegativeRateFullySamples(t *testing.T) {
	t.Parallel()

	exporter := tracetest.NewInMemoryExporter()

	provider, err := newTracerProvider(exporter, Config{
		Enabled:     true,
		ServiceName: "proxy",
		SampleRate:  -0.1,
	})
	require.NoError(t, err)

	_, span := provider.Tracer("test").Start(context.Background(), "root")
	span.End()

	require.NoError(t, provider.ForceFlush(context.Background()))
	assert.Len(t, exporter.GetSpans(), 1,
		"a negative (typo) rate must default to full sampling, not silently drop the trace")

	require.NoError(t, provider.Shutdown(context.Background()))
}

// TestNewTracerProvider_ZeroRateDropsRootSpan proves the ratio sampler honors a
// 0.0 rate by dropping a root span (no parent decision to inherit).
func TestNewTracerProvider_ZeroRateDropsRootSpan(t *testing.T) {
	t.Parallel()

	exporter := tracetest.NewInMemoryExporter()

	provider, err := newTracerProvider(exporter, Config{
		Enabled:     true,
		ServiceName: "proxy",
		SampleRate:  0.0,
	})
	require.NoError(t, err)

	_, span := provider.Tracer("test").Start(context.Background(), "root")
	span.End()

	require.NoError(t, provider.ForceFlush(context.Background()))
	assert.Empty(t, exporter.GetSpans(), "rate 0.0 must drop the root span")

	require.NoError(t, provider.Shutdown(context.Background()))

	var _ sdktrace.SpanExporter = exporter
}
