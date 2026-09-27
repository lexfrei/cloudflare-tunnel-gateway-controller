package docsdrift_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// chartKubeFloor matches the lower bound of Chart.yaml's kubeVersion
// constraint, e.g. ">=1.25.0-0", capturing "1.25".
var chartKubeFloor = regexp.MustCompile(`^>=\s*v?([0-9]+\.[0-9]+)\.[0-9]+(?:-0)?$`)

// TestDocsKubernetesFloorMatchesChart pins the Kubernetes minimum the
// compatibility tables state to the kubeVersion constraint Helm enforces at
// install time, so raising the chart floor cannot leave the docs promising a
// version the chart refuses.
func TestDocsKubernetesFloorMatchesChart(t *testing.T) {
	t.Parallel()

	root := findRepoRoot(t)

	raw, err := os.ReadFile(filepath.Join(root, "charts", "cloudflare-tunnel-gateway-controller", "Chart.yaml"))
	if err != nil {
		t.Fatalf("reading Chart.yaml: %v", err)
	}

	var chart struct {
		KubeVersion string `json:"kubeVersion"`
	}

	err = yaml.Unmarshal(raw, &chart)
	if err != nil {
		t.Fatalf("parsing Chart.yaml: %v", err)
	}

	match := chartKubeFloor.FindStringSubmatch(strings.TrimSpace(chart.KubeVersion))
	if match == nil {
		t.Fatalf("Chart.yaml kubeVersion %q is not a single >= bound; update this guard", chart.KubeVersion)
	}

	needle := "| Kubernetes | " + match[1] + "+"

	for _, file := range []string{"README.md", "docs/getting-started/prerequisites.md"} {
		body, readErr := os.ReadFile(filepath.Join(root, file))
		if readErr != nil {
			t.Fatalf("reading %s: %v", file, readErr)
		}

		if !strings.Contains(string(body), needle) {
			t.Errorf("%s does not contain %q, the floor Chart.yaml's kubeVersion enforces", file, needle)
		}
	}
}
