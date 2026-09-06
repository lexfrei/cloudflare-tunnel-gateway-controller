package docsdrift_test

// helm-docs scope guard: `helm-docs` takes no positional argument. It
// accepts only flags, and --chart-search-root defaults to ".", so a
// chart path passed positionally is discarded and every chart reachable
// from the working directory is regenerated -- including sibling git
// worktrees under .claude/worktrees, whose owners then find a modified
// chart README in a branch that never touched the chart.

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// helmDocsPositionalArg matches a helm-docs command word followed by a
// non-flag token. The leading class rejects paths and hyphenated names
// (/usr/local/bin/helm-docs, helm-docs-tmp) that merely contain the
// binary name; group 2 is the discarded positional argument.
var helmDocsPositionalArg = regexp.MustCompile(`(?:^|[^\w./-])helm-docs[ \t]+([^-\s]\S*)`)

func TestHelmDocsInvocationsAreScopedToTheChart(t *testing.T) {
	t.Parallel()

	repoRoot := findRepoRoot(t)

	out, err := exec.CommandContext(t.Context(), "git", "-C", repoRoot, "ls-files", "-z", ":!vendor/**").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		checkHelmDocsScope(t, repoRoot, rel)
	}
}

// checkHelmDocsScope fails for each helm-docs invocation in the file
// whose FIRST argument is a chart path rather than --chart-search-root,
// which is the copy-paste form the documented command had. Separating a
// flag's value from a positional further along the line would need a
// table of which helm-docs flags take a value, and no freshness check
// is written that way. Every match on a line is examined: a benign
// leading match (`which helm-docs > /dev/null`) must not shadow a real
// invocation appended after it.
func checkHelmDocsScope(t *testing.T, repoRoot, rel string) {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if errors.Is(err, fs.ErrNotExist) {
		return // tracked but absent from the working tree; nothing to scan
	}

	if err != nil {
		t.Fatalf("read %s: %v", rel, err) // a file the guard cannot read is a file it does not cover
	}

	for i, line := range strings.Split(string(body), "\n") {
		for _, m := range helmDocsPositionalArg.FindAllStringSubmatch(line, -1) {
			if !namesChartDir(m[1]) {
				continue
			}

			t.Errorf("%s:%d passes %q to helm-docs positionally; helm-docs takes only flags, so the path is discarded and every chart under the working directory is regenerated. Use --chart-search-root:\n  %s",
				rel, i+1, m[1], strings.TrimSpace(line))
		}
	}
}

// namesChartDir reports whether a positional token is a chart directory
// -- a path or a variable holding one. Prose that merely contains the
// word "chart" after the binary name is not an invocation.
func namesChartDir(token string) bool {
	return strings.Contains(strings.ToLower(token), "chart") && strings.ContainsAny(token, "/$")
}
