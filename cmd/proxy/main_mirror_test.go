package main

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestHandlerOptions_MirrorLimitOnlyWhenSet pins the wiring from the env var to
// the handler: a valid limit adds exactly one option, and an unset one adds
// none. The remaining handler env vars are pinned empty so the count compares
// like with like.
func TestHandlerOptions_MirrorLimitOnlyWhenSet(t *testing.T) {
	for _, name := range []string{
		"PROXY_WS_DIAL_TIMEOUT", "PROXY_WS_HANDSHAKE_TIMEOUT", "PROXY_WS_IDLE_TIMEOUT",
		"PROXY_ACCESS_LOG_ENABLED", "PROXY_TRACING_ENABLED", "PROXY_ALLOW_X_ORIGINAL_HOST",
	} {
		t.Setenv(name, "")
	}

	logger := slog.New(slog.DiscardHandler)

	t.Setenv(mirrorMaxInFlightEnv, "")
	base := len(handlerOptions(logger))

	t.Setenv(mirrorMaxInFlightEnv, "128")
	assert.Len(t, handlerOptions(logger), base+1)
}

// TestMirrorMaxInFlight_Matrix pins PROXY_MIRROR_MAX_IN_FLIGHT parsing. Unset,
// malformed and non-positive values all mean "keep the built-in cap": a typo
// must never uncap mirroring or turn it off.
func TestMirrorMaxInFlight_Matrix(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "unset", value: "", want: 0},
		{name: "positive", value: "256", want: 256},
		{name: "surrounding space", value: " 16 ", want: 16},
		{name: "zero", value: "0", want: 0},
		{name: "negative", value: "-4", want: 0},
		{name: "not a number", value: "lots", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Parallel skipped: t.Setenv mutates process env.
			t.Setenv(mirrorMaxInFlightEnv, tt.value)

			assert.Equal(t, tt.want, mirrorMaxInFlight(slog.New(slog.DiscardHandler)))
		})
	}
}
