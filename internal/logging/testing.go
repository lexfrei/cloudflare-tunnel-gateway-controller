package logging

import (
	"bytes"
	"log/slog"
	"testing"
)

// TestLogger returns a logger that captures output for assertions.
// The returned buffer contains all log output in JSON format.
func TestLogger(tb testing.TB) (*slog.Logger, *bytes.Buffer) {
	tb.Helper()

	buf := &bytes.Buffer{}
	handler := slog.NewJSONHandler(buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	logger := slog.New(handler)

	return logger, buf
}

// DiscardLogger returns a logger that discards all output.
// Useful for tests that don't need to verify log output.
func DiscardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
