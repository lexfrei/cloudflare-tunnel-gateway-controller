package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvOrDefault_DefaultValue(t *testing.T) {
	t.Parallel()

	result := envOrDefault("NONEXISTENT_ENV_VAR_FOR_TESTING_12345", "fallback")
	assert.Equal(t, "fallback", result)
}

func TestEnvOrDefault_EnvSet(t *testing.T) {
	t.Setenv("TEST_ENV_OR_DEFAULT", "custom")

	result := envOrDefault("TEST_ENV_OR_DEFAULT", "fallback")
	assert.Equal(t, "custom", result)
}

func TestNewServer(t *testing.T) {
	t.Parallel()

	handler := http.NewServeMux()
	server := newServer(":0", handler)

	require.NotNil(t, server)
	assert.Equal(t, ":0", server.Addr)
	assert.Equal(t, readHeaderTimeout, server.ReadHeaderTimeout)
	assert.Equal(t, configReadTimeout, server.ReadTimeout)
	assert.Equal(t, configWriteTimeout, server.WriteTimeout)
}

func TestHandleSignals_CancelOnSignal(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	logger := slog.Default()

	done := make(chan struct{})

	go func() {
		handleSignals(ctx, logger, cancel, sigChan)
		close(done)
	}()

	// Send signal — handler should cancel context.
	sigChan <- os.Interrupt

	<-done
	assert.Error(t, ctx.Err(), "context should be cancelled after signal")
}

func TestHandleSignals_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	sigChan := make(chan os.Signal, 1)
	logger := slog.Default()

	done := make(chan struct{})

	go func() {
		handleSignals(ctx, logger, cancel, sigChan)
		close(done)
	}()

	// Cancel context — handler should exit without signal.
	cancel()

	<-done
}

func TestConstants(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ":8081", defaultConfigAddr)
	assert.Equal(t, ":8080", defaultProxyAddr)
}
