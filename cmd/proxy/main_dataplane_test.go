package main

import (
	"bytes"
	"log/slog"
	"os"
	"testing"

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
	t.Setenv("PROXY_AUTH_TOKEN", "")
	t.Setenv(allowUnauthenticatedConfigAPIEnv, "")
	require.NoError(t, os.Unsetenv("PROXY_AUTH_TOKEN"))
	require.NoError(t, os.Unsetenv(allowUnauthenticatedConfigAPIEnv))

	var logs bytes.Buffer

	assert.Equal(t, 1, run(slog.New(slog.NewTextHandler(&logs, nil))))
	assert.Contains(t, logs.String(), "refusing to start with a broken config-API auth configuration")
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
