// Package tracing wires an OpenTelemetry TracerProvider and W3C trace-context
// propagator into the controller and proxy binaries. It is off by default: when
// disabled it leaves the global no-op provider in place so there is zero cost on
// the request hot path, and the existing slog trace-enrichment (internal/logging)
// simply never sees a valid span context.
package tracing

import (
	"context"
	"net/http"
	"strings"

	"github.com/cockroachdb/errors"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// Config configures distributed tracing for a binary.
type Config struct {
	// Enabled turns tracing on. When false, Setup is a no-op.
	Enabled bool
	// Endpoint is the OTLP collector address. When empty, the exporter honors
	// the standard OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
	// environment variables. A value with a scheme (http:// or https://) selects
	// plaintext vs TLS; a bare host:port uses plaintext gRPC.
	Endpoint string
	// SampleRate is the head-sampling probability in [0, 1] applied at the trace
	// root via ParentBased(TraceIDRatioBased).
	SampleRate float64
	// ServiceName is the OTel service.name resource attribute ("controller" | "proxy").
	ServiceName string
	// Version is the OTel service.version resource attribute.
	Version string
}

// noopShutdown is the shutdown function returned when tracing is disabled.
func noopShutdown(context.Context) error { return nil }

// Setup installs a global OpenTelemetry TracerProvider and text-map propagator.
// It returns a shutdown function that flushes and closes the exporter.
//
// When cfg.Enabled is false it leaves the default no-op provider and propagator
// in place and returns a no-op shutdown — zero runtime cost on the hot path, and
// no change to outbound header forwarding.
func Setup(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	if !cfg.Enabled {
		return noopShutdown, nil
	}

	exporter, err := newExporter(ctx, cfg)
	if err != nil {
		return nil, err
	}

	provider, err := newTracerProvider(exporter, cfg)
	if err != nil {
		return nil, err
	}

	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return provider.Shutdown, nil
}

// WrapTransport returns base wrapped with otelhttp.NewTransport when enabled,
// so an outbound HTTP client emits a SpanKindClient span per call and injects
// the active span's W3C trace context into the request. When disabled it
// returns base unchanged, keeping the outbound path byte-identical. A nil base
// with enabled defaults to http.DefaultTransport under the wrapper.
func WrapTransport(base http.RoundTripper, enabled bool) http.RoundTripper {
	if !enabled {
		return base
	}

	if base == nil {
		base = http.DefaultTransport
	}

	return otelhttp.NewTransport(base)
}

// newExporter builds the OTLP/gRPC span exporter. The gRPC client dials lazily,
// so a missing collector does not fail Setup — spans are dropped until the
// collector becomes reachable.
func newExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	var opts []otlptracegrpc.Option

	switch {
	case cfg.Endpoint == "":
		// Honor the standard OTEL_EXPORTER_OTLP_* environment variables.
	case strings.Contains(cfg.Endpoint, "://"):
		// A scheme is present: let the exporter parse http:// (plaintext) vs
		// https:// (TLS) from the URL.
		opts = append(opts, otlptracegrpc.WithEndpointURL(cfg.Endpoint))
	default:
		// Bare host:port defaults to plaintext gRPC, matching the common
		// in-cluster otel-collector:4317 deployment.
		opts = append(opts, otlptracegrpc.WithEndpoint(cfg.Endpoint), otlptracegrpc.WithInsecure())
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, errors.Wrap(err, "create OTLP trace exporter")
	}

	return exporter, nil
}

// newTracerProvider builds a TracerProvider from an exporter and config. It is
// separated from Setup so tests can drive it with an in-memory exporter.
func newTracerProvider(exporter sdktrace.SpanExporter, cfg Config) (*sdktrace.TracerProvider, error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.Version),
		),
	)
	if err != nil {
		return nil, errors.Wrap(err, "build tracing resource")
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		// ParentBased respects an upstream sampled/dropped decision and only
		// rolls the ratio at the trace root, so partial traces stay intact when
		// the proxy sits mid-chain behind the edge.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(clampSampleRate(cfg.SampleRate)))),
		sdktrace.WithResource(res),
	)

	return provider, nil
}

// clampSampleRate brings an out-of-range sampling probability into [0, 1].
// A value above 1 already means "always sample"; a negative value is only ever
// an operator typo (0 is the valid "sample nothing" setting). Both out-of-range
// cases collapse to full sampling so a malformed flag / env never silently
// drops every trace the way bare TraceIDRatioBased(<0) would. The Helm schema
// already bounds the chart path to [0, 1]; this guards the raw env / cobra-flag
// paths that bypass it.
func clampSampleRate(rate float64) float64 {
	if rate < 0 || rate > 1 {
		return 1.0
	}

	return rate
}
