package docsdrift_test

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// gatewayClassConfigSpecProperties extracts the spec property names the shipped
// GatewayClassConfig CRD declares.
func gatewayClassConfigSpecProperties(t *testing.T) map[string]any {
	t.Helper()

	raw, err := os.ReadFile("../../charts/cloudflare-tunnel-gateway-controller/crds/cf.k8s.lex.la_gatewayclassconfigs.yaml")
	if err != nil {
		t.Fatalf("reading GatewayClassConfig CRD: %v", err)
	}

	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties map[string]struct {
							Properties map[string]any `json:"properties"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"` //nolint:tagliatelle // upstream apiextensions field name
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}

	err = yaml.Unmarshal(raw, &crd)
	if err != nil {
		t.Fatalf("parsing GatewayClassConfig CRD: %v", err)
	}

	if len(crd.Spec.Versions) == 0 {
		t.Fatal("CRD has no versions — the extraction shape drifted")
	}

	properties := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
	if len(properties) == 0 {
		t.Fatal("CRD spec has no properties — the extraction shape drifted")
	}

	return properties
}

// TestGatewayClassConfigCRDDeclaresEverySpecField pins the hand-maintained CRD
// YAML against the Go type it is supposed to mirror. There is no controller-gen
// target here, so the two drift by hand.
//
// The failure this prevents is silent in the worst direction: the apiserver
// PRUNES an undeclared field rather than rejecting it, so a security control
// the operator set — maxDataPlanesPerNamespace, allowSharedTunnels — would read
// back as its permissive default with nothing logged anywhere.
func TestGatewayClassConfigCRDDeclaresEverySpecField(t *testing.T) {
	t.Parallel()

	properties := gatewayClassConfigSpecProperties(t)
	specType := reflect.TypeFor[v1alpha1.GatewayClassConfigSpec]()

	for i := range specType.NumField() {
		name, _, _ := strings.Cut(specType.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}

		if _, ok := properties[name]; !ok {
			t.Errorf("GatewayClassConfigSpec.%s serialises as %q, which the shipped CRD does not declare: "+
				"the apiserver would prune it and the field would silently read as its default",
				specType.Field(i).Name, name)
		}
	}
}

// TestCRDReferenceDocumentsTheDataPlaneCap pins the operator-facing
// documentation of a field an operator has to know exists to use, and of the
// condition reason it produces.
//
// The reason is checked in its own TABLE ROW, not merely somewhere in the file.
// An operator who reads DataPlaneQuotaExceeded off a Gateway looks it up in the
// status-conditions table; a mention in the spec-field row above satisfies a
// whole-file search while leaving that table incomplete.
func TestCRDReferenceDocumentsTheDataPlaneCap(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../docs/reference/crd-reference.md")
	if err != nil {
		t.Fatalf("reading crd-reference.md: %v", err)
	}

	doc := string(raw)
	if !strings.Contains(doc, "maxDataPlanesPerNamespace") {
		t.Error("crd-reference.md must document the maxDataPlanesPerNamespace field")
	}

	if !strings.Contains(doc, "| `Accepted` | `False` | `DataPlaneQuotaExceeded` |") {
		t.Error("the Gateway status-conditions table must carry a DataPlaneQuotaExceeded row; " +
			"an operator reading that reason off a Gateway looks it up there")
	}
}
