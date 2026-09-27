package docsdrift_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// chartKubeFloor matches the lower bound of Chart.yaml's kubeVersion
// constraint, e.g. ">=1.25.0-0", capturing "1.25".
var chartKubeFloor = regexp.MustCompile(`^>=\s*v?([0-9]+\.[0-9]+)\.[0-9]+(?:-0)?$`)

// gatewayAPIBundleKubeFloor is the oldest Kubernetes minor that accepts the
// Gateway API standard bundle the controller is built against. The v1.6
// TLSRoute CRD has a validation rule calling the CEL isIP function, which a
// newly created CRD can use from 1.31. Raise it when a bundle bump raises the
// requirement.
var gatewayAPIBundleKubeFloor = [2]int{1, 31}

// TestChartKubeFloorCoversGatewayAPIBundle pins Chart.yaml's kubeVersion to
// no lower than the Gateway API bundle's floor, so Helm refuses a cluster
// that cannot install the CRDs the controller needs instead of installing a
// controller that can never start there.
func TestChartKubeFloorCoversGatewayAPIBundle(t *testing.T) {
	t.Parallel()

	floor := readChartKubeFloor(t, findRepoRoot(t))

	parts := strings.SplitN(floor, ".", 2)

	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])

	if majorErr != nil || minorErr != nil {
		t.Fatalf("Chart.yaml kubeVersion floor %q is not major.minor", floor)
	}

	want := gatewayAPIBundleKubeFloor
	if major < want[0] || (major == want[0] && minor < want[1]) {
		t.Errorf("Chart.yaml kubeVersion floor %s is below %d.%d, the oldest Kubernetes that accepts the Gateway API bundle",
			floor, want[0], want[1])
	}
}

// TestDocsKubernetesFloorMatchesChart pins the Kubernetes minimum the
// compatibility tables state to the kubeVersion constraint Helm enforces at
// install time, so raising the chart floor cannot leave the docs promising a
// version the chart refuses.
func TestDocsKubernetesFloorMatchesChart(t *testing.T) {
	t.Parallel()

	root := findRepoRoot(t)
	needle := "| Kubernetes | " + readChartKubeFloor(t, root) + "+"

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

// readChartKubeFloor returns the major.minor lower bound of Chart.yaml's
// kubeVersion constraint.
func readChartKubeFloor(t *testing.T, root string) string {
	t.Helper()

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

	return match[1]
}
