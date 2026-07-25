package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveAuthToken_UnsetMeansNoAuth pins the historical default: a raw
// manifest or local-dev run that never wires PROXY_AUTH_TOKEN at all must
// keep starting with the config API unauthenticated, exactly as before this
// change. Explicitly os.Unsetenv rather than relying on ambient process
// state -- t.Setenv can only set a value, it cannot force a variable to be
// absent. No restore-on-cleanup: PROXY_AUTH_TOKEN is application-specific and
// never ambiently set outside a test explicitly setting it, and every other
// test in this file that does set it already restores via its own
// t.Setenv-registered cleanup before the next test runs (none of these tests
// run in parallel).
func TestResolveAuthToken_UnsetMeansNoAuth(t *testing.T) {
	require.NoError(t, os.Unsetenv("PROXY_AUTH_TOKEN"))

	token, err := resolveAuthToken()
	require.NoError(t, err)
	assert.Empty(t, token)
}

// TestResolveAuthToken_EmptyMeansBrokenConfig pins the new behavior: the
// chart now wires PROXY_AUTH_TOKEN unconditionally, so the variable's mere
// presence signals that authentication was intended. A present-but-empty
// value (the auth Secret's key resolved to an empty string) is therefore a
// broken configuration, not a choice to run open, and must fail rather than
// silently disable the one check this whole feature exists to enforce.
func TestResolveAuthToken_EmptyMeansBrokenConfig(t *testing.T) {
	t.Setenv("PROXY_AUTH_TOKEN", "")

	token, err := resolveAuthToken()
	require.Error(t, err)
	assert.ErrorIs(t, err, errProxyAuthTokenEmpty)
	assert.Empty(t, token)
}

// TestResolveAuthToken_NonEmptyReturnsValue is the plain happy path: a real
// token passes through unchanged.
func TestResolveAuthToken_NonEmptyReturnsValue(t *testing.T) {
	t.Setenv("PROXY_AUTH_TOKEN", "real-token-value")

	token, err := resolveAuthToken()
	require.NoError(t, err)
	assert.Equal(t, "real-token-value", token)
}
