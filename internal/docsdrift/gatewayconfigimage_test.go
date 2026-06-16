package docsdrift_test

import (
	"os"
	"regexp"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestGatewayConfigCRDImageValidated pins the GatewayConfig.spec.image
// admission guard: the field must carry a MinLength and a permissive
// image-reference Pattern so a garbage value (empty, leading junk, embedded
// whitespace) is rejected at admission — surfaced on the Gateway's status —
// rather than failing far away at pod-pull time. The pattern is deliberately
// loose: it must still accept registry[:port]/repo[:tag][@digest], so the test
// asserts BOTH that the marker rendered into the CRD AND that the rendered
// pattern accepts real refs while rejecting obvious garbage.
func TestGatewayConfigCRDImageValidated(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../charts/cloudflare-tunnel-gateway-controller/crds/cf.k8s.lex.la_gatewayconfigs.yaml")
	if err != nil {
		t.Fatalf("reading GatewayConfig CRD: %v", err)
	}

	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties map[string]struct {
							Properties map[string]struct {
								MinLength *int64 `json:"minLength"`
								Pattern   string `json:"pattern"`
							} `json:"properties"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"` //nolint:tagliatelle // upstream apiextensions field name
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}

	err = yaml.Unmarshal(raw, &crd)
	if err != nil {
		t.Fatalf("parsing GatewayConfig CRD: %v", err)
	}

	if len(crd.Spec.Versions) == 0 {
		t.Fatal("CRD has no versions — the extraction shape drifted")
	}

	image := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["image"]

	if image.MinLength == nil || *image.MinLength < 1 {
		t.Error("spec.image must carry minLength >= 1 — an empty image override must be rejected at admission")
	}

	if image.Pattern == "" {
		t.Fatal("spec.image must carry a pattern — a garbage image override must be rejected at admission")
	}

	pattern, compileErr := regexp.Compile(image.Pattern)
	if compileErr != nil {
		t.Fatalf("spec.image pattern %q does not compile: %v", image.Pattern, compileErr)
	}

	// The pattern must accept real image references — it is a sanity check, not
	// a strict OCI-reference grammar.
	for _, valid := range []string{
		"ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-proxy:v1.2.3",
		"registry.example.com:5000/team/proxy@sha256:abc123",
		"nginx",
		"proxy:latest",
	} {
		if !pattern.MatchString(valid) {
			t.Errorf("spec.image pattern rejects valid reference %q — the pattern is too strict", valid)
		}
	}

	// ...while still rejecting obvious garbage.
	for _, invalid := range []string{
		"",
		" leading-space",
		"-leading-dash",
		"has space",
	} {
		if pattern.MatchString(invalid) {
			t.Errorf("spec.image pattern accepts garbage reference %q — the pattern is too loose", invalid)
		}
	}
}
