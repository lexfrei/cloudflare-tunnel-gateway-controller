// Package docsdrift contains a mechanical guard against documentation
// regressions: after v3 cut, the docs corpus must not advertise features
// or vocabulary that the v3 implementation removed. If any of these
// strings come back into a tracked doc, this test fires.
//
// The point is to lock the rev together: if `internal/helm` or `AWGConfig`
// or `cloudflared.enabled` ever come back to the codebase, the docs that
// describe them must come back together, and vice versa.
package docsdrift_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retiredSubstrings lists v2 vocabulary that must not appear in the docs.
// Each entry has a why-it-was-removed explanation so a future maintainer
// who hits this test understands the v3 boundary.
//
//nolint:gochecknoglobals // test data table; package-level slice is the standard Go test pattern.
var retiredSubstrings = []struct {
	needle string
	why    string
}{
	{
		needle: "Helm SDK",
		why:    "v3 dropped internal/helm package; controller no longer manages cloudflared via Helm SDK",
	},
	{
		needle: "manage-cloudflared",
		why:    "no such CLI flag in v3 (or any prior version); --proxy-endpoints is the v3 required flag",
	},
	{
		needle: "gatewayClassConfig.cloudflared",
		why:    "v3 removed the cloudflared block from the GatewayClassConfig CRD spec",
	},
	{
		needle: "gatewayClassConfig.tunnelTokenSecretRef",
		why:    "v3 moved the tunnel-token Secret from the CRD to chart values (proxy.tunnelTokenSecretRef)",
	},
	{
		needle: "Optional cloudflared lifecycle",
		why:    "v3 ships a single mandatory in-process L7 proxy data plane; lifecycle is not optional",
	},
	{
		needle: "cloudflare-go/v6",
		why:    "go.mod is on cloudflare-go/v7; v6 is gone from the dependency graph",
	},
	{
		needle: "AmneziaWG Sidecar Issues",
		why:    "v3 has no AWG sidecar; troubleshooting section was deleted with the feature",
	},
	{
		needle: "proxy.enabled",
		why:    "v3 chart removed the proxy.enabled toggle; the proxy is always rendered",
	},
	{
		needle: "Helm chart to deploy cloudflared",
		why:    "v3 has no separate cloudflared deployment; the proxy embeds cloudflared transport",
	},
	{
		needle: "Helm chart under the hood",
		why:    "v3 controller does not delegate to another Helm chart",
	},
	{
		needle: "cloudflared.enabled: true",
		why:    "v3 dropped the cloudflared toggle from chart values and the CRD",
	},
	{
		needle: "cloudflared.enabled: false",
		why:    "v3 dropped the cloudflared toggle from chart values and the CRD",
	},
	{
		needle: "AmneziaWG Sidecar Guide",
		why:    "v3 deleted docs/guides/awg-sidecar.md; no AWG sidecar in the proxy pod",
	},
	{
		needle: "AWG, external-dns",
		why:    "docs navigation no longer advertises an AWG guide",
	},
	{
		needle: "Without L7 Proxy",
		why:    "v3 has a single data plane; with/without-proxy comparison tables are gone",
	},
	{
		needle: "Without the L7 proxy",
		why:    "v3 has a single data plane; with/without-proxy comparison tables are gone",
	},
	{
		needle: "without L7 proxy",
		why:    "v3 has a single data plane; with/without-proxy comparison tables are gone",
	},
	{
		needle: "Requires L7 proxy",
		why:    "v3 always has the proxy; per-feature 'requires proxy' annotations are stale",
	},
	{
		needle: "manageCloudflared",
		why:    "this Helm value never existed in the current chart and v3 strips even the dead helpers that referenced it",
	},
	{
		needle: "cloudflare.tunnelId",
		why:    "this Helm value never existed in the current chart; gatewayClassConfig.tunnelID is the v3 key",
	},
	{
		needle: "cloudflare.apiToken",
		why:    "this Helm value never existed in the current chart; gatewayClassConfig.cloudflareCredentialsSecretRef is the v3 path",
	},
	{
		needle: "v2 proxy",
		why:    "v3 has a single proxy data plane; 'v2 proxy' in comments is leftover from the dual-mode era",
	},
	{
		needle: "v1 path",
		why:    "v3 has no 'v1 path'; tunnel traffic always flows through the L7 proxy's OverrideProxy hook",
	},
}

// trackedRoots is the list of trees this guard scans. Walked
// recursively; non-text files are skipped by extension. Covers both the
// markdown docs corpus and the Go source — the "v2 proxy" / "v1 path"
// vocabulary must not survive in either.
//
//nolint:gochecknoglobals // test data; package-level is idiomatic.
var trackedRoots = []string{
	"docs",
	"CLAUDE.md",
	"README.md",
	"SECURITY.md",
	"charts/cloudflare-tunnel-gateway-controller/README.md",
	"charts/cloudflare-tunnel-gateway-controller/README.md.gotmpl",
	"charts/cloudflare-tunnel-gateway-controller/templates",
	"charts/cloudflare-tunnel-gateway-controller/examples",
	"deploy",
	"internal",
	"cmd",
	"api",
	"test",
}

// trackedExtensions are the file types the guard inspects. Everything
// else (images, lockfiles, binaries) is skipped.
//
//nolint:gochecknoglobals // test data; package-level is idiomatic.
var trackedExtensions = map[string]bool{
	".md":     true,
	".gotmpl": true,
	".go":     true,
	".yaml":   true,
	".yml":    true,
	".txt":    true, // chart NOTES.txt
	".sh":     true, // hack/conformance-setup.sh
}

// allowedFiles list paths that LEGITIMATELY mention retired vocabulary
// (the v2→v3 migration guide must call out what was removed; this test
// must reference the strings it's guarding against). One per file +
// substring pair.
//
//nolint:gochecknoglobals // test data; package-level is idiomatic.
var allowedFiles = map[string]map[string]bool{
	"docs/upgrading/v2-to-v3.md": {
		"Helm SDK":                                true,
		"gatewayClassConfig.cloudflared":          true,
		"gatewayClassConfig.tunnelTokenSecretRef": true,
		"cloudflare-go/v6":                        true,
		"proxy.enabled":                           true,
	},
	// values.yaml's `proxy:` description recaps the upgrade story (what was
	// removed in v3); helm-docs renders the recap into the chart README
	// table. Both mentions are intentional and locked here.
	"charts/cloudflare-tunnel-gateway-controller/README.md": {
		"proxy.enabled": true,
	},
	"charts/cloudflare-tunnel-gateway-controller/README.md.gotmpl": {
		"proxy.enabled": true,
	},
	// limitations.md walks operators through the GRPCRoute regression and
	// must reference the v2 default mode (`proxy.enabled: false`) by name
	// so they recognise their setup.
	"docs/gateway-api/limitations.md": {
		"proxy.enabled": true,
	},
	"internal/docsdrift/docsdrift_test.go": {
		"Helm SDK":                                true,
		"manage-cloudflared":                      true,
		"gatewayClassConfig.cloudflared":          true,
		"gatewayClassConfig.tunnelTokenSecretRef": true,
		"Optional cloudflared lifecycle":          true,
		"cloudflare-go/v6":                        true,
		"AmneziaWG Sidecar Issues":                true,
		"proxy.enabled":                           true,
		"Helm chart to deploy cloudflared":        true,
		"Helm chart under the hood":               true,
		"cloudflared.enabled: true":               true,
		"cloudflared.enabled: false":              true,
		"AmneziaWG Sidecar Guide":                 true,
		"AWG, external-dns":                       true,
		"Without L7 Proxy":                        true,
		"Without the L7 proxy":                    true,
		"without L7 proxy":                        true,
		"Requires L7 proxy":                       true,
		"manageCloudflared":                       true,
		"cloudflare.tunnelId":                     true,
		"cloudflare.apiToken":                     true,
		"v2 proxy":                                true,
		"v1 path":                                 true,
	},
}

func TestRetiredV2VocabularyAbsent(t *testing.T) {
	t.Parallel()

	repoRoot := findRepoRoot(t)

	for _, root := range trackedRoots {
		absRoot := filepath.Join(repoRoot, root)

		info, err := os.Stat(absRoot)
		if err != nil {
			if os.IsNotExist(err) {
				continue // missing path is fine; nothing to scan
			}

			t.Fatalf("stat %s: %v", absRoot, err)
		}

		if info.IsDir() {
			scanTree(t, repoRoot, absRoot)
		} else {
			scanFile(t, repoRoot, absRoot)
		}
	}
}

// scanTree walks every tracked file under root.
func scanTree(t *testing.T, repoRoot, root string) {
	t.Helper()

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		if !trackedExtensions[filepath.Ext(path)] {
			return nil
		}

		scanFile(t, repoRoot, path)

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// scanFile loads a file and reports any retired-vocabulary hits that are
// not in the per-file allowlist.
func scanFile(t *testing.T, repoRoot, absPath string) {
	t.Helper()

	relPath, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		relPath = absPath
	}

	body, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatalf("read %s: %v", absPath, err)
	}

	text := string(body)
	allowed := allowedFiles[relPath]

	for _, entry := range retiredSubstrings {
		if !strings.Contains(text, entry.needle) {
			continue
		}

		if allowed[entry.needle] {
			continue
		}

		t.Errorf("%s contains retired v2 vocabulary %q (why removed: %s); add to allowedFiles only if the mention is intentional",
			relPath, entry.needle, entry.why)
	}
}

// findRepoRoot walks up from the test working directory until it finds a
// directory containing go.mod, which marks the repo root.
func findRepoRoot(t *testing.T) string {
	t.Helper()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	dir := cwd
	for {
		_, statErr := os.Stat(filepath.Join(dir, "go.mod"))
		if statErr == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod starting from %s", cwd)
		}

		dir = parent
	}
}
