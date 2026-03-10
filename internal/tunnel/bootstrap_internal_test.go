package tunnel

import (
	"context"
	"crypto/tls"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudflare/cloudflared/connection"
)

func TestBuildCatchAllIngress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		proxyURL string
		wantErr  bool
	}{
		{
			name:     "valid HTTP URL",
			proxyURL: "http://localhost:8080",
			wantErr:  false,
		},
		{
			name:     "valid HTTPS URL",
			proxyURL: "https://backend.svc.cluster.local:443",
			wantErr:  false,
		},
		{
			name:     "empty URL produces error",
			proxyURL: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := buildCatchAllIngress(tt.proxyURL)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.NotEmpty(t, result.Rules, "ingress rules should not be empty")
		})
	}
}

func TestBuildRootCAPool(t *testing.T) {
	t.Parallel()

	pool, err := buildRootCAPool()

	require.NoError(t, err)
	assert.NotNil(t, pool, "root CA pool should not be nil")
}

func TestNewZerologLogger(t *testing.T) {
	t.Parallel()

	logger := newZerologLogger()

	assert.NotNil(t, logger, "zerolog logger should not be nil")
}

func TestBuildCatchAllIngress_PlaceholderURL(t *testing.T) {
	t.Parallel()

	// In in-process mode, buildOrchestrator uses a placeholder URL.
	// Verify it produces valid ingress rules.
	result, err := buildCatchAllIngress("http://localhost:0")

	require.NoError(t, err)
	assert.NotEmpty(t, result.Rules, "placeholder URL should produce valid ingress rules")
}

func TestBuildCatchAllIngress_VariousSchemes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		proxyURL string
		wantErr  bool
	}{
		{
			name:     "unix socket",
			proxyURL: "unix:/var/run/app.sock",
			wantErr:  false,
		},
		{
			name:     "hello world service",
			proxyURL: "hello_world",
			wantErr:  false,
		},
		{
			name:     "http status service",
			proxyURL: "http_status:404",
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := buildCatchAllIngress(tt.proxyURL)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.NotEmpty(t, result.Rules)
		})
	}
}

func TestBuildEdgeTLSConfigs_ReturnsAllProtocols(t *testing.T) {
	t.Parallel()

	configs, err := buildEdgeTLSConfigs()

	require.NoError(t, err)
	require.NotNil(t, configs)

	// Every protocol in ProtocolList must have a TLS config.
	for _, proto := range connection.ProtocolList {
		tlsCfg, ok := configs[proto]
		require.True(t, ok, "missing TLS config for protocol %s", proto)
		assert.NotNil(t, tlsCfg, "TLS config for %s should not be nil", proto)
	}
}

func TestBuildEdgeTLSConfigs_MinTLSVersion(t *testing.T) {
	t.Parallel()

	configs, err := buildEdgeTLSConfigs()

	require.NoError(t, err)

	for _, proto := range connection.ProtocolList {
		tlsCfg := configs[proto]
		assert.Equal(t, uint16(tls.VersionTLS12), tlsCfg.MinVersion,
			"protocol %s should enforce TLS 1.2 minimum", proto)
	}
}

func TestBuildEdgeTLSConfigs_HasRootCAs(t *testing.T) {
	t.Parallel()

	configs, err := buildEdgeTLSConfigs()

	require.NoError(t, err)

	for _, proto := range connection.ProtocolList {
		tlsCfg := configs[proto]
		assert.NotNil(t, tlsCfg.RootCAs,
			"protocol %s should have root CAs configured", proto)
	}
}

func TestBuildEdgeTLSConfigs_HasServerName(t *testing.T) {
	t.Parallel()

	configs, err := buildEdgeTLSConfigs()

	require.NoError(t, err)

	for _, proto := range connection.ProtocolList {
		tlsCfg := configs[proto]
		assert.NotEmpty(t, tlsCfg.ServerName,
			"protocol %s should have a server name", proto)
	}
}

func TestBuildEdgeTLSConfigs_MapSize(t *testing.T) {
	t.Parallel()

	configs, err := buildEdgeTLSConfigs()

	require.NoError(t, err)
	assert.Len(t, configs, len(connection.ProtocolList),
		"config map should have one entry per protocol")
}

func newTestToken() *Token {
	return &Token{
		AccountTag:   "test-account",
		TunnelSecret: []byte("test-secret"),
		TunnelID:     uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
	}
}

func TestBuildProtocolAndClient_Success(t *testing.T) {
	t.Parallel()

	token := newTestToken()
	zlog := newZerologLogger()

	selector, tunnelCfg, err := buildProtocolAndClient(t.Context(), token, &zlog)

	require.NoError(t, err)
	assert.NotNil(t, selector)
	assert.NotNil(t, tunnelCfg)
	assert.Equal(t, defaultHAConnections, tunnelCfg.HAConnections)
	assert.Equal(t, defaultGracePeriod, tunnelCfg.GracePeriod)
	assert.Equal(t, uint(defaultRetries), tunnelCfg.Retries)
	assert.Equal(t, defaultRPCTimeout, tunnelCfg.RPCTimeout)
	assert.Equal(t, defaultWriteStreamTimeout, tunnelCfg.WriteStreamTimeout)
	assert.Equal(t, proxyVersion, tunnelCfg.ReportedVersion)
}

func TestBuildProtocolAndClient_TokenEndpointPassedThrough(t *testing.T) {
	t.Parallel()

	token := newTestToken()
	token.Endpoint = "us"
	zlog := newZerologLogger()

	_, tunnelCfg, err := buildProtocolAndClient(t.Context(), token, &zlog)

	require.NoError(t, err)
	assert.Equal(t, "us", tunnelCfg.Region)
}

func TestBuildProtocolAndClient_QUICFlowControl(t *testing.T) {
	t.Parallel()

	token := newTestToken()
	zlog := newZerologLogger()

	_, tunnelCfg, err := buildProtocolAndClient(t.Context(), token, &zlog)

	require.NoError(t, err)
	assert.Equal(t, uint64(defaultQUICFlowControlConn), tunnelCfg.QUICConnectionLevelFlowControlLimit)
	assert.Equal(t, uint64(defaultQUICFlowControlStr), tunnelCfg.QUICStreamLevelFlowControlLimit)
}

func TestBuildProtocolAndClient_CloseConnOnce(t *testing.T) {
	t.Parallel()

	token := newTestToken()
	zlog := newZerologLogger()

	_, tunnelCfg, err := buildProtocolAndClient(t.Context(), token, &zlog)

	require.NoError(t, err)
	assert.NotNil(t, tunnelCfg.CloseConnOnce, "CloseConnOnce must be initialized")
}

// TestBuildTunnelConfig_InvalidProxyURL tests the error path which fails before
// Prometheus metrics registration, so it is safe to run independently.
func TestBuildTunnelConfig_InvalidProxyURL(t *testing.T) {
	t.Parallel()

	token := newTestToken()
	zlog := newZerologLogger()

	_, _, err := buildTunnelConfig(t.Context(), token, "", &zlog)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ingress")
}

// TestBuildOrchestrator_InvalidProxyURL tests the error path which fails before
// Prometheus metrics registration.
func TestBuildOrchestrator_InvalidProxyURL(t *testing.T) {
	t.Parallel()

	token := newTestToken()
	cfg := &Config{
		ProxyURL: "",
	}

	_, _, err := buildOrchestrator(t.Context(), cfg, token, slog.Default())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "build tunnel config")
}

// TestStartTunnel_InvalidToken_ErrorPath verifies StartTunnel returns an error
// for invalid tokens without reaching buildTunnelConfig (no Prometheus conflict).
func TestStartTunnel_InvalidToken_ErrorPath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := StartTunnel(ctx, Config{
		Token:    "not-valid-base64!!!",
		ProxyURL: "http://localhost:8080",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse tunnel token")
}

// TestStartTunnel_EmptyToken_ErrorPath verifies StartTunnel returns an error
// for empty tokens without reaching buildTunnelConfig.
func TestStartTunnel_EmptyToken_ErrorPath(t *testing.T) {
	t.Parallel()

	err := StartTunnel(t.Context(), Config{
		Token:    "",
		ProxyURL: "http://localhost:8080",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "tunnel token is empty")
}

func TestConstants(t *testing.T) {
	t.Parallel()

	// Verify constants have expected values to catch accidental changes.
	assert.Equal(t, 4, defaultHAConnections)
	assert.Equal(t, 5, defaultRetries)
	assert.Equal(t, 8, defaultMaxEdgeAddrRetries)
	assert.Equal(t, "cloudflare-tunnel-gateway-proxy", proxyVersion)
}
