package logging

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// TraceHandler wraps slog.Handler to automatically inject OpenTelemetry
// trace context (trace_id, span_id) into log records.
type TraceHandler struct {
	slog.Handler
}

// NewTraceHandler creates a new TraceHandler wrapping the given handler.
func NewTraceHandler(h slog.Handler) *TraceHandler {
	return &TraceHandler{Handler: h}
}

// Handle injects trace_id and span_id from OpenTelemetry context into the log record.
//
//nolint:gocritic // slog.Handler interface requires value receiver for Record
func (h *TraceHandler) Handle(ctx context.Context, r slog.Record) error {
	spanCtx := trace.SpanContextFromContext(ctx)
	if spanCtx.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", spanCtx.TraceID().String()),
			slog.String("span_id", spanCtx.SpanID().String()),
		)
	}

	//nolint:wrapcheck // Handler interface requires unwrapped error
	return h.Handler.Handle(ctx, r)
}

// WithAttrs returns a new TraceHandler with additional attributes.
func (h *TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup returns a new TraceHandler with a new group.
func (h *TraceHandler) WithGroup(name string) slog.Handler {
	return &TraceHandler{Handler: h.Handler.WithGroup(name)}
}
