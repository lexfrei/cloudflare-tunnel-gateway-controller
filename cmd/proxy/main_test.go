package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

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

// TestParseWSEnvDurations_Matrix pins the env-var-to-duration
// translation in parseWSEnvDurations: three branches per env var
// (unset / unparseable / parseable). Without this, a future edit
// that swaps PROXY_WS_DIAL_TIMEOUT and PROXY_WS_HANDSHAKE_TIMEOUT,
// typos one of them, or wires the dial result to the handshake env
// would silently fall through to the 30s package default with no
// test catching it.
//
// Parsing tested directly (not through the wsHandlerOptions ->
// proxy.NewHandler -> EffectiveWS*ForTest chain) because the
// EffectiveWS*ForTest helpers live in internal/proxy/export_test.go
// and are invisible to cmd/proxy. The downstream wsHandlerOptions
// composition is mechanical: zero -> no opt, > 0 -> With* opt; the
// proxy package's TestNewHandler_WSTimeoutOptions exhaustively pins
// that translation. Testing the env-parse boundary here closes the
// gap end-to-end.
func TestParseWSEnvDurations_Matrix(t *testing.T) {
	tests := []struct {
		name              string
		dialEnv           string
		handshakeEnv      string
		setDialEnv        bool
		setHandshakeEnv   bool
		wantDial          time.Duration
		wantHandshakeRead time.Duration
		wantWarn          bool
	}{
		{
			name:              "both env vars unset returns zeros",
			wantDial:          0,
			wantHandshakeRead: 0,
		},
		{
			name:              "parseable dial env returns the parsed duration",
			dialEnv:           "5s",
			setDialEnv:        true,
			wantDial:          5 * time.Second,
			wantHandshakeRead: 0,
		},
		{
			name:              "parseable handshake env returns the parsed duration",
			handshakeEnv:      "7s",
			setHandshakeEnv:   true,
			wantDial:          0,
			wantHandshakeRead: 7 * time.Second,
		},
		{
			name:              "both parseable values returned independently",
			dialEnv:           "2s",
			handshakeEnv:      "3s",
			setDialEnv:        true,
			setHandshakeEnv:   true,
			wantDial:          2 * time.Second,
			wantHandshakeRead: 3 * time.Second,
		},
		{
			name:              "unparseable dial env returns zero and warns",
			dialEnv:           "bogus",
			setDialEnv:        true,
			wantDial:          0,
			wantHandshakeRead: 0,
			wantWarn:          true,
		},
		{
			name:              "unparseable handshake env returns zero and warns",
			handshakeEnv:      "also-bogus",
			setHandshakeEnv:   true,
			wantDial:          0,
			wantHandshakeRead: 0,
			wantWarn:          true,
		},
		{
			name:              "empty string is treated as unset",
			dialEnv:           "",
			setDialEnv:        true,
			wantDial:          0,
			wantHandshakeRead: 0,
		},
		{
			// time.ParseDuration accepts negative durations, so parseEnvDuration
			// returns them as-is. The > 0 gate in wsHandlerOptions then drops
			// the resulting option so the proxy defaults stick. Pinning the
			// parse-side behaviour here keeps the gate's purpose visible -- if
			// someone widens the gate to >= 0, this case still passes the
			// parsing step and the matching TestWSHandlerOptions_OptionCount
			// case ("negative dial => no opts") catches the dropped guard.
			name:              "negative dial is parseable and passes through",
			dialEnv:           "-5s",
			setDialEnv:        true,
			wantDial:          -5 * time.Second,
			wantHandshakeRead: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Parallel skipped: t.Setenv mutates process env, must run sequentially.
			if tt.setDialEnv {
				t.Setenv("PROXY_WS_DIAL_TIMEOUT", tt.dialEnv)
			} else {
				_ = os.Unsetenv("PROXY_WS_DIAL_TIMEOUT")
			}

			if tt.setHandshakeEnv {
				t.Setenv("PROXY_WS_HANDSHAKE_TIMEOUT", tt.handshakeEnv)
			} else {
				_ = os.Unsetenv("PROXY_WS_HANDSHAKE_TIMEOUT")
			}

			var logBuf bytes.Buffer

			logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

			dial, handshake := parseWSEnvDurations(logger)

			assert.Equal(t, tt.wantDial, dial,
				"dial duration must match expectation for env=%q", tt.dialEnv)
			assert.Equal(t, tt.wantHandshakeRead, handshake,
				"handshake duration must match expectation for env=%q", tt.handshakeEnv)

			if tt.wantWarn {
				assert.Contains(t, logBuf.String(), "failed to parse",
					"unparseable env var must surface a WARN")
			} else {
				assert.NotContains(t, logBuf.String(), "failed to parse",
					"parseable / unset env vars must NOT warn")
			}
		})
	}
}

// TestWSHandlerOptions_OptionCount pins the composition shape: every
// zero duration from parseWSEnvDurations drops to no option (so
// downstream proxy defaults stick), every positive duration emits one
// option. Catches a future regression that double-appends or drops
// the > 0 gate.
func TestWSHandlerOptions_OptionCount(t *testing.T) {
	tests := []struct {
		name          string
		dialEnv       string
		handshakeEnv  string
		wantOptionLen int
	}{
		{name: "no envs => no opts", wantOptionLen: 0},
		{name: "dial only", dialEnv: "1s", wantOptionLen: 1},
		{name: "handshake only", handshakeEnv: "1s", wantOptionLen: 1},
		{name: "both", dialEnv: "1s", handshakeEnv: "1s", wantOptionLen: 2},
		{name: "unparseable dial => no opts", dialEnv: "bogus", wantOptionLen: 0},
		// Negative duration parses cleanly but trips the > 0 gate in
		// wsHandlerOptions, so no option is emitted. Pins the guard so a
		// future "ge 0" widening fails here loudly instead of leaking a
		// negative timeout into proxy.NewHandler.
		{name: "negative dial => no opts", dialEnv: "-5s", wantOptionLen: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PROXY_WS_DIAL_TIMEOUT", tt.dialEnv)
			t.Setenv("PROXY_WS_HANDSHAKE_TIMEOUT", tt.handshakeEnv)

			logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn}))

			opts := wsHandlerOptions(logger)
			assert.Len(t, opts, tt.wantOptionLen)
		})
	}
}
