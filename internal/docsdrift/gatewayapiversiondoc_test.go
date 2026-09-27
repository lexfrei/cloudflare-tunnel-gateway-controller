package docsdrift_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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

	for _, claim := range gatewayAPIDocClaims(t) {
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

// auditedGatewayAPIMinor is the Gateway API minor whose normative surface the
// spec audit under docs/gateway-api/_spec-audit/ was last read against. It
// moves by hand, never through Renovate.
const auditedGatewayAPIMinor = "v1.6"

// TestVendoredGatewayAPIMinorHasBeenAudited stops an unattended Gateway API
// minor bump. Patch releases pass through; a new minor can add or change
// normative clauses, so it waits for someone to read them.
func TestVendoredGatewayAPIMinorHasBeenAudited(t *testing.T) {
	t.Parallel()

	parts := strings.SplitN(consts.BundleVersion, ".", 3)
	if len(parts) < 2 {
		t.Fatalf("consts.BundleVersion %q is not vMAJOR.MINOR.PATCH", consts.BundleVersion)
	}
	if vendored := parts[0] + "." + parts[1]; vendored != auditedGatewayAPIMinor {
		t.Errorf(
			"sigs.k8s.io/gateway-api is now on minor %s, but the spec audit was last read against %s. "+
				"Read the new minor's normative changes against the audit in docs/gateway-api/_spec-audit/, "+
				"record the result in the matrix's baseline refresh section, then set auditedGatewayAPIMinor to %s",
			vendored, auditedGatewayAPIMinor, vendored,
		)
	}
}

// TestBundleVersionMatchesGoMod names the cause when upstream ships a
// consts.BundleVersion that disagrees with its own module tag. Renovate
// rewrites the doc claims from the module version while the claims are
// checked against the constant, so without this every claim would be
// reported stale instead.
func TestBundleVersionMatchesGoMod(t *testing.T) {
	t.Parallel()

	module := goModVersion(t, findRepoRoot(t), "sigs.k8s.io/gateway-api")
	if module != consts.BundleVersion {
		t.Errorf(
			"go.mod requires sigs.k8s.io/gateway-api %s but the vendored consts.BundleVersion is %s; the doc claims follow the constant, Renovate follows the module",
			module, consts.BundleVersion,
		)
	}
}

// gatewayAPIDocClaims is shared with TestRenovateMatchesPinnedDocClaims, which
// asserts that renovate.json rewrites every needle listed here. Install URLs
// come from a scan of the tree, so a new page carrying one is covered without
// an entry here; the prose forms vary too much to scan for and stay listed.
func gatewayAPIDocClaims(t *testing.T) []docClaim {
	t.Helper()

	urls := scanInstallURLs(t)
	claims := make([]docClaim, 0, len(urls))
	for _, found := range urls {
		claims = append(claims, docClaim{
			file:   found.file,
			needle: found.url,
			why:    "the install command must fetch the same bundle version the controller is built against",
		})
	}

	return append(claims, []docClaim{
		{
			file:   "docs/gateway-api/limitations.md",
			needle: "Standard channel (Gateway API " + consts.BundleVersion + ")",
			why:    "the SupportedVersion limitation section names the pinned bundle the controller is built against",
		},
		{
			file:   "docs/getting-started/prerequisites.md",
			needle: "built against " + consts.BundleVersion + ",",
			why:    "the prerequisites page names the bundle the controller is built against; SupportedVersion=False fires for any other minor",
		},
		{
			file:   "docs/getting-started/prerequisites.md",
			needle: "apply the " + consts.BundleVersion + " standard bundle",
			why:    "the prerequisites page tells an operator on an older bundle which one to install",
		},
		{
			file:   "README.md",
			needle: "Standard channel (Gateway API " + consts.BundleVersion + ")",
			why:    "the README compatibility table names the bundle the controller is built against",
		},
		{
			file:   "docs/getting-started/prerequisites.md",
			needle: "Standard channel (Gateway API " + consts.BundleVersion + ")",
			why:    "the prerequisites compatibility table names the bundle the controller is built against",
		},
		{
			file:   "hack/conformance-setup.sh",
			needle: "GATEWAY_API_VERSION=\"" + consts.BundleVersion + "\"",
			why:    "the vendored suite refuses to run against a CRD bundle that differs from consts.BundleVersion",
		},
	}...)
}

// installURLPattern matches a Gateway API release-asset install URL and
// captures the release it names.
var installURLPattern = regexp.MustCompile(`gateway-api/releases/download/([^/\s]+)/[a-z-]+-install\.yaml`)

// installURLUnpinned lists the paths whose install URLs, if any, are
// deliberately left out of the scan, each with the reason.
var installURLUnpinned = map[string]string{
	"docs/gateway-api/_spec-audit": "records an audit performed at a stated version; it moves by hand, not with a bump",
}

type installURL struct {
	file    string
	url     string
	version string
}

// scanInstallURLs returns every Gateway API install URL in the tree outside
// installURLUnpinned.
func scanInstallURLs(t *testing.T) []installURL {
	t.Helper()

	root := findRepoRoot(t)

	var found []installURL
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fmt.Errorf("relativising %s: %w", path, relErr)
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if skipWalkDir(entry.Name()) || installURLUnpinned[rel] != "" {
				return fs.SkipDir
			}

			return nil
		}

		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", path, readErr)
		}
		for _, match := range installURLPattern.FindAllStringSubmatch(string(body), -1) {
			found = append(found, installURL{file: rel, url: match[0], version: match[1]})
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	return found
}

// TestGatewayAPIInstallURLsNameVendoredBundle holds every Gateway API install
// URL in the tree to consts.BundleVersion, so a page added later is pinned
// the moment it carries one.
func TestGatewayAPIInstallURLsNameVendoredBundle(t *testing.T) {
	t.Parallel()

	found := scanInstallURLs(t)
	if len(found) == 0 {
		t.Fatal("the scan found no Gateway API install URL anywhere, so this test would pass without checking anything")
	}

	for _, url := range found {
		if url.version != consts.BundleVersion {
			t.Errorf(
				"%s installs Gateway API %s via %q, but the controller is built against %s; update the page when bumping sigs.k8s.io/gateway-api",
				url.file, url.version, url.url, consts.BundleVersion,
			)
		}
	}
}

// TestInstallURLUnpinnedPathsExist keeps the scan exclusions honest: an
// excluded path that was moved or deleted would silently stop meaning
// anything.
func TestInstallURLUnpinnedPathsExist(t *testing.T) {
	t.Parallel()

	root := findRepoRoot(t)
	for path, why := range installURLUnpinned {
		_, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			t.Errorf("installURLUnpinned names %s (%s), which does not exist: %v", path, why, err)
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
