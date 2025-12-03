// Package logging provides structured logging utilities with context propagation
// and OpenTelemetry trace integration.
package logging

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

type contextKey int

const (
	loggerKey contextKey = iota
	reconcileIDKey
)

// FromContext extracts logger from context.
// Falls back to slog.Default() if no logger is present.
func FromContext(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return logger
	}

	return slog.Default()
}

// WithLogger returns a new context with the given logger embedded.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

// Component returns a logger with component name preset.
// The logger is extracted from context and enriched with "component" attribute.
func Component(ctx context.Context, name string) *slog.Logger {
	return FromContext(ctx).With("component", name)
}

// WithReconcileID adds a reconcile-scoped request ID to context and returns
// a new context with the logger enriched with this ID.
func WithReconcileID(ctx context.Context) context.Context {
	reconcileID := uuid.New().String()[:8]
	logger := FromContext(ctx).With("reconcile_id", reconcileID)

	ctx = context.WithValue(ctx, reconcileIDKey, reconcileID)

	return WithLogger(ctx, logger)
}

// ReconcileIDFromContext extracts the reconcile ID from context.
// Returns empty string if not present.
func ReconcileIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(reconcileIDKey).(string); ok {
		return id
	}

	return ""
}
