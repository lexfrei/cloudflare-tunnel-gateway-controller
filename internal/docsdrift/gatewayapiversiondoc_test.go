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

	root := findRepoRoot(t)

	for _, claim := range gatewayAPIDocClaims() {
		body, err := os.ReadFile(filepath.Join(root, claim.file))
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

// gatewayAPIDocClaims is shared with TestRenovateMatchesPinnedDocClaims, which
// asserts that renovate.json rewrites every needle listed here.
func gatewayAPIDocClaims() []docClaim {
	return []docClaim{
		{
			file:   "docs/gateway-api/_spec-audit/00-compliance-matrix.md",
			needle: "# Gateway API " + consts.BundleVersion + " spec compliance matrix",
			why:    "the matrix title names the module version it is the matrix for, which is a fact about the tree",
		},
		{
			file:   "docs/gateway-api/_spec-audit/00-compliance-matrix.md",
			needle: "sigs.k8s.io/gateway-api " + consts.BundleVersion + "` Standard channel",
			why:    "the matrix names the module whose normative surface the clauses were extracted from",
		},
		{
			file:   "docs/gateway-api/limitations.md",
			needle: "Standard channel (Gateway API " + consts.BundleVersion + ")",
			why:    "the SupportedVersion limitation section names the pinned bundle the controller is built against",
		},
		{
			file:   "docs/getting-started/prerequisites.md",
			needle: "built and tested against " + consts.BundleVersion + ",",
			why:    "the prerequisites page names the tested bundle; SupportedVersion=False fires for any other minor",
		},
		{
			file:   "docs/getting-started/prerequisites.md",
			needle: "apply the " + consts.BundleVersion + " standard bundle",
			why:    "the prerequisites page tells an operator on an older bundle which one to install",
		},
		{
			file:   "docs/getting-started/prerequisites.md",
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the install command must fetch the same bundle version the controller is built against",
		},
		{
			file:   "README.md",
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the README quick start must fetch the same bundle version the controller is built against",
		},
		{
			file:   "docs/index.md",
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the docs homepage install command must match the built-against bundle",
		},
		{
			file:   "docs/development/setup.md",
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the dev setup install command must match the built-against bundle",
		},
		{
			file:   "docs/operations/manual-installation.md",
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the manual install command must match the built-against bundle",
		},
		{
			file:   "docs/reference/crd-reference.md",
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the CRD reference install command must match the built-against bundle",
		},
		{
			file:   "docs/reference/helm-chart.md",
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the chart reference install command must match the built-against bundle",
		},
		{
			file:   "charts/cloudflare-tunnel-gateway-controller/README.md.gotmpl",
			needle: "releases/download/" + consts.BundleVersion + "/standard-install.yaml",
			why:    "the chart README template (helm-docs source) must match the built-against bundle",
		},
		{
			file:   "hack/conformance-setup.sh",
			needle: "GATEWAY_API_VERSION=\"" + consts.BundleVersion + "\"",
			why:    "the vendored suite refuses to run against a CRD bundle that differs from consts.BundleVersion",
		},
	}
}

// TestDocsDoNotReclaimLiftedConformanceSkips pins the docs pages that used
// to describe conformance skips lifted by the v1.6.0 bump (GRPCRouteWeight
// through the injectable gRPC client, HTTPRouteBackendProtocolWebSocket
// through the injectable WebSocket dialer). If any of the retired claims
// come back, the docs are describing the product incorrectly.
func TestDocsDoNotReclaimLiftedConformanceSkips(t *testing.T) {
	t.Parallel()

	root := findRepoRoot(t)

	forbidden := []struct {
		file   string
		needle string
		why    string
	}{
		{
			file:   "docs/gateway-api/supported-resources.md",
			needle: "stays skipped",
			why:    "GRPCRouteWeight runs through the injectable suite client as of gateway-api v1.6.0",
		},
		{
			file:   "docs/gateway-api/supported-resources.md",
			needle: "bypasses the injectable",
			why:    "the v1.6.0 weight sampler routes through suite.GRPCClient",
		},
		{
			file:   "docs/gateway-api/limitations.md",
			needle: "exposes no injection point",
			why:    "gateway-api v1.6.0 added an injectable WebSocket dialer; the conformance run supplies a tunnel-aware one",
		},
		{
			file:   "docs/gateway-api/limitations.md",
			needle: "stays skipped",
			why:    "HTTPRouteBackendProtocolWebSocket is no longer skipped",
		},
		{
			file:   "docs/development/testing.md",
			needle: "cannot dial through the tunnel",
			why:    "the conformance gRPC tests dial the Cloudflare edge via the injectable TunnelGRPCClient",
		},
		{
			file:   "docs/development/testing.md",
			needle: "gRPC dialer cannot reach",
			why:    "the conformance gRPC tests dial the Cloudflare edge via the injectable TunnelGRPCClient",
		},
		{
			file:   "test/e2e/e2e_backend_protocol_websocket_test.go",
			needle: "cannot run",
			why:    "the conformance WebSocket test runs through the injectable dialer; the e2e is the production-pattern complement, not a substitute",
		},
		{
			file:   "test/e2e/e2e_backend_protocol_websocket_test.go",
			needle: "no RoundTripper hook",
			why:    "gateway-api v1.6.0 added the WebSocket dialer injection point",
		},
	}

	for _, claim := range forbidden {
		body, err := os.ReadFile(filepath.Join(root, claim.file))
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

	repoRoot := findRepoRoot(t)
	roots := []string{
		filepath.Join(repoRoot, "test"),
		filepath.Join(repoRoot, "internal"),
		filepath.Join(repoRoot, "docs"),
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

// TestSpecAuditAssessedThroughVendoredVersion pins the one claim in the
// compliance matrix that no bot may author. The others say which module
// version the matrix is for, which is a fact Renovate can rewrite from the
// vendored tree; this one says the verdicts were assessed against that
// release's normative surface, which is a judgement someone reached by
// reading the tag diff. It is deliberately absent from renovate.json, so a
// Gateway API bump arrives red here and is not automergeable until the
// assessment is done. That is the intended cost.
func TestSpecAuditAssessedThroughVendoredVersion(t *testing.T) {
	t.Parallel()

	const file = "docs/gateway-api/_spec-audit/00-compliance-matrix.md"

	needle := "The verdicts below were assessed through " + consts.BundleVersion + "."
	body, err := os.ReadFile(filepath.Join(findRepoRoot(t), file))
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}

	if !strings.Contains(string(body), needle) {
		t.Errorf(
			"%s does not say %q. The vendored Gateway API is now %s and nothing has recorded what its normative surface did to the verdicts below. "+
				"Read the upstream tag diff, add a row to the refresh section saying what changed and which audit rows move, then update this sentence. "+
				"Editing the sentence alone makes the matrix assert an audit that did not happen.",
			file, needle, consts.BundleVersion,
		)
	}
}
