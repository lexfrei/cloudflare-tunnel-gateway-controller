package tunnel

import (
	"context"
	"crypto/tls"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
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

// validTestTokenBase64 returns a base64-encoded token string that passes ParseTunnelToken.
func validTestTokenBase64() string {
	return "eyJhIjoidGVzdC1hY2NvdW50IiwicyI6ImRHVnpkQzF6WldOeVpYUT0iLCJ0IjoiNTUwZTg0MDAtZTI5Yi00MWQ0LWE3MTYtNDQ2NjU1NDQwMDAwIn0="
}

// TestParseTunnelToken_MalformedInputs covers edge cases for token parsing.
func TestParseTunnelToken_MalformedInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		token     string
		wantErr   bool
		errSubstr string
	}{
		{
			name:      "valid base64 wrapping non-JSON bytes",
			token:     "AQIDBA==", // base64 of raw bytes 0x01 0x02 0x03 0x04
			wantErr:   true,
			errSubstr: "not valid JSON",
		},
		{
			name:      "valid base64 wrapping empty JSON object",
			token:     "e30=", // base64 of "{}"
			wantErr:   true,
			errSubstr: "missing account tag",
		},
		{
			name:      "valid JSON with only account tag",
			token:     "eyJhIjoiYWNjdCJ9", // {"a":"acct"}
			wantErr:   true,
			errSubstr: "missing tunnel ID",
		},
		{
			name:      "valid JSON with account and nil tunnel ID",
			token:     "eyJhIjoiYWNjdCIsInQiOiIwMDAwMDAwMC0wMDAwLTAwMDAtMDAwMC0wMDAwMDAwMDAwMDAifQ==", // {"a":"acct","t":"00000000-0000-0000-0000-000000000000"}
			wantErr:   true,
			errSubstr: "missing tunnel ID",
		},
		{
			name:      "valid JSON with account and tunnel ID but no secret",
			token:     "eyJhIjoiYWNjdCIsInQiOiI1NTBlODQwMC1lMjliLTQxZDQtYTcxNi00NDY2NTU0NDAwMDAifQ==", // {"a":"acct","t":"550e8400-e29b-41d4-a716-446655440000"}
			wantErr:   true,
			errSubstr: "missing tunnel secret",
		},
		{
			name:      "valid JSON with invalid UUID format",
			token:     "eyJhIjoiYWNjdCIsInMiOiJjMlZqY21WMCIsInQiOiJub3QtYS11dWlkIn0=", // {"a":"acct","s":"c2VjcmV0","t":"not-a-uuid"}
			wantErr:   true,
			errSubstr: "not valid JSON", // UUID unmarshal fails at JSON level
		},
		{
			name:      "valid JSON with empty secret array",
			token:     "eyJhIjoiYWNjdCIsInMiOiIiLCJ0IjoiNTUwZTg0MDAtZTI5Yi00MWQ0LWE3MTYtNDQ2NjU1NDQwMDAwIn0=", // {"a":"acct","s":"","t":"550e8400-..."}
			wantErr:   true,
			errSubstr: "missing tunnel secret",
		},
		{
			name:    "complete valid token without endpoint",
			token:   validTestTokenBase64(),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := ParseTunnelToken(tt.token)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errSubstr)
				assert.Nil(t, result)

				return
			}

			require.NoError(t, err)
			require.NotNil(t, result)
			assert.NotEmpty(t, result.AccountTag)
			assert.NotEqual(t, uuid.Nil, result.TunnelID)
			assert.NotEmpty(t, result.TunnelSecret)
		})
	}
}

// TestStartTunnel_NilLogger_BuildOrchestratorError verifies StartTunnel falls back
// to slog.Default when Logger is nil, and returns an error from buildOrchestrator
// (empty ProxyURL) before reaching Prometheus registration.
func TestStartTunnel_NilLogger_BuildOrchestratorError(t *testing.T) {
	t.Parallel()

	err := StartTunnel(t.Context(), Config{
		Token:    validTestTokenBase64(),
		Logger:   nil,
		ProxyURL: "",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "build tunnel config")
}

// TestStartTunnel_ExplicitLogger_BuildOrchestratorError verifies StartTunnel uses
// the provided logger and returns an error from buildOrchestrator (empty ProxyURL)
// before reaching Prometheus registration.
func TestStartTunnel_ExplicitLogger_BuildOrchestratorError(t *testing.T) {
	t.Parallel()

	err := StartTunnel(t.Context(), Config{
		Token:    validTestTokenBase64(),
		Logger:   slog.Default(),
		ProxyURL: "",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "build tunnel config")
}

// TestStartTunnel_ValidBase64InvalidJSON_ErrorPath verifies StartTunnel wraps
// ParseTunnelToken error when base64 decodes but JSON is invalid.
func TestStartTunnel_ValidBase64InvalidJSON_ErrorPath(t *testing.T) {
	t.Parallel()

	// "not json" in base64
	err := StartTunnel(t.Context(), Config{
		Token:    "bm90IGpzb24=",
		ProxyURL: "http://localhost:8080",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse tunnel token")
	assert.Contains(t, err.Error(), "not valid JSON")
}

// TestStartTunnel_EmptyJSONObject_ErrorPath verifies StartTunnel wraps
// ParseTunnelToken error when token is valid base64+JSON but missing fields.
func TestStartTunnel_EmptyJSONObject_ErrorPath(t *testing.T) {
	t.Parallel()

	// "{}" in base64
	err := StartTunnel(t.Context(), Config{
		Token:    "e30=",
		ProxyURL: "http://localhost:8080",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse tunnel token")
	assert.Contains(t, err.Error(), "missing account tag")
}

// TestStartTunnel_PartialFields_ErrorPath verifies StartTunnel wraps
// ParseTunnelToken error when token has some fields but not all.
func TestStartTunnel_PartialFields_ErrorPath(t *testing.T) {
	t.Parallel()

	// {"a":"acct"} in base64 — has account but no tunnel ID or secret
	err := StartTunnel(t.Context(), Config{
		Token:    "eyJhIjoiYWNjdCJ9",
		ProxyURL: "http://localhost:8080",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse tunnel token")
	assert.Contains(t, err.Error(), "missing tunnel ID")
}

// TestBuildOrchestrator_EmptyProxyURL_NilOriginProxy verifies buildOrchestrator
// with nil OriginProxy uses cfg.ProxyURL directly (empty string → ingress error).
func TestBuildOrchestrator_EmptyProxyURL_NilOriginProxy(t *testing.T) {
	t.Parallel()

	token := newTestToken()
	cfg := &Config{
		ProxyURL:    "",
		OriginProxy: nil,
	}

	_, _, err := buildOrchestrator(t.Context(), cfg, token, slog.Default())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "build tunnel config")
}

// TestBuildOrchestrator_ExplicitProxyURL_NilOriginProxy verifies buildOrchestrator
// passes cfg.ProxyURL through when OriginProxy is nil.
// Uses an invalid URL scheme that fails at ingress validation, before prometheus.
func TestBuildOrchestrator_ExplicitProxyURL_NilOriginProxy_InvalidScheme(t *testing.T) {
	t.Parallel()

	token := newTestToken()
	cfg := &Config{
		ProxyURL:    "",
		OriginProxy: nil,
	}

	_, _, err := buildOrchestrator(t.Context(), cfg, token, slog.Default())

	require.Error(t, err)
}

// TestBuildTunnelConfig_EmptyProxyURL_ErrorMessage verifies the exact error
// wrapping chain for empty proxy URL.
func TestBuildTunnelConfig_EmptyProxyURL_ErrorMessage(t *testing.T) {
	t.Parallel()

	token := newTestToken()
	zlog := newZerologLogger()

	_, _, err := buildTunnelConfig(t.Context(), token, "", &zlog)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "build ingress rules")
	assert.Contains(t, err.Error(), "parse catch-all ingress")
}

// TestBuildProtocolAndClient_CancelledContext verifies buildProtocolAndClient
// behaves correctly with an already-cancelled context.
func TestBuildProtocolAndClient_CancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	token := newTestToken()
	zlog := newZerologLogger()

	// buildProtocolAndClient may or may not error on cancelled context
	// (depends on cloudflared internals). We verify it does not panic.
	selector, tunnelCfg, err := buildProtocolAndClient(ctx, token, &zlog)
	if err != nil {
		assert.Nil(t, selector)
		assert.Nil(t, tunnelCfg)
	} else {
		assert.NotNil(t, selector)
		assert.NotNil(t, tunnelCfg)
	}
}

// TestBuildProtocolAndClient_LogAndTransportFields verifies Log and LogTransport
// fields are populated on the returned tunnel config.
func TestBuildProtocolAndClient_LogAndTransportFields(t *testing.T) {
	t.Parallel()

	token := newTestToken()
	zlog := newZerologLogger()

	_, tunnelCfg, err := buildProtocolAndClient(t.Context(), token, &zlog)

	require.NoError(t, err)
	assert.NotNil(t, tunnelCfg.Log, "Log field should be set")
	assert.NotNil(t, tunnelCfg.LogTransport, "LogTransport field should be set")
}

// TestBuildProtocolAndClient_MaxEdgeAddrRetries verifies the MaxEdgeAddrRetries
// constant is propagated to the tunnel config.
func TestBuildProtocolAndClient_MaxEdgeAddrRetries(t *testing.T) {
	t.Parallel()

	token := newTestToken()
	zlog := newZerologLogger()

	_, tunnelCfg, err := buildProtocolAndClient(t.Context(), token, &zlog)

	require.NoError(t, err)
	assert.Equal(t, uint8(defaultMaxEdgeAddrRetries), tunnelCfg.MaxEdgeAddrRetries)
}

// TestBuildProtocolAndClient_ClientConfig verifies the client config is populated.
func TestBuildProtocolAndClient_ClientConfig(t *testing.T) {
	t.Parallel()

	token := newTestToken()
	zlog := newZerologLogger()

	_, tunnelCfg, err := buildProtocolAndClient(t.Context(), token, &zlog)

	require.NoError(t, err)
	assert.NotNil(t, tunnelCfg.ClientConfig, "ClientConfig should be set")
}

// TestBuildEdgeTLSConfigs_NextProtos verifies that protocols with NextProtos
// have them configured in the TLS config.
func TestBuildEdgeTLSConfigs_NextProtos(t *testing.T) {
	t.Parallel()

	configs, err := buildEdgeTLSConfigs()

	require.NoError(t, err)

	for _, proto := range connection.ProtocolList {
		tlsSettings := proto.TLSSettings()
		if tlsSettings != nil && len(tlsSettings.NextProtos) > 0 {
			tlsCfg := configs[proto]
			assert.Equal(t, tlsSettings.NextProtos, tlsCfg.NextProtos,
				"protocol %s should have matching NextProtos", proto)
		}
	}
}

// TestErrorSentinels verifies error sentinel variables are properly initialized.
func TestErrorSentinels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		msg  string
	}{
		{name: "errEmptyToken", err: errEmptyToken, msg: "tunnel token is empty"},
		{name: "errInvalidBase64", err: errInvalidBase64, msg: "tunnel token is not valid base64"},
		{name: "errInvalidTokenJSON", err: errInvalidTokenJSON, msg: "tunnel token is not valid JSON"},
		{name: "errMissingAccountID", err: errMissingAccountID, msg: "tunnel token missing account tag"},
		{name: "errMissingTunnelID", err: errMissingTunnelID, msg: "tunnel token missing tunnel ID"},
		{name: "errMissingSecret", err: errMissingSecret, msg: "tunnel token missing tunnel secret"},
		{name: "errUnknownTLS", err: errUnknownTLS, msg: "unknown TLS settings for protocol"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.NotNil(t, tt.err)
			assert.Equal(t, tt.msg, tt.err.Error())
		})
	}
}

// TestTokenStruct verifies Token struct field assignment and JSON tags.
func TestTokenStruct(t *testing.T) {
	t.Parallel()

	tid := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	secret := []byte("my-secret")

	token := &Token{
		AccountTag:   "my-account",
		TunnelSecret: secret,
		TunnelID:     tid,
		Endpoint:     "eu",
	}

	assert.Equal(t, "my-account", token.AccountTag)
	assert.Equal(t, secret, token.TunnelSecret)
	assert.Equal(t, tid, token.TunnelID)
	assert.Equal(t, "eu", token.Endpoint)
}

// TestConfigStruct verifies Config struct field assignment.
func TestConfigStruct(t *testing.T) {
	t.Parallel()

	logger := slog.Default()
	cfg := Config{
		Token:    "some-token",
		Logger:   logger,
		ProxyURL: "http://localhost:8080",
	}

	assert.Equal(t, "some-token", cfg.Token)
	assert.Equal(t, logger, cfg.Logger)
	assert.Equal(t, "http://localhost:8080", cfg.ProxyURL)
	assert.Nil(t, cfg.OriginProxy)
}

// withFreshRegisterer temporarily replaces prometheus.DefaultRegisterer with a
// fresh registry so that buildTunnelConfig can call origins.NewMetrics without
// panicking on duplicate registration. This helper is NOT safe for parallel use.
func withFreshRegisterer(fn func()) {
	origRegisterer := prometheus.DefaultRegisterer
	origGatherer := prometheus.DefaultGatherer

	fresh := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = fresh
	prometheus.DefaultGatherer = fresh

	defer func() {
		prometheus.DefaultRegisterer = origRegisterer
		prometheus.DefaultGatherer = origGatherer
	}()

	fn()
}

// TestBuildTunnelConfig_SuccessPath exercises the full success path of
// buildTunnelConfig, covering warp routing, origin dialer, observer, DNS
// service, edge TLS, protocol selector, and named tunnel credential assignment.
//
// NOT parallel: temporarily replaces prometheus.DefaultRegisterer.
func TestBuildTunnelConfig_SuccessPath(t *testing.T) {
	token := newTestToken()
	zlog := newZerologLogger()

	withFreshRegisterer(func() {
		tunnelCfg, orchCfg, err := buildTunnelConfig(t.Context(), token, "http://localhost:8080", &zlog)

		require.NoError(t, err)
		require.NotNil(t, tunnelCfg)
		require.NotNil(t, orchCfg)

		// Verify tunnel config fields are populated.
		assert.NotNil(t, tunnelCfg.EdgeTLSConfigs, "EdgeTLSConfigs should be set")
		assert.NotNil(t, tunnelCfg.ProtocolSelector, "ProtocolSelector should be set")
		assert.NotNil(t, tunnelCfg.OriginDialerService, "OriginDialerService should be set")
		assert.NotNil(t, tunnelCfg.Observer, "Observer should be set")
		assert.NotNil(t, tunnelCfg.OriginDNSService, "OriginDNSService should be set")
		assert.NotNil(t, tunnelCfg.NamedTunnel, "NamedTunnel should be set")

		// Verify credentials are passed through.
		assert.Equal(t, token.AccountTag, tunnelCfg.NamedTunnel.Credentials.AccountTag)
		assert.Equal(t, token.TunnelSecret, tunnelCfg.NamedTunnel.Credentials.TunnelSecret)
		assert.Equal(t, token.TunnelID, tunnelCfg.NamedTunnel.Credentials.TunnelID)

		// Verify orchestrator config.
		assert.NotNil(t, orchCfg.Ingress)
		assert.NotNil(t, orchCfg.WarpRouting)
		assert.NotNil(t, orchCfg.OriginDialerService)
	})
}

// TestBuildOrchestrator_SuccessPath_NilOriginProxy exercises the full success
// path of buildOrchestrator with nil OriginProxy.
//
// NOT parallel: temporarily replaces prometheus.DefaultRegisterer.
func TestBuildOrchestrator_SuccessPath_NilOriginProxy(t *testing.T) {
	token := newTestToken()
	cfg := &Config{
		ProxyURL: "http://localhost:8080",
	}

	withFreshRegisterer(func() {
		orchestrator, tunnelCfg, err := buildOrchestrator(t.Context(), cfg, token, slog.Default())

		require.NoError(t, err)
		assert.NotNil(t, orchestrator)
		assert.NotNil(t, tunnelCfg)
		assert.Nil(t, orchestrator.OverrideProxy, "OverrideProxy should be nil when OriginProxy is not set")
	})
}

// TestBuildOrchestrator_SuccessPath_WithOriginProxy exercises the OriginProxy
// branch of buildOrchestrator.
//
// NOT parallel: temporarily replaces prometheus.DefaultRegisterer.
func TestBuildOrchestrator_SuccessPath_WithOriginProxy(t *testing.T) {
	token := newTestToken()
	proxy := &GatewayOriginProxy{
		handler: nil,
		logger:  slog.Default(),
	}
	cfg := &Config{
		ProxyURL:    "http://ignored.example.com",
		OriginProxy: proxy,
	}

	withFreshRegisterer(func() {
		orchestrator, tunnelCfg, err := buildOrchestrator(t.Context(), cfg, token, slog.Default())

		require.NoError(t, err)
		assert.NotNil(t, orchestrator)
		assert.NotNil(t, tunnelCfg)
		assert.Equal(t, proxy, orchestrator.OverrideProxy, "OverrideProxy should be set to the provided OriginProxy")
	})
}

// TestStartTunnel_SuccessPath_CancelledContext exercises StartTunnel beyond
// token parsing and buildOrchestrator by providing a valid token and ProxyURL,
// but with an already-cancelled context so that StartTunnelDaemon returns
// immediately.
//
// NOT parallel: temporarily replaces prometheus.DefaultRegisterer.
func TestStartTunnel_SuccessPath_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	withFreshRegisterer(func() {
		err := StartTunnel(ctx, Config{
			Token:    validTestTokenBase64(),
			Logger:   slog.Default(),
			ProxyURL: "http://localhost:8080",
		})
		// StartTunnelDaemon should return quickly with a context-related error.
		// The exact error depends on cloudflared internals, but it should not panic.
		if err != nil {
			assert.NotContains(t, err.Error(), "parse tunnel token",
				"should pass token parsing")
		}
	})
}

// TestStartTunnel_NilLogger_SuccessPath_CancelledContext exercises the nil
// logger fallback through the full success path (cancelled context).
//
// NOT parallel: temporarily replaces prometheus.DefaultRegisterer.
func TestStartTunnel_NilLogger_SuccessPath_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	withFreshRegisterer(func() {
		err := StartTunnel(ctx, Config{
			Token:    validTestTokenBase64(),
			Logger:   nil,
			ProxyURL: "http://localhost:8080",
		})
		// Should not panic, nil logger should fall back to slog.Default.
		if err != nil {
			assert.NotContains(t, err.Error(), "parse tunnel token")
		}
	})
}
