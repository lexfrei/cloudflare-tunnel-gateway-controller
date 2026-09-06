package docsdrift_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocsPinnedConformanceSuiteVersionMatchesGoMod locks the docs that name
// the conformance suite version to sigs.k8s.io/gateway-api/conformance in
// go.mod. That module releases separately from sigs.k8s.io/gateway-api, so
// consts.BundleVersion — which ships in the API module — is the wrong source
// for this claim: the two agreed only for as long as nobody bumped one alone,
// and PR #737 bumped the conformance module by itself.
func TestDocsPinnedConformanceSuiteVersionMatchesGoMod(t *testing.T) {
	t.Parallel()

	for _, claim := range conformanceDocClaims(t) {
		body, err := os.ReadFile(claim.file)
		if err != nil {
			t.Fatalf("reading %s: %v", claim.file, err)
		}
		if !strings.Contains(string(body), claim.needle) {
			t.Errorf(
				"%s does not contain %q — %s; update the doc when bumping sigs.k8s.io/gateway-api/conformance",
				claim.file, claim.needle, claim.why,
			)
		}
	}
}

// conformanceDocClaims is shared with TestRenovateMatchesPinnedDocClaims,
// which asserts that renovate.json rewrites every needle listed here.
func conformanceDocClaims(t *testing.T) []docClaim {
	t.Helper()

	root := findRepoRoot(t)
	version := goModVersion(t, root, "sigs.k8s.io/gateway-api/conformance")

	return []docClaim{
		{
			file:   filepath.Join(root, "CLAUDE.md"),
			needle: "sigs.k8s.io/gateway-api/conformance` " + version,
			why:    "the contributor doc names the conformance suite the kind run executes",
		},
	}
}

// docClaim is one pinned version claim: a file, the exact text that must
// appear in it, and why that text is load-bearing.
type docClaim struct {
	file   string
	needle string
	why    string
}

// goModVersion returns the version go.mod requires for the given module path.
// Lines carrying a replace arrow are skipped so the left-hand side of a
// replace directive cannot be mistaken for a requirement.
func goModVersion(t *testing.T, root, module string) string {
	t.Helper()

	file, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("opening go.mod: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "=>") {
			continue
		}

		fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "require "))
		if len(fields) >= 2 && fields[0] == module && strings.HasPrefix(fields[1], "v") {
			return fields[1]
		}
	}
	err = scanner.Err()
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}

	t.Fatalf("go.mod has no require entry for %s", module)

	return ""
}
