package logging_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
)

func TestFromContext_WithLogger(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	ctx := logging.WithLogger(context.Background(), logger)

	extractedLogger := logging.FromContext(ctx)
	extractedLogger.Info("test message")

	assert.Contains(t, buf.String(), "test message")
}

func TestFromContext_FallbackToDefault(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	logger := logging.FromContext(ctx)

	assert.NotNil(t, logger)
}

func TestComponent(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	ctx := logging.WithLogger(context.Background(), logger)

	componentLogger := logging.Component(ctx, "TestComponent")
	componentLogger.Info("component message")

	output := buf.String()
	assert.Contains(t, output, "component message")
	assert.Contains(t, output, "TestComponent")
	assert.Contains(t, output, `"component"`)
}

func TestWithReconcileID(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	ctx := logging.WithLogger(context.Background(), logger)

	ctx = logging.WithReconcileID(ctx)

	reconcileID := logging.ReconcileIDFromContext(ctx)
	require.NotEmpty(t, reconcileID)
	assert.Len(t, reconcileID, 8)

	logging.FromContext(ctx).Info("reconcile message")

	output := buf.String()
	assert.Contains(t, output, "reconcile message")
	assert.Contains(t, output, reconcileID)
	assert.Contains(t, output, `"reconcile_id"`)
}

func TestReconcileIDFromContext_NotPresent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reconcileID := logging.ReconcileIDFromContext(ctx)

	assert.Empty(t, reconcileID)
}

func TestDiscardLogger(t *testing.T) {
	t.Parallel()

	logger := logging.DiscardLogger()

	require.NotNil(t, logger)
	logger.Info("this should not panic")
}

func TestTestLogger(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)

	logger.Debug("debug message")
	logger.Info("info message")
	logger.Warn("warn message")

	output := buf.String()
	assert.Contains(t, output, "debug message")
	assert.Contains(t, output, "info message")
	assert.Contains(t, output, "warn message")

	lines := strings.Split(strings.TrimSpace(output), "\n")
	assert.Len(t, lines, 3)
}

func TestWithLogger_ChainedCalls(t *testing.T) {
	t.Parallel()

	logger1, buf1 := logging.TestLogger(t)
	logger2, buf2 := logging.TestLogger(t)

	ctx := context.Background()
	ctx = logging.WithLogger(ctx, logger1)

	logging.FromContext(ctx).Info("first logger")
	assert.Contains(t, buf1.String(), "first logger")
	assert.NotContains(t, buf2.String(), "first logger")

	ctx = logging.WithLogger(ctx, logger2)

	logging.FromContext(ctx).Info("second logger")
	assert.Contains(t, buf2.String(), "second logger")
}

func TestComponent_WithAttributes(t *testing.T) {
	t.Parallel()

	logger, buf := logging.TestLogger(t)
	ctx := logging.WithLogger(context.Background(), logger)

	componentLogger := logging.Component(ctx, "MyComponent").With(
		slog.String("key", "value"),
	)
	componentLogger.Info("enriched message")

	output := buf.String()
	assert.Contains(t, output, "MyComponent")
	assert.Contains(t, output, "key")
	assert.Contains(t, output, "value")
}
