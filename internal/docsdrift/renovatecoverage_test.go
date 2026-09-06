package docsdrift_test

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/gateway-api/pkg/consts"
)

// renovateConfig is the subset of renovate.json this test reads.
type renovateConfig struct {
	CustomManagers []struct {
		DepNameTemplate     string   `json:"depNameTemplate"`
		MatchStrings        []string `json:"matchStrings"`
		ManagerFilePatterns []string `json:"managerFilePatterns"`
	} `json:"customManagers"`
}

// TestRenovateMatchesPinnedDocClaims ties the two halves of the Gateway API
// bump together: the docsdrift guard fails the branch when a pinned claim
// names a version the vendored module is not on, and renovate.json is what
// rewrites those claims. Reword a pinned sentence and the guard says so at
// once; nothing says the custom manager's regex stopped matching it, and the
// next bump arrives red with no hint of why. So assert the regexes against
// the very text the guard pins.
func TestRenovateMatchesPinnedDocClaims(t *testing.T) {
	t.Parallel()

	managers := loadRenovateManagers(t)

	covered := map[string][]*regexp.Regexp{
		"sigs.k8s.io/gateway-api":             managers["sigs.k8s.io/gateway-api"],
		"sigs.k8s.io/gateway-api/conformance": managers["sigs.k8s.io/gateway-api/conformance"],
	}

	claims := map[string][]docClaim{
		"sigs.k8s.io/gateway-api":             gatewayAPIDocClaims(),
		"sigs.k8s.io/gateway-api/conformance": conformanceDocClaims(t),
	}

	for dep, list := range claims {
		patterns := covered[dep]
		if len(patterns) == 0 {
			t.Fatalf("renovate.json has no custom manager for %s", dep)
		}

		for _, claim := range list {
			if !anyMatches(patterns, claim.needle) {
				t.Errorf(
					"no matchString in the %s custom manager matches %q (pinned in %s) — Renovate would leave that claim stale and the bump PR would be red",
					dep, claim.needle, claim.file,
				)
			}
		}
	}
}

// TestRenovateLeavesForeignVersionsAlone walks the files each Gateway API
// custom manager is scoped to and fails if a matchString captures a version
// other than the one that claim is pinned to. Those files also carry version
// mentions that are history rather than pins — when a CRD entered the
// Standard channel, which release the badge reports — and a regex loose
// enough to capture one would have Renovate silently rewrite a true sentence
// into a false one.
func TestRenovateLeavesForeignVersionsAlone(t *testing.T) {
	t.Parallel()

	root := findRepoRoot(t)
	managers := loadRenovateManagers(t)
	scopes := loadRenovateScopes(t)

	expected := map[string]string{
		"sigs.k8s.io/gateway-api":             consts.BundleVersion,
		"sigs.k8s.io/gateway-api/conformance": goModVersion(t, root, "sigs.k8s.io/gateway-api/conformance"),
	}

	for dep, want := range expected {
		for _, file := range filesInScope(t, root, scopes[dep]) {
			body, err := os.ReadFile(filepath.Join(root, file))
			if err != nil {
				t.Fatalf("reading %s: %v", file, err)
			}

			for _, pattern := range managers[dep] {
				for _, got := range captures(pattern, string(body)) {
					if got != want {
						t.Errorf(
							"%s: the %s custom manager captures %s via %q, but that claim is pinned to %s — Renovate would rewrite a version it does not own",
							file, dep, got, pattern.String(), want,
						)
					}
				}
			}
		}
	}
}

// loadRenovateManagers compiles each Gateway API custom manager's matchStrings,
// keyed by the module it tracks.
func loadRenovateManagers(t *testing.T) map[string][]*regexp.Regexp {
	t.Helper()

	compiled := map[string][]*regexp.Regexp{}
	for _, manager := range parseRenovate(t).CustomManagers {
		if !strings.HasPrefix(manager.DepNameTemplate, "sigs.k8s.io/gateway-api") {
			continue
		}
		for _, pattern := range manager.MatchStrings {
			expr, err := regexp.Compile(pattern)
			if err != nil {
				t.Fatalf("compiling matchString %q for %s: %v", pattern, manager.DepNameTemplate, err)
			}
			compiled[manager.DepNameTemplate] = append(compiled[manager.DepNameTemplate], expr)
		}
	}

	return compiled
}

// loadRenovateScopes compiles each Gateway API custom manager's
// managerFilePatterns, which Renovate writes as /regex/.
func loadRenovateScopes(t *testing.T) map[string][]*regexp.Regexp {
	t.Helper()

	compiled := map[string][]*regexp.Regexp{}
	for _, manager := range parseRenovate(t).CustomManagers {
		if !strings.HasPrefix(manager.DepNameTemplate, "sigs.k8s.io/gateway-api") {
			continue
		}
		for _, pattern := range manager.ManagerFilePatterns {
			expr, err := regexp.Compile(strings.Trim(pattern, "/"))
			if err != nil {
				t.Fatalf("compiling managerFilePattern %q for %s: %v", pattern, manager.DepNameTemplate, err)
			}
			compiled[manager.DepNameTemplate] = append(compiled[manager.DepNameTemplate], expr)
		}
	}

	return compiled
}

func parseRenovate(t *testing.T) renovateConfig {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(findRepoRoot(t), "renovate.json"))
	if err != nil {
		t.Fatalf("reading renovate.json: %v", err)
	}

	var cfg renovateConfig
	err = json.Unmarshal(body, &cfg)
	if err != nil {
		t.Fatalf("parsing renovate.json: %v", err)
	}

	return cfg
}

// filesInScope returns the repo-relative paths a set of managerFilePatterns
// selects, mirroring how Renovate walks the checkout.
func filesInScope(t *testing.T, root string, patterns []*regexp.Regexp) []string {
	t.Helper()

	var matched []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() {
			if name == "vendor" || name == ".git" || name == "site" {
				return fs.SkipDir
			}

			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return fmt.Errorf("relativising %s: %w", path, relErr)
		}
		rel = filepath.ToSlash(rel)
		for _, pattern := range patterns {
			if pattern.MatchString(rel) {
				matched = append(matched, rel)

				break
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	return matched
}

func anyMatches(patterns []*regexp.Regexp, text string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(text) {
			return true
		}
	}

	return false
}

// captures returns every currentValue the pattern extracts from the text.
func captures(pattern *regexp.Regexp, text string) []string {
	index := pattern.SubexpIndex("currentValue")
	if index < 0 {
		return nil
	}

	var found []string
	for _, match := range pattern.FindAllStringSubmatch(text, -1) {
		found = append(found, match[index])
	}

	return found
}
