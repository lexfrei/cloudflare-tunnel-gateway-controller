//go:build e2e

package e2e

import (
	"fmt"
	"strings"
)

// grpcRequiresHTTP2SkipReason returns a non-empty skip reason when the
// configured tunnel transport cannot carry gRPC, and "" when it can.
//
// gRPC mandates the http2 transport: cloudflared does not forward HTTP
// trailers over QUIC, so the grpc-status trailer is dropped at the edge and
// every call fails with "server closed the stream without sending trailers".
// Only "http2" is safe. "quic", "auto" (auto lets cloudflared negotiate QUIC),
// the unset value, and any unrecognised value are reported as skip so the
// suite fails fast with an actionable message instead of polling for ~90s
// before timing out against a transport that can never succeed.
func grpcRequiresHTTP2SkipReason(protocol string) string {
	if strings.EqualFold(strings.TrimSpace(protocol), "http2") {
		return ""
	}

	return fmt.Sprintf(
		"gRPC requires the http2 tunnel transport (configured protocol is %q): "+
			"cloudflared drops HTTP trailers over QUIC, so grpc-status is lost and every "+
			"gRPC call fails. Deploy the proxy with proxy.tunnel.protocol=http2.",
		protocol,
	)
}
