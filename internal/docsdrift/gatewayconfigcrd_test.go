package docsdrift_test

import (
	"os"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestGatewayConfigCRDSecretRefsAreNamespaceLocal pins the tenancy boundary
// of the GatewayConfig CRD: NO Secret reference in its spec may carry a
// namespace field. GatewayConfig is reached through
// Gateway.spec.infrastructure.parametersRef (namespace-local by Gateway API
// definition), and a cross-namespace credential reference would let a tenant
// point the controller at ANOTHER tenant's (or the operator's) Secret — a
// confused-deputy hole plus a secret-existence oracle through the surfaced
// status messages.
func TestGatewayConfigCRDSecretRefsAreNamespaceLocal(t *testing.T) {
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
								Properties map[string]any `json:"properties"`
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

	specProps := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
	if len(specProps) == 0 {
		t.Fatal("CRD spec has no properties — the extraction shape drifted")
	}

	checked := 0

	for name, field := range specProps {
		if field.Properties == nil {
			continue
		}

		if _, hasName := field.Properties["name"]; !hasName {
			continue // not a Secret-reference-shaped object
		}

		checked++

		if _, hasNamespace := field.Properties["namespace"]; hasNamespace {
			t.Errorf("spec.%s exposes a namespace field — GatewayConfig Secret references must be namespace-local", name)
		}
	}

	if checked < 3 {
		t.Fatalf("expected at least the three Secret references (tunnelToken, cloudflareCredentials, authToken); found %d — the extraction shape drifted", checked)
	}
}
