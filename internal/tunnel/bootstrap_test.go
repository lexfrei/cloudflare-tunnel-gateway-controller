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

// TestParseTunnelToken_DoesNotLeakSecret pins a security boundary: a malformed
// token must produce an error that never echoes decoded token bytes. Parse
// errors are logged AND surfaced on the tenant-readable Gateway status, so a
// leak here exposes tunnel credentials to anyone with Gateway read RBAC. The
// stdlib base64/json errors embed the offending input bytes, so the wrapped
// error must carry only the static sentinel, never the stdlib detail.
func TestParseTunnelToken_DoesNotLeakSecret(t *testing.T) {
	t.Parallel()

	const secretSentinel = "SUPERSECRETtunnelVALUE"

	t.Run("base64-invalid secret field", func(t *testing.T) {
		t.Parallel()

		// TunnelSecret is a []byte field, so json.Unmarshal base64-decodes the
		// "s" string; an invalid value makes the unmarshal fail with a stdlib
		// base64 detail ("illegal base64 data at input byte N"). The wrapped
		// error must collapse to the static JSON sentinel, never carry that
		// detail — asserting only NotContains(secret) is vacuous here because
		// the base64 error reports an offset, not the bytes.
		tokenJSON := `{"a":"abc123","t":"550e8400-e29b-41d4-a716-446655440000","s":"` + secretSentinel + `_!!!"}`
		_, err := tunnel.ParseTunnelToken(base64.StdEncoding.EncodeToString([]byte(tokenJSON)))

		require.Error(t, err)
		assert.Contains(t, err.Error(), "not valid JSON", "must collapse to the static sentinel")
		assert.NotContains(t, err.Error(), secretSentinel)
		assert.NotContains(t, err.Error(), "illegal base64",
			"the stdlib base64 detail from the json []byte decode must not propagate")
	})

	t.Run("decoded bytes are not JSON", func(t *testing.T) {
		t.Parallel()

		// Valid base64 whose decoded bytes ARE the secret, not JSON (a corrupted
		// or truncated real connector token). json.Unmarshal echoes the leading
		// decoded byte(s) — the parse error must not propagate them.
		_, err := tunnel.ParseTunnelToken(base64.StdEncoding.EncodeToString([]byte(secretSentinel + " and more")))

		require.Error(t, err)
		assert.Contains(t, err.Error(), "not valid JSON")
		assert.NotContains(t, err.Error(), "invalid character",
			"the stdlib json error embeds decoded token bytes and must not propagate")
	})

	t.Run("invalid base64", func(t *testing.T) {
		t.Parallel()

		_, err := tunnel.ParseTunnelToken("not-valid-base64-but-secret!!!")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "not valid base64")
		assert.NotContains(t, err.Error(), "illegal base64",
			"the stdlib base64 error detail must not propagate")
	})
}

func TestStartTunnel_InvalidToken(t *testing.T) {
	t.Parallel()

	err := tunnel.StartTunnel(t.Context(), &tunnel.Config{
		Token: "invalid",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse tunnel token")
}
