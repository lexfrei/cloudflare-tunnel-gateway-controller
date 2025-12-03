package logging_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/trace"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
)

func TestTraceHandler_WithValidSpanContext(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	baseHandler := slog.NewJSONHandler(buf, nil)
	traceHandler := logging.NewTraceHandler(baseHandler)
	logger := slog.New(traceHandler)

	traceID, err := trace.TraceIDFromHex("00000000000000000000000000000001")
	if err != nil {
		t.Fatal(err)
	}

	spanID, err := trace.SpanIDFromHex("0000000000000001")
	if err != nil {
		t.Fatal(err)
	}

	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	logger.InfoContext(ctx, "traced message")

	output := buf.String()
	assert.Contains(t, output, "traced message")
	assert.Contains(t, output, "trace_id")
	assert.Contains(t, output, "span_id")
	assert.Contains(t, output, "00000000000000000000000000000001")
	assert.Contains(t, output, "0000000000000001")
}

func TestTraceHandler_WithoutSpanContext(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	baseHandler := slog.NewJSONHandler(buf, nil)
	traceHandler := logging.NewTraceHandler(baseHandler)
	logger := slog.New(traceHandler)

	logger.InfoContext(context.Background(), "untraced message")

	output := buf.String()
	assert.Contains(t, output, "untraced message")
	assert.NotContains(t, output, "trace_id")
	assert.NotContains(t, output, "span_id")
}

func TestTraceHandler_WithAttrs(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	baseHandler := slog.NewJSONHandler(buf, nil)
	traceHandler := logging.NewTraceHandler(baseHandler)

	handlerWithAttrs := traceHandler.WithAttrs([]slog.Attr{
		slog.String("preset_key", "preset_value"),
	})
	logger := slog.New(handlerWithAttrs)

	logger.Info("message with attrs")

	output := buf.String()
	assert.Contains(t, output, "preset_key")
	assert.Contains(t, output, "preset_value")
}

func TestTraceHandler_WithGroup(t *testing.T) {
	t.Parallel()

	buf := &bytes.Buffer{}
	baseHandler := slog.NewJSONHandler(buf, nil)
	traceHandler := logging.NewTraceHandler(baseHandler)

	handlerWithGroup := traceHandler.WithGroup("mygroup")
	logger := slog.New(handlerWithGroup)

	logger.Info("grouped message", "key", "value")

	output := buf.String()
	assert.Contains(t, output, "mygroup")
}
