package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigAPIAuthFor pins the mapping from run mode to auth requirement: the
// internet-facing tunnel mode requires an authenticated config API, standalone
// mode does not. TestRun_TunnelModeRefusesAnUnauthenticatedConfigAPI pins that
// run actually uses it.
func TestConfigAPIAuthFor(t *testing.T) {
	t.Parallel()

	assert.Equal(t, authRequired, configAPIAuthFor("tunnel-token"), "tunnel mode")
	assert.Equal(t, authOptional, configAPIAuthFor(""), "standalone mode")
}

// TestRun_TunnelModeRefusesAnUnauthenticatedConfigAPI pins the call site in
// run: a tunnel token with no config-API token must stop startup. It runs
// offline because resolveTunnelToken only checks for emptiness and
// buildDataPlane refuses before anything listens or dials.
func TestRun_TunnelModeRefusesAnUnauthenticatedConfigAPI(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "not-a-real-token")
	t.Setenv("PROXY_TRACING_ENABLED", "")
	t.Setenv("PROXY_TRACING_ENDPOINT", "")
	t.Setenv("PROXY_AUTH_TOKEN", "")
	t.Setenv(allowUnauthenticatedConfigAPIEnv, "")
	require.NoError(t, os.Unsetenv("PROXY_AUTH_TOKEN"))
	require.NoError(t, os.Unsetenv(allowUnauthenticatedConfigAPIEnv))

	var logs bytes.Buffer

	assert.Equal(t, 1, run(slog.New(slog.NewTextHandler(&logs, nil))))
	assert.Contains(t, logs.String(), "refusing to start with a broken config-API auth configuration")
}

// TestTunnelExitCode pins how tunnel mode's ending maps to the process exit
// code. A cancelled context is a requested shutdown, unless the cancel came
// from the config API failing to serve, which is a failure the exit code must
// report.
func TestTunnelExitCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		err          error
		configFailed bool
		want         int
	}{
		{name: "clean exit", err: nil, want: 0},
		{name: "requested shutdown", err: context.Canceled, want: 0},
		{name: "wrapped requested shutdown", err: fmt.Errorf("tunnel: %w", context.Canceled), want: 0},
		{name: "tunnel failure", err: errTestTunnelFailure, want: 1},
		{name: "config API failure cancelled the tunnel", err: context.Canceled, configFailed: true, want: 1},
		{name: "config API failure, tunnel ended cleanly", err: nil, configFailed: true, want: 1},
		{name: "config API failure and tunnel failure", err: errTestTunnelFailure, configFailed: true, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, tunnelExitCode(slog.New(slog.DiscardHandler), tt.err, tt.configFailed))
		})
	}
}

var errTestTunnelFailure = errors.New("edge unreachable")

// TestServeConfigAPI_RecordsAFailureBeforeCancelling pins the other half of
// the tunnel exit code: a config API that cannot serve cancels the tunnel and
// leaves the flag that tells tunnelExitCode the cancel was a failure.
func TestServeConfigAPI_RecordsAFailureBeforeCancelling(t *testing.T) {
	t.Parallel()

	occupied, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	t.Cleanup(func() { _ = occupied.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server := newServer(occupied.Addr().String(), http.NotFoundHandler())
	failed := serveConfigAPI(slog.New(slog.DiscardHandler), server, cancel)

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a config API that cannot bind must cancel the tunnel")
	}

	assert.True(t, failed.Load())
}

// TestServeConfigAPI_ShutdownIsNotAFailure pins that a normal shutdown leaves
// the flag clear and the tunnel's context alone.
func TestServeConfigAPI_ShutdownIsNotAFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	server := newServer("127.0.0.1:0", http.NotFoundHandler())
	failed := serveConfigAPI(slog.New(slog.DiscardHandler), server, cancel)

	// Shutdown before or after ListenAndServe starts ends it with
	// http.ErrServerClosed either way.
	require.NoError(t, server.Shutdown(t.Context()))

	time.Sleep(100 * time.Millisecond)

	assert.False(t, failed.Load())
	assert.NoError(t, ctx.Err(), "a normal shutdown must not cancel the tunnel")
}

// TestRun_StandaloneStartupFailureReturnsOne pins that a run mode's failure
// comes back to run as an exit code, so the deferred tracing shutdown in run
// still happens. A proxy listener that cannot bind at startup is the failure.
func TestRun_StandaloneStartupFailureReturnsOne(t *testing.T) {
	occupied, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	t.Cleanup(func() { _ = occupied.Close() })

	t.Setenv("TUNNEL_TOKEN", "")
	t.Setenv("PROXY_AUTH_TOKEN", "")
	require.NoError(t, os.Unsetenv("TUNNEL_TOKEN"))
	require.NoError(t, os.Unsetenv("PROXY_AUTH_TOKEN"))
	t.Setenv("PROXY_CONFIG_ADDR", "127.0.0.1:0")
	t.Setenv("PROXY_ADDR", occupied.Addr().String())

	assert.Equal(t, 1, run(slog.New(slog.DiscardHandler)))
}

// TestBuildDataPlane_ReturnsTheAuthRefusal pins that a broken config-API auth
// configuration comes back to the caller as an error instead of ending the
// process from inside the builder, so main can log it and still run its
// deferred tracing shutdown.
func TestBuildDataPlane_ReturnsTheAuthRefusal(t *testing.T) {
	// t.Setenv registers the restore; the unset then makes the variable absent.
	t.Setenv("PROXY_AUTH_TOKEN", "")
	t.Setenv(allowUnauthenticatedConfigAPIEnv, "")
	require.NoError(t, os.Unsetenv("PROXY_AUTH_TOKEN"))
	require.NoError(t, os.Unsetenv(allowUnauthenticatedConfigAPIEnv))

	_, err := buildDataPlane(slog.New(slog.DiscardHandler), authRequired)
	require.ErrorIs(t, err, errProxyAuthTokenMissing)
}

// TestBuildDataPlane_StandaloneBuildsWithoutAuth pins the other half: with no
// token, standalone mode builds its data plane rather than refusing.
func TestBuildDataPlane_StandaloneBuildsWithoutAuth(t *testing.T) {
	t.Setenv("PROXY_AUTH_TOKEN", "")
	require.NoError(t, os.Unsetenv("PROXY_AUTH_TOKEN"))

	plane, err := buildDataPlane(slog.New(slog.DiscardHandler), authOptional)
	require.NoError(t, err)
	assert.NotNil(t, plane.router)
	assert.NotNil(t, plane.handler)
	assert.NotNil(t, plane.configAPI)
}
