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
