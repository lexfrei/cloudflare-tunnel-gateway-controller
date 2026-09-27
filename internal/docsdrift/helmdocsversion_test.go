package docsdrift_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestChartReadmeFooterMatchesPinnedHelmDocs ties the committed chart README
// to the helm-docs that hack/tools/go.mod pins. make helm-docs and CI both
// build that pin, so a README whose footer names another version was written
// by a binary neither of them runs, and the freshness check would fail on it.
func TestChartReadmeFooterMatchesPinnedHelmDocs(t *testing.T) {
	t.Parallel()

	root := findRepoRoot(t)
	version := goModVersion(t, filepath.Join(root, "hack", "tools"), "github.com/norwoodj/helm-docs")

	readme := filepath.Join("charts", "cloudflare-tunnel-gateway-controller", "README.md")

	body, err := os.ReadFile(filepath.Join(root, readme))
	if err != nil {
		t.Fatalf("reading %s: %v", readme, err)
	}

	footer := "using [helm-docs " + version + "]"
	if !strings.Contains(string(body), footer) {
		t.Errorf("%s does not contain %q; regenerate it with make helm-docs", readme, footer)
	}
}
