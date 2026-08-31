package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveAuthToken_UnsetInStandaloneMeansNoAuth pins the historical default
// where it still applies: standalone mode is a development server, so a run
// that never wires PROXY_AUTH_TOKEN keeps starting with the config API
// unauthenticated. Explicitly os.Unsetenv rather than relying on ambient
// process state -- t.Setenv can only set a value, it cannot force a variable to
// be absent. No restore-on-cleanup: PROXY_AUTH_TOKEN is application-specific
// and never ambiently set outside a test explicitly setting it, and every other
// test in this file that does set it already restores via its own
// t.Setenv-registered cleanup before the next test runs (none of these tests
// run in parallel).
func TestResolveAuthToken_UnsetInStandaloneMeansNoAuth(t *testing.T) {
	require.NoError(t, os.Unsetenv("PROXY_AUTH_TOKEN"))

	token, err := resolveAuthToken(authOptional)
	require.NoError(t, err)
	assert.Empty(t, token)
}

// TestResolveAuthToken_UnsetInTunnelModeIsFatal pins the fail-closed contract
// for the mode that faces the internet. In tunnel mode the config API binds
// :8081 on every interface and a successful PUT replaces the whole routing
// table, so starting it with no Bearer check is not a state anything should
// reach by omitting a variable.
func TestResolveAuthToken_UnsetInTunnelModeIsFatal(t *testing.T) {
	require.NoError(t, os.Unsetenv("PROXY_AUTH_TOKEN"))
	require.NoError(t, os.Unsetenv(allowUnauthenticatedConfigAPIEnv))

	_, err := resolveAuthToken(authRequired)
	require.Error(t, err, "tunnel mode must refuse to start with an unauthenticated config API")
	assert.ErrorIs(t, err, errProxyAuthTokenMissing)
}

// TestResolveAuthToken_UnsetInTunnelModeIsAllowedWhenAcknowledged pins the
// escape hatch. It is a separate variable from PROXY_AUTH_TOKEN on purpose: an
// empty token is what a broken Secret produces, so consent must not be
// spellable by the same misconfiguration.
func TestResolveAuthToken_UnsetInTunnelModeIsAllowedWhenAcknowledged(t *testing.T) {
	require.NoError(t, os.Unsetenv("PROXY_AUTH_TOKEN"))
	t.Setenv(allowUnauthenticatedConfigAPIEnv, "1")

	token, err := resolveAuthToken(authRequired)
	require.NoError(t, err)
	assert.Empty(t, token)
}

// TestResolveAuthToken_EmptyIsBrokenEvenWhenAcknowledged pins that the escape
// hatch does not cover a broken Secret. The acknowledgement says "run without
// authentication"; an empty PROXY_AUTH_TOKEN says "authentication was wired and
// resolved to nothing", which stays fatal in either mode.
func TestResolveAuthToken_EmptyIsBrokenEvenWhenAcknowledged(t *testing.T) {
	t.Setenv("PROXY_AUTH_TOKEN", "")
	t.Setenv(allowUnauthenticatedConfigAPIEnv, "1")

	for _, requireAuth := range []configAPIAuth{authRequired, authOptional} {
		_, err := resolveAuthToken(requireAuth)
		require.Error(t, err)
		assert.ErrorIs(t, err, errProxyAuthTokenEmpty)
	}
}

// TestResolveAuthToken_EmptyMeansBrokenConfig pins the behavior the chart
// depends on: it wires PROXY_AUTH_TOKEN unconditionally, so the variable's mere
// presence signals that authentication was intended. A present-but-empty value
// (the auth Secret's key resolved to an empty string) is therefore a broken
// configuration, not a choice to run open, and must fail rather than silently
// disable the one check this whole feature exists to enforce.
func TestResolveAuthToken_EmptyMeansBrokenConfig(t *testing.T) {
	t.Setenv("PROXY_AUTH_TOKEN", "")

	token, err := resolveAuthToken(authRequired)
	require.Error(t, err)
	assert.ErrorIs(t, err, errProxyAuthTokenEmpty)
	assert.Empty(t, token)
}

// TestResolveAuthToken_NonEmptyReturnsValue is the plain happy path: a real
// token passes through unchanged.
func TestResolveAuthToken_NonEmptyReturnsValue(t *testing.T) {
	t.Setenv("PROXY_AUTH_TOKEN", "real-token-value")

	token, err := resolveAuthToken(authRequired)
	require.NoError(t, err)
	assert.Equal(t, "real-token-value", token)
}

// TestResolveTunnelToken_EmptyIsBrokenConfig pins the same rule TUNNEL_TOKEN
// deserves as PROXY_AUTH_TOKEN: present-but-empty is a broken configuration.
//
// The run mode is chosen on the token being non-empty, so an empty one does not
// merely fail — it silently selects standalone, which is the mode that still
// starts without a config-API token. A tunnel deployment whose token Secret
// resolved to nothing would come up as an unauthenticated local proxy instead
// of refusing.
func TestResolveTunnelToken_EmptyIsBrokenConfig(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "")

	_, err := resolveTunnelToken()
	require.Error(t, err, "an empty tunnel token must not silently select standalone mode")
	assert.ErrorIs(t, err, errTunnelTokenEmpty)
}

// TestResolveTunnelToken_NonEmptySelectsTunnelMode pins the branch that picks
// the mode this change hardened.
func TestResolveTunnelToken_NonEmptySelectsTunnelMode(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "a-real-connector-token")

	token, err := resolveTunnelToken()
	require.NoError(t, err)
	assert.Equal(t, "a-real-connector-token", token)
}

// TestResolveTunnelToken_UnsetSelectsStandalone pins the deliberate half: never
// wiring the variable is how standalone mode is requested.
func TestResolveTunnelToken_UnsetSelectsStandalone(t *testing.T) {
	require.NoError(t, os.Unsetenv("TUNNEL_TOKEN"))

	token, err := resolveTunnelToken()
	require.NoError(t, err)
	assert.Empty(t, token)
}
