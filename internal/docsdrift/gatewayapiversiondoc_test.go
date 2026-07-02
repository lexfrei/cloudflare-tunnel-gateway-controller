package docsdrift_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/gateway-api/pkg/consts"
)

// TestDocsPinnedGatewayAPIVersionMatchesVendored locks the user-facing docs
// that name the vendored Gateway API version to consts.BundleVersion, so a
// dependency bump cannot leave a stale version claim behind (the v1.5.1 →
// v1.6.0 bump missed limitations.md until review caught it).
func TestDocsPinnedGatewayAPIVersionMatchesVendored(t *testing.T) {
	t.Parallel()

	claims := []struct {
		file   string
		needle string
		why    string
	}{
		{
			file:   filepath.Join("..", "..", "docs", "gateway-api", "limitations.md"),
			needle: "Standard channel (Gateway API " + consts.BundleVersion + ")",
			why:    "the SupportedVersion limitation section names the pinned bundle the controller is built against",
		},
		{
			file:   filepath.Join("..", "..", "docs", "getting-started", "prerequisites.md"),
			needle: "built and tested against " + consts.BundleVersion,
			why:    "the prerequisites page names the tested bundle; SupportedVersion=False fires for any other minor",
		},
		{
			file:   filepath.Join("..", "..", "docs", "getting-started", "prerequisites.md"),
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the install command must fetch the same bundle version the controller is built against",
		},
		{
			file:   filepath.Join("..", "..", "README.md"),
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the README quick start must fetch the same bundle version the controller is built against",
		},
	}

	for _, claim := range claims {
		body, err := os.ReadFile(claim.file)
		if err != nil {
			t.Fatalf("reading %s: %v", claim.file, err)
		}
		if !strings.Contains(string(body), claim.needle) {
			t.Errorf(
				"%s does not contain %q — %s; update the doc (or this test) when bumping sigs.k8s.io/gateway-api",
				claim.file, claim.needle, claim.why,
			)
		}
	}
}

// TestDocsDoNotReclaimLiftedConformanceSkips pins the docs pages that used
// to describe conformance skips lifted by the v1.6.0 bump (GRPCRouteWeight
// through the injectable gRPC client, HTTPRouteBackendProtocolWebSocket
// through the injectable WebSocket dialer). If any of the retired claims
// come back, the docs are describing the product incorrectly.
func TestDocsDoNotReclaimLiftedConformanceSkips(t *testing.T) {
	t.Parallel()

	forbidden := []struct {
		file   string
		needle string
		why    string
	}{
		{
			file:   filepath.Join("..", "..", "docs", "gateway-api", "supported-resources.md"),
			needle: "stays skipped",
			why:    "GRPCRouteWeight runs through the injectable suite client as of gateway-api v1.6.0",
		},
		{
			file:   filepath.Join("..", "..", "docs", "gateway-api", "supported-resources.md"),
			needle: "bypasses the injectable",
			why:    "the v1.6.0 weight sampler routes through suite.GRPCClient",
		},
		{
			file:   filepath.Join("..", "..", "docs", "gateway-api", "limitations.md"),
			needle: "exposes no injection point",
			why:    "gateway-api v1.6.0 added an injectable WebSocket dialer; the conformance run supplies a tunnel-aware one",
		},
		{
			file:   filepath.Join("..", "..", "docs", "gateway-api", "limitations.md"),
			needle: "stays skipped",
			why:    "HTTPRouteBackendProtocolWebSocket is no longer skipped",
		},
		{
			file:   filepath.Join("..", "..", "docs", "development", "testing.md"),
			needle: "cannot dial through the tunnel",
			why:    "the conformance gRPC tests dial the Cloudflare edge via the injectable TunnelGRPCClient",
		},
		{
			file:   filepath.Join("..", "..", "docs", "development", "testing.md"),
			needle: "gRPC dialer cannot reach",
			why:    "the conformance gRPC tests dial the Cloudflare edge via the injectable TunnelGRPCClient",
		},
	}

	for _, claim := range forbidden {
		body, err := os.ReadFile(claim.file)
		if err != nil {
			t.Fatalf("reading %s: %v", claim.file, err)
		}
		if strings.Contains(string(body), claim.needle) {
			t.Errorf(
				"%s still contains %q — %s; the claim was retired by the gateway-api v1.6.0 bump",
				claim.file, claim.needle, claim.why,
			)
		}
	}
}
