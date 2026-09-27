package docsdrift_test

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/gateway-api/pkg/consts"
)

// renovateConfig is the subset of renovate.json this test reads.
type renovateConfig struct {
	CustomManagers []struct {
		Description         string   `json:"description"`
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

	cfg := parseRenovate(t)
	managers := loadRenovateManagers(t, cfg)
	scopes := loadRenovateScopes(t, cfg)

	claims := map[string][]docClaim{
		"sigs.k8s.io/gateway-api":             gatewayAPIDocClaims(t),
		"sigs.k8s.io/gateway-api/conformance": conformanceDocClaims(t),
	}

	for dep, list := range claims {
		if len(managers[dep]) == 0 {
			t.Fatalf("renovate.json has no custom manager for %s", dep)
		}

		for _, claim := range list {
			if !anyMatches(managers[dep], claim.needle) {
				t.Errorf(
					"no matchString in the %s custom manager matches %q (pinned in %s) — Renovate would leave that claim stale and the bump PR would be red",
					dep, claim.needle, claim.file,
				)
			}
			if !anyMatches(scopes[dep], claim.file) {
				t.Errorf(
					"%s is outside the %s custom manager's managerFilePatterns, so Renovate never reads it — the regex matching %q there rewrites nothing",
					claim.file, dep, claim.needle,
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
	cfg := parseRenovate(t)
	managers := loadRenovateManagers(t, cfg)
	scopes := loadRenovateScopes(t, cfg)

	expected := map[string]string{
		"sigs.k8s.io/gateway-api":             consts.BundleVersion,
		"sigs.k8s.io/gateway-api/conformance": goModVersion(t, root, "sigs.k8s.io/gateway-api/conformance"),
	}

	for dep, want := range expected {
		inScope := filesInScope(t, root, scopes[dep])
		if len(inScope) == 0 {
			t.Fatalf("no file matches the %s custom manager's managerFilePatterns, so this test would pass without reading anything", dep)
		}

		for _, file := range inScope {
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

// TestRenovateCustomManagersStillMatch fails when a custom manager's
// matchString matches nothing in the files its managerFilePatterns select.
// Renovate reports nothing in that case: the dependency it was written for
// simply stops being updated.
func TestRenovateCustomManagersStillMatch(t *testing.T) {
	t.Parallel()

	root := findRepoRoot(t)
	cfg := parseRenovate(t)
	if len(cfg.CustomManagers) == 0 {
		t.Fatal("renovate.json has no custom managers, so this test would pass without reading anything")
	}

	for _, manager := range cfg.CustomManagers {
		inScope := filesInScope(t, root, compileFilePatterns(t, manager.Description, manager.ManagerFilePatterns))
		if len(inScope) == 0 {
			t.Errorf("custom manager %q: no file matches its managerFilePatterns", manager.Description)

			continue
		}

		bodies := make([]string, 0, len(inScope))
		for _, file := range inScope {
			body, err := os.ReadFile(filepath.Join(root, file))
			if err != nil {
				t.Fatalf("reading %s: %v", file, err)
			}
			bodies = append(bodies, string(body))
		}

		for _, pattern := range manager.MatchStrings {
			expr, err := regexp.Compile(pattern)
			if err != nil {
				t.Fatalf("custom manager %q: compiling matchString %q: %v", manager.Description, pattern, err)
			}
			if !slices.ContainsFunc(bodies, expr.MatchString) {
				t.Errorf(
					"custom manager %q: matchString %q matches nothing in %v, so the dependency it tracks is never updated",
					manager.Description, pattern, inScope,
				)
			}
		}
	}
}

// loadRenovateManagers compiles each Gateway API custom manager's matchStrings,
// keyed by the module it tracks.
func loadRenovateManagers(t *testing.T, cfg renovateConfig) map[string][]*regexp.Regexp {
	t.Helper()

	compiled := map[string][]*regexp.Regexp{}
	for _, manager := range cfg.CustomManagers {
		if !strings.HasPrefix(manager.DepNameTemplate, "sigs.k8s.io/gateway-api") {
			continue
		}
		for _, pattern := range manager.MatchStrings {
			expr, err := regexp.Compile(pattern)
			if err != nil {
				t.Fatalf("compiling matchString %q for %s: %v", pattern, manager.DepNameTemplate, err)
			}
			if expr.SubexpIndex("currentValue") < 0 {
				t.Fatalf(
					"matchString %q for %s has no currentValue group, so Renovate extracts nothing from it and rewrites nothing",
					pattern, manager.DepNameTemplate,
				)
			}
			compiled[manager.DepNameTemplate] = append(compiled[manager.DepNameTemplate], expr)
		}
	}

	return compiled
}

// loadRenovateScopes compiles each Gateway API custom manager's
// managerFilePatterns, rejecting anything that is not the /regex/ form.
func loadRenovateScopes(t *testing.T, cfg renovateConfig) map[string][]*regexp.Regexp {
	t.Helper()

	compiled := map[string][]*regexp.Regexp{}
	for _, manager := range cfg.CustomManagers {
		if !strings.HasPrefix(manager.DepNameTemplate, "sigs.k8s.io/gateway-api") {
			continue
		}
		compiled[manager.DepNameTemplate] = append(
			compiled[manager.DepNameTemplate],
			compileFilePatterns(t, manager.DepNameTemplate, manager.ManagerFilePatterns)...,
		)
	}

	return compiled
}

// compileFilePatterns compiles managerFilePatterns, rejecting anything that is
// not the /regex/ form.
func compileFilePatterns(t *testing.T, owner string, patterns []string) []*regexp.Regexp {
	t.Helper()

	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		if !strings.HasPrefix(pattern, "/") {
			t.Fatalf(
				"managerFilePattern %q for %s is not the /regex/ form this test assumes; a glob compiled as a regex matches more than it scopes, so the scope assertion would pass against something looser than reality",
				pattern, owner,
			)
		}
		expr, err := regexp.Compile(strings.TrimSuffix(strings.TrimPrefix(pattern, "/"), "/"))
		if err != nil {
			t.Fatalf("compiling managerFilePattern %q for %s: %v", pattern, owner, err)
		}
		compiled = append(compiled, expr)
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
		if entry.IsDir() {
			if skipWalkDir(entry.Name()) {
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

// skipWalkDir reports whether a tree walk should skip a directory holding
// third-party code, build output or local checkouts rather than repo content.
func skipWalkDir(name string) bool {
	return name == "vendor" || name == ".git" || name == "site" || name == ".claude"
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
// loadRenovateManagers guarantees the group exists.
func captures(pattern *regexp.Regexp, text string) []string {
	index := pattern.SubexpIndex("currentValue")
	matches := pattern.FindAllStringSubmatch(text, -1)

	found := make([]string, 0, len(matches))
	for _, match := range matches {
		found = append(found, match[index])
	}

	return found
}
