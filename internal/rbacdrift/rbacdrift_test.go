// Package rbacdrift contains a guard against the chart-rendered ClusterRole
// and the standalone deploy/rbac/role.yaml drifting apart. Both ship the
// same RBAC scope and operators may pick either; if a rule lands in one
// but not the other, this test fires.
//
// Without this guard the chart and the manual-install YAML can disagree
// silently — exactly how the legacy Helm-SDK rules survived through v3:
// nothing forced the two surfaces to stay in lockstep.
package rbacdrift_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"sigs.k8s.io/yaml"
)

type rbacRule struct {
	APIGroups []string `json:"apiGroups"`
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}

type clusterRole struct {
	Kind  string     `json:"kind"`
	Rules []rbacRule `json:"rules"`
}

// TestChartAndDeployRBAC_Match renders the chart's ClusterRole and parses
// deploy/rbac/role.yaml, then asserts the two rule sets are identical.
// Skipped when `helm` is not on PATH (CI always has it; local dev may not).
func TestChartAndDeployRBAC_Match(t *testing.T) {
	t.Parallel()

	helmBin, helmErr := exec.LookPath("helm")
	if helmErr != nil {
		// Under CI the chart-vs-deploy RBAC contract MUST be enforced; a
		// silent skip would let the drift guard go dark on a runner
		// refresh that drops helm from PATH. Locally helm may be
		// genuinely unavailable -- skip is fine there.
		if os.Getenv("CI") != "" {
			t.Fatalf("helm is required on PATH under CI but was not found: %v", helmErr)
		}

		t.Skip("helm not on PATH; skipping chart-vs-deploy RBAC drift check (local dev)")
	}

	repoRoot := findRepoRoot(t)

	chartRules := renderChartClusterRole(t, helmBin, repoRoot)
	deployRules := loadDeployClusterRole(t, filepath.Join(repoRoot, "deploy", "rbac", "role.yaml"))

	normaliseRules(chartRules)
	normaliseRules(deployRules)

	if !reflect.DeepEqual(chartRules, deployRules) {
		t.Errorf("chart ClusterRole rules diverge from deploy/rbac/role.yaml.\n\nchart:\n%s\n\ndeploy:\n%s",
			rulesDump(chartRules), rulesDump(deployRules))
	}
}

// renderChartClusterRole calls `helm template` and extracts the ClusterRole
// document. The chart requires proxy.tunnelTokenSecretRef.name, so we pass
// it; the value is irrelevant to the RBAC scope.
func renderChartClusterRole(t *testing.T, helmBin, repoRoot string) []rbacRule {
	t.Helper()

	chartDir := filepath.Join(repoRoot, "charts", "cloudflare-tunnel-gateway-controller")

	cmd := exec.CommandContext(context.Background(), helmBin,
		"template", "test-release", chartDir,
		"--show-only", "templates/clusterrole.yaml",
		"--set", "proxy.tunnelTokenSecretRef.name=dummy")

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template failed: %v\nstderr: %s", err, stderr.String())
	}

	var cr clusterRole

	yamlErr := yaml.Unmarshal(out, &cr)
	if yamlErr != nil {
		t.Fatalf("unmarshal chart ClusterRole: %v", yamlErr)
	}

	if cr.Kind != "ClusterRole" {
		t.Fatalf("expected ClusterRole, got %q", cr.Kind)
	}

	return cr.Rules
}

// loadDeployClusterRole reads the standalone manifest and extracts the
// ClusterRole's rules.
func loadDeployClusterRole(t *testing.T, path string) []rbacRule {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var cr clusterRole

	yamlErr := yaml.Unmarshal(body, &cr)
	if yamlErr != nil {
		t.Fatalf("unmarshal deploy ClusterRole: %v", yamlErr)
	}

	if cr.Kind != "ClusterRole" {
		t.Fatalf("expected ClusterRole, got %q", cr.Kind)
	}

	return cr.Rules
}

// normaliseRules sorts every slice in every rule so deep-equality compares
// content, not ordering. The chart and deploy YAML may legitimately order
// rules and verbs differently for readability.
func normaliseRules(rules []rbacRule) {
	for i := range rules {
		sort.Strings(rules[i].APIGroups)
		sort.Strings(rules[i].Resources)
		sort.Strings(rules[i].Verbs)
	}

	sort.Slice(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		if !reflect.DeepEqual(a.APIGroups, b.APIGroups) {
			return joinKey(a.APIGroups) < joinKey(b.APIGroups)
		}

		if !reflect.DeepEqual(a.Resources, b.Resources) {
			return joinKey(a.Resources) < joinKey(b.Resources)
		}

		return joinKey(a.Verbs) < joinKey(b.Verbs)
	})
}

func joinKey(s []string) string {
	out := make([]byte, 0, 32)
	for _, v := range s {
		out = append(out, '|')
		out = append(out, v...)
	}

	return string(out)
}

func rulesDump(rules []rbacRule) string {
	b, _ := yaml.Marshal(rules)

	return string(b)
}

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

		if errors.Is(statErr, os.ErrNotExist) {
			parent := filepath.Dir(dir)
			if parent == dir {
				t.Fatalf("could not find go.mod starting from %s", cwd)
			}

			dir = parent

			continue
		}

		t.Fatalf("stat %s/go.mod: %v", dir, statErr)
	}
}
