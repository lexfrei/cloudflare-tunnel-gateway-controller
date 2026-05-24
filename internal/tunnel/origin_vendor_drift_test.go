package tunnel_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/cloudflare/cloudflared/connection"
)

// TestVendoredCloudflaredUpgradeConstantsPinned guards against silent drift
// in the vendored cloudflared fork's WebSocket signaling constants.
//
// `GatewayOriginProxy.ProxyHTTP` (`internal/tunnel/origin.go`) re-injects
// the standard HTTP/1.1 `Connection: Upgrade` headers when cloudflared
// signals a WebSocket request via the `isWebsocket bool` parameter — but
// cloudflared itself determines that a request is a WebSocket upgrade by
// inspecting the `Cf-Cloudflared-Proxy-Connection-Upgrade: websocket`
// internal header that the Cloudflare edge sets when translating an
// HTTP/1.1 upgrade request onto the HTTP/2 tunnel transport.
//
// If a future cloudflared bump renames either constant — e.g. a new fork
// rebase or upstream change to the internal header name — our bridge's
// re-injection silently stops firing on real WebSocket requests because
// `connType` never resolves to `TypeWebsocket`. The breakage is end-to-end
// invisible in unit tests (they pass headers in explicitly), so the
// vendored value is the only signal a maintainer has at re-vendor time.
//
// This test fails loudly the moment `go mod vendor` brings in a renamed
// constant, forcing whoever does the bump to update both the production
// re-injection and any fake fixture that asserts against the same wire
// values.
func TestVendoredCloudflaredUpgradeConstantsPinned(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		"Cf-Cloudflared-Proxy-Connection-Upgrade",
		connection.InternalUpgradeHeader,
		"vendored cloudflared `InternalUpgradeHeader` changed — "+
			"GatewayOriginProxy bridge re-injection relies on cloudflared identifying "+
			"the request as a WebSocket upgrade via this exact header name. "+
			"If the value diverges, update `internal/tunnel/origin.go` "+
			"`GatewayOriginProxy.ProxyHTTP` and re-audit the sibling behavioural pin "+
			"`TestGatewayOriginProxy_ProxyHTTP_WebSocketReinjectsHeaders` "+
			"(`internal/tunnel/origin_test.go`), then update this test.")

	assert.Equal(t,
		"websocket",
		connection.WebsocketUpgrade,
		"vendored cloudflared `WebsocketUpgrade` token changed — "+
			"if the value diverges, the bridge's `isWebsocket` path will silently "+
			"stop firing because cloudflared no longer classifies requests as "+
			"`TypeWebsocket`. Audit `GatewayOriginProxy.ProxyHTTP` and re-audit "+
			"the sibling behavioural pin "+
			"`TestGatewayOriginProxy_ProxyHTTP_WebSocketReinjectsHeaders` "+
			"(`internal/tunnel/origin_test.go`) before re-pinning this constant.")
}
