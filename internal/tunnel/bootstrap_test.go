package tunnel_test

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
)

func TestParseTunnelToken_Valid(t *testing.T) {
	t.Parallel()

	tokenJSON := `{"a":"abc123","s":"c2VjcmV0","t":"550e8400-e29b-41d4-a716-446655440000"}`
	encoded := base64.StdEncoding.EncodeToString([]byte(tokenJSON))

	token, err := tunnel.ParseTunnelToken(encoded)

	require.NoError(t, err)
	assert.Equal(t, "abc123", token.AccountTag)
	assert.Equal(t, "550e8400-e29b-41d4-a716-446655440000", token.TunnelID.String())
	assert.NotEmpty(t, token.TunnelSecret)
}

func TestParseTunnelToken_WithEndpoint(t *testing.T) {
	t.Parallel()

	tokenJSON := `{"a":"abc123","s":"c2VjcmV0","t":"550e8400-e29b-41d4-a716-446655440000","e":"us"}`
	encoded := base64.StdEncoding.EncodeToString([]byte(tokenJSON))

	token, err := tunnel.ParseTunnelToken(encoded)

	require.NoError(t, err)
	assert.Equal(t, "us", token.Endpoint)
}

func TestParseTunnelToken_EmptyString(t *testing.T) {
	t.Parallel()

	_, err := tunnel.ParseTunnelToken("")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "tunnel token is empty")
}

func TestParseTunnelToken_InvalidBase64(t *testing.T) {
	t.Parallel()

	_, err := tunnel.ParseTunnelToken("not-valid-base64!!!")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid base64")
}

func TestParseTunnelToken_InvalidJSON(t *testing.T) {
	t.Parallel()

	encoded := base64.StdEncoding.EncodeToString([]byte("not json"))

	_, err := tunnel.ParseTunnelToken(encoded)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid JSON")
}

func TestParseTunnelToken_MissingAccountTag(t *testing.T) {
	t.Parallel()

	tokenJSON := `{"s":"c2VjcmV0","t":"550e8400-e29b-41d4-a716-446655440000"}`
	encoded := base64.StdEncoding.EncodeToString([]byte(tokenJSON))

	_, err := tunnel.ParseTunnelToken(encoded)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing account tag")
}

func TestParseTunnelToken_MissingTunnelID(t *testing.T) {
	t.Parallel()

	tokenJSON := `{"a":"abc123","s":"c2VjcmV0"}`
	encoded := base64.StdEncoding.EncodeToString([]byte(tokenJSON))

	_, err := tunnel.ParseTunnelToken(encoded)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing tunnel ID")
}

func TestParseTunnelToken_MissingSecret(t *testing.T) {
	t.Parallel()

	tokenJSON := `{"a":"abc123","t":"550e8400-e29b-41d4-a716-446655440000"}`
	encoded := base64.StdEncoding.EncodeToString([]byte(tokenJSON))

	_, err := tunnel.ParseTunnelToken(encoded)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing tunnel secret")
}

func TestStartTunnel_InvalidToken(t *testing.T) {
	t.Parallel()

	err := tunnel.StartTunnel(t.Context(), tunnel.Config{
		Token: "invalid",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse tunnel token")
}
