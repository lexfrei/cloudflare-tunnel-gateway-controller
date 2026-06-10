package rbacdrift_test

// Guard against docs/reference/security.md drifting from the shipped RBAC:
// the doc's "Minimum required permissions" block claims to match the chart's
// ClusterRole, and operators build manual RBAC from it. A rule landing in
// the chart/deploy manifests without the doc following (or vice versa) is
// exactly how a hand-built install ends up Forbidden at runtime.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"sigs.k8s.io/yaml"
)

// expandRules flattens rules into a (apiGroup, resource) -> sorted verbs map
// so differently-grouped but semantically identical rule sets compare equal.
func expandRules(t *testing.T, rules []rbacRule) map[string]string {
	t.Helper()

	out := make(map[string]string)

	for _, rule := range rules {
		verbs := append([]string(nil), rule.Verbs...)
		sort.Strings(verbs)

		joined := ""
		for i, verb := range verbs {
			if i > 0 {
				joined += ","
			}

			joined += verb
		}

		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				out[group+"/"+resource] = joined
			}
		}
	}

	return out
}

func TestSecurityDocRBAC_MatchesDeployRole(t *testing.T) {
	t.Parallel()

	repoRoot := findRepoRoot(t)

	mdBody, err := os.ReadFile(filepath.Join(repoRoot, "docs", "reference", "security.md"))
	if err != nil {
		t.Fatalf("read security.md: %v", err)
	}

	fence := regexp.MustCompile("(?s)```yaml\n(# Minimum required permissions.*?)```").FindSubmatch(mdBody)
	if fence == nil {
		t.Fatal("security.md no longer carries the 'Minimum required permissions' yaml block this guard pins")
	}

	var doc struct {
		Rules []rbacRule `json:"rules"`
	}
	if err := yaml.Unmarshal(fence[1], &doc); err != nil {
		t.Fatalf("parse security.md rules block: %v", err)
	}

	deployRules := loadDeployClusterRole(t, filepath.Join(repoRoot, "deploy", "rbac", "role.yaml"))

	docMap := expandRules(t, doc.Rules)
	deployMap := expandRules(t, deployRules)

	for key, verbs := range deployMap {
		if docMap[key] != verbs {
			t.Errorf("security.md documents %q verbs as %q but the shipped role grants %q", key, docMap[key], verbs)
		}
	}

	for key := range docMap {
		if _, exists := deployMap[key]; !exists {
			t.Errorf("security.md documents %q which the shipped role does not grant", key)
		}
	}
}
