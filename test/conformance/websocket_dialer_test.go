//go:build conformance

package conformance

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildEdgeWebSocketConfig pins the URL-rewrite and header-conveyance
// decisions for the tunnel WebSocket dialer: the suite hands Dial a
// ws://<gwAddr>/path URL built from the (unroutable) Gateway status address,
// and the dialer must redirect the handshake to the edge hostname over wss
// while carrying the test's intended host via X-Original-Host — the
// WebSocket analogue of buildEdgeRequest (HTTP) and buildOutgoingMetadata
// (gRPC).
func TestBuildEdgeWebSocketConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		rawURL           string
		protocol         string
		origin           string
		edgeHost         string
		wantLocation     string
		wantOriginalHost string
		wantProtocol     []string
	}{
		{
			name:             "gateway host rewritten to edge, original host carried",
			rawURL:           "ws://abcd1234.cfargotunnel.com/ws",
			origin:           "ws://gateway/TestWebSocket",
			edgeHost:         "cf-conformance-test.example.com",
			wantLocation:     "wss://cf-conformance-test.example.com/ws",
			wantOriginalHost: "abcd1234.cfargotunnel.com",
		},
		{
			name:             "query string is preserved",
			rawURL:           "ws://abcd1234.cfargotunnel.com/ws?foo=bar",
			origin:           "ws://gateway/TestWebSocket",
			edgeHost:         "cf-conformance-test.example.com",
			wantLocation:     "wss://cf-conformance-test.example.com/ws?foo=bar",
			wantOriginalHost: "abcd1234.cfargotunnel.com",
		},
		{
			name:             "subprotocol passes through",
			rawURL:           "ws://abcd1234.cfargotunnel.com/ws",
			protocol:         "graphql-ws",
			origin:           "ws://gateway/TestWebSocket",
			edgeHost:         "cf-conformance-test.example.com",
			wantLocation:     "wss://cf-conformance-test.example.com/ws",
			wantOriginalHost: "abcd1234.cfargotunnel.com",
			wantProtocol:     []string{"graphql-ws"},
		},
		{
			name:             "empty subprotocol is not set",
			rawURL:           "ws://abcd1234.cfargotunnel.com/ws",
			protocol:         "",
			origin:           "ws://gateway/TestWebSocket",
			edgeHost:         "cf-conformance-test.example.com",
			wantLocation:     "wss://cf-conformance-test.example.com/ws",
			wantOriginalHost: "abcd1234.cfargotunnel.com",
			wantProtocol:     nil,
		},
		{
			name:             "host already equal to edge host carries no X-Original-Host",
			rawURL:           "ws://cf-conformance-test.example.com/ws",
			origin:           "ws://gateway/TestWebSocket",
			edgeHost:         "cf-conformance-test.example.com",
			wantLocation:     "wss://cf-conformance-test.example.com/ws",
			wantOriginalHost: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			config, err := buildEdgeWebSocketConfig(tt.rawURL, tt.protocol, tt.origin, tt.edgeHost)
			require.NoError(t, err)
			require.NotNil(t, config)

			assert.Equal(t, tt.wantLocation, config.Location.String(),
				"Location must target the edge hostname over wss, preserving path and query")
			assert.Equal(t, tt.origin, config.Origin.String(), "Origin must pass through unchanged")
			assert.Equal(t, tt.wantOriginalHost, config.Header.Get(originalHostHeader),
				"X-Original-Host must carry the test's intended host, empty when it already equals the edge host")
			assert.Equal(t, tt.wantProtocol, config.Protocol, "subprotocol must pass through only when non-empty")
			require.NotNil(t, config.TlsConfig)
			assert.Equal(t, tt.edgeHost, config.TlsConfig.ServerName, "TLS SNI must be the edge hostname")
		})
	}
}

// TestBuildEdgeWebSocketConfigInvalidURL pins the error path for a malformed
// suite-supplied URL.
func TestBuildEdgeWebSocketConfigInvalidURL(t *testing.T) {
	t.Parallel()

	_, err := buildEdgeWebSocketConfig("not a url", "", "ws://gateway/x", "edge.example.com")
	require.Error(t, err)
}
