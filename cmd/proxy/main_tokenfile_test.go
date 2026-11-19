package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTokenFile writes a token file for the TUNNEL_TOKEN_FILE tests and
// returns its path. A real Secret mount delivers the token as a file in a
// read-only volume; a temp file is the same contract.
func writeTokenFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "tunnel-token")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

// TestResolveTunnelToken_FileSelectsTunnelMode pins the file-based delivery
// path: TUNNEL_TOKEN_FILE names a mounted Secret file and the token never
// sits in the process environment, mirroring upstream cloudflared's
// --token-file.
func TestResolveTunnelToken_FileSelectsTunnelMode(t *testing.T) {
	require.NoError(t, os.Unsetenv("TUNNEL_TOKEN"))
	t.Setenv("TUNNEL_TOKEN_FILE", writeTokenFile(t, "a-real-connector-token"))

	token, err := resolveTunnelToken()
	require.NoError(t, err)
	assert.Equal(t, "a-real-connector-token", token)
}

// TestResolveTunnelToken_FileTrimsTrailingNewline pins tolerance for the way
// tokens actually land in Secret files: `kubectl create secret --from-file`
// and heredocs routinely append a newline, which is not part of the token.
func TestResolveTunnelToken_FileTrimsTrailingNewline(t *testing.T) {
	require.NoError(t, os.Unsetenv("TUNNEL_TOKEN"))
	t.Setenv("TUNNEL_TOKEN_FILE", writeTokenFile(t, "a-real-connector-token\n"))

	token, err := resolveTunnelToken()
	require.NoError(t, err)
	assert.Equal(t, "a-real-connector-token", token)
}

// TestResolveTunnelToken_FileAndEnvIsAmbiguous pins the refusal when both
// sources are wired: silently preferring one would let a stale env token keep
// serving after the operator switched to (or rotated) the file, or vice
// versa. Two configured sources is a broken deployment, not a tie to break.
func TestResolveTunnelToken_FileAndEnvIsAmbiguous(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "env-token")
	t.Setenv("TUNNEL_TOKEN_FILE", writeTokenFile(t, "file-token"))

	_, err := resolveTunnelToken()
	require.Error(t, err, "both TUNNEL_TOKEN and TUNNEL_TOKEN_FILE set must be refused")
	assert.ErrorIs(t, err, errTunnelTokenAmbiguous)
}

// TestResolveTunnelToken_FileSetButEmptyIsBrokenConfig pins the same rule an
// empty TUNNEL_TOKEN gets: present-but-empty selects nothing silently. An
// empty path would otherwise fall through to standalone mode, the mode that
// still starts without a config-API token.
func TestResolveTunnelToken_FileSetButEmptyIsBrokenConfig(t *testing.T) {
	require.NoError(t, os.Unsetenv("TUNNEL_TOKEN"))
	t.Setenv("TUNNEL_TOKEN_FILE", "")

	_, err := resolveTunnelToken()
	require.Error(t, err, "an empty TUNNEL_TOKEN_FILE must not silently select standalone mode")
	assert.ErrorIs(t, err, errTunnelTokenFileEmpty)
}

// TestResolveTunnelToken_FileMissingIsBrokenConfig pins the unreadable-file
// case: a mount that did not land (wrong Secret name, missing key) must fail
// the start, not fall back to an unauthenticated standalone proxy.
func TestResolveTunnelToken_FileMissingIsBrokenConfig(t *testing.T) {
	require.NoError(t, os.Unsetenv("TUNNEL_TOKEN"))
	t.Setenv("TUNNEL_TOKEN_FILE", filepath.Join(t.TempDir(), "does-not-exist"))

	_, err := resolveTunnelToken()
	require.Error(t, err, "an unreadable token file must fail the start")
}

// TestResolveTunnelToken_FileWithEmptyContentIsBrokenConfig pins the
// file-delivery twin of errTunnelTokenEmpty: a mounted file holding nothing
// (an empty Secret key) is a broken configuration, not a request for
// standalone mode.
func TestResolveTunnelToken_FileWithEmptyContentIsBrokenConfig(t *testing.T) {
	require.NoError(t, os.Unsetenv("TUNNEL_TOKEN"))
	t.Setenv("TUNNEL_TOKEN_FILE", writeTokenFile(t, "\n"))

	_, err := resolveTunnelToken()
	require.Error(t, err, "an empty token file must not silently select standalone mode")
	assert.ErrorIs(t, err, errTunnelTokenEmpty)
}
