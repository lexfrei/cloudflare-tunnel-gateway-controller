package docsdrift_test

import (
	"fmt"
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
		{
			file:   filepath.Join("..", "..", "docs", "index.md"),
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the docs homepage install command must match the built-against bundle",
		},
		{
			file:   filepath.Join("..", "..", "docs", "development", "setup.md"),
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the dev setup install command must match the built-against bundle",
		},
		{
			file:   filepath.Join("..", "..", "docs", "operations", "manual-installation.md"),
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the manual install command must match the built-against bundle",
		},
		{
			file:   filepath.Join("..", "..", "docs", "reference", "crd-reference.md"),
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the CRD reference install command must match the built-against bundle",
		},
		{
			file:   filepath.Join("..", "..", "docs", "reference", "helm-chart.md"),
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the chart reference install command must match the built-against bundle",
		},
		{
			file:   filepath.Join("..", "..", "charts", "cloudflare-tunnel-gateway-controller", "README.md.gotmpl"),
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the chart README template (helm-docs source) must match the built-against bundle",
		},
		{
			file:   filepath.Join("..", "..", "hack", "conformance-setup.sh"),
			needle: "GATEWAY_API_VERSION=\"" + consts.BundleVersion + "\"",
			why:    "the vendored suite refuses to run against a CRD bundle that differs from consts.BundleVersion",
		},
		{
			file:   filepath.Join("..", "..", "CLAUDE.md"),
			needle: "sigs.k8s.io/gateway-api/conformance` " + consts.BundleVersion,
			why:    "the contributor doc names the conformance suite version",
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
		{
			file:   filepath.Join("..", "..", "test", "e2e", "e2e_backend_protocol_websocket_test.go"),
			needle: "cannot run",
			why:    "the conformance WebSocket test runs through the injectable dialer; the e2e is the production-pattern complement, not a substitute",
		},
		{
			file:   filepath.Join("..", "..", "test", "e2e", "e2e_backend_protocol_websocket_test.go"),
			needle: "no RoundTripper hook",
			why:    "gateway-api v1.6.0 added the WebSocket dialer injection point",
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

// TestNoRealInfrastructureHostnamesInFixtures keeps real tunnel hostnames out
// of committed fixtures — .env.example is explicit that real hostnames live
// only in the uncommitted .env. Reserved example domains (RFC 2606) are the
// fixture vocabulary.
func TestNoRealInfrastructureHostnamesInFixtures(t *testing.T) {
	t.Parallel()

	roots := []string{
		filepath.Join("..", "..", "test"),
		filepath.Join("..", "..", "internal"),
		filepath.Join("..", "..", "docs"),
	}

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return walkErr
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return fmt.Errorf("reading %s: %w", path, readErr)
			}
			realHostnameSuffix := "lexfrei" + ".dev" // concatenated so this scanner does not match itself
			if strings.Contains(string(body), realHostnameSuffix) {
				t.Errorf("%s references a real infrastructure hostname (%s); use an RFC 2606 example domain in fixtures", path, realHostnameSuffix)
			}

			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
}
