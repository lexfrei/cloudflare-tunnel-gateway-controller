package docsdrift_test

// Lint-command drift guard: CI lints with --build-tags so the tag-gated
// test packages (test/e2e, test/conformance, envtest suites) are compiled
// by the linter. Every documented local lint invocation must carry the
// same flag, or a contributor following the docs gets a local "0 issues"
// on packages the linter never compiled and a red CI — the exact blind
// spot the CI flag closed.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// lintCommandDocs are the files whose golangci-lint invocations must match
// the CI arguments. Every line mentioning `golangci-lint run` in these
// files must carry the CI --build-tags value.
var lintCommandDocs = []string{
	"CLAUDE.md",
	".github/pull_request_template.md",
	"docs/development/index.md",
	"docs/development/setup.md",
	"docs/development/contributing.md",
	"docs/development/testing.md",
}

// ciLintBuildTags extracts the --build-tags value from the lint job in
// .github/workflows/pr.yaml. Failing to find it is itself a drift: CI
// stopped linting the tag-gated packages.
func ciLintBuildTags(t *testing.T, repoRoot string) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "pr.yaml"))
	if err != nil {
		t.Fatalf("read pr.yaml: %v", err)
	}

	m := regexp.MustCompile(`args:.*--build-tags\s+(\S+)`).FindStringSubmatch(string(body))
	if m == nil {
		t.Fatal("pr.yaml lint job no longer passes --build-tags; the tag-gated test packages are invisible to CI lint again")
	}

	return m[1]
}

func TestDocumentedLintCommandsCarryCIBuildTags(t *testing.T) {
	t.Parallel()

	repoRoot := findRepoRoot(t)
	tags := ciLintBuildTags(t, repoRoot)
	want := "--build-tags " + tags

	for _, rel := range lintCommandDocs {
		body, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}

		for i, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, "golangci-lint run") {
				continue
			}

			if !strings.Contains(line, want) {
				t.Errorf("%s:%d documents a golangci-lint invocation without %q (CI lints with it; docs must match or tag-gated packages silently pass locally):\n  %s",
					rel, i+1, want, strings.TrimSpace(line))
			}
		}
	}
}
