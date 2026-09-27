package docsdrift_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestDeploySampleTunnelIDMatchesCRDPattern applies the CRD's tunnelID
// pattern to the raw-manifest sample. A placeholder that is not a UUID makes
// `kubectl apply` of the sample fail validation before the reader has edited
// anything.
func TestDeploySampleTunnelIDMatchesCRDPattern(t *testing.T) {
	t.Parallel()

	root := findRepoRoot(t)

	tunnelID, ok := gatewayClassConfigSpecProperties(t)["tunnelID"].(map[string]any)
	if !ok {
		t.Fatal("CRD spec has no tunnelID property — the extraction shape drifted")
	}

	pattern, ok := tunnelID["pattern"].(string)
	if !ok || pattern == "" {
		t.Fatal("CRD tunnelID carries no pattern — the extraction shape drifted")
	}

	raw, err := os.ReadFile(filepath.Join(root, "deploy", "samples", "gatewayclassconfig.yaml"))
	if err != nil {
		t.Fatalf("reading sample: %v", err)
	}

	var sample struct {
		Spec struct {
			TunnelID string `json:"tunnelID"` //nolint:tagliatelle // CRD field name
		} `json:"spec"`
	}

	err = yaml.Unmarshal(raw, &sample)
	if err != nil {
		t.Fatalf("parsing sample: %v", err)
	}

	if !regexp.MustCompile(pattern).MatchString(sample.Spec.TunnelID) {
		t.Errorf("deploy/samples/gatewayclassconfig.yaml tunnelID %q does not match the CRD pattern %s", sample.Spec.TunnelID, pattern)
	}
}

// deployImageRef matches every reference to one of the project's images in
// the raw manifests, whether as `image:` or as the --proxy-image flag value.
var deployImageRef = regexp.MustCompile(`ghcr\.io/lexfrei/cloudflare-tunnel-gateway-controller(?:-proxy)?:([^\s"']+)`)

// publishedTag is the form the release workflow publishes: the git tag with
// its leading "v" removed, both by docker/metadata-action's {{version}} and by
// the chart job's own shell strip.
var publishedTag = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// TestDeployManifestImagesUsePublishedTagForm pins the raw Deployment's image
// references to a tag shape the registry actually carries, and keeps the
// controller and proxy images on the same release.
func TestDeployManifestImagesUsePublishedTagForm(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join(findRepoRoot(t), "deploy", "controller", "deployment.yaml"))
	if err != nil {
		t.Fatalf("reading deployment: %v", err)
	}

	refs := deployImageRef.FindAllStringSubmatch(string(raw), -1)
	if len(refs) == 0 {
		t.Fatal("no project image reference found in deploy/controller/deployment.yaml — the extraction regex drifted")
	}

	for _, ref := range refs {
		if !publishedTag.MatchString(ref[1]) {
			t.Errorf("%s: tag %q is not in the published form MAJOR.MINOR.PATCH (no leading v)", ref[0], ref[1])
		}

		if ref[1] != refs[0][1] {
			t.Errorf("%s: tag %q differs from %q on the first image reference", ref[0], ref[1], refs[0][1])
		}
	}
}
