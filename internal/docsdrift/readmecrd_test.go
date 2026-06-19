package docsdrift_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestReadmeInfrastructureSupported pins the README supported-fields table to
// the shipped reality: per-Gateway data planes (#479) implement
// spec.infrastructure.parametersRef, so the table must NOT call
// spec.infrastructure "Not implemented" — a reader consulting it would be told
// the opposite of what the same README advertises.
func TestReadmeInfrastructureSupported(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}

	readme := string(raw)

	if regexp.MustCompile(`spec\.infrastructure` + "`" + `\s*\|\s*❌`).MatchString(readme) {
		t.Error("README marks spec.infrastructure unsupported, but per-Gateway data planes implement spec.infrastructure.parametersRef")
	}

	if !strings.Contains(readme, "spec.infrastructure.parametersRef") {
		t.Error("README must document spec.infrastructure.parametersRef support")
	}
}

// TestCRDReferenceCountMatchesShippedCRDs pins the CRD-reference prose count to
// the actual number of project-owned CRD YAMLs in the chart — the file claimed
// "two" after a third (GatewayConfig) shipped.
func TestCRDReferenceCountMatchesShippedCRDs(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir("../../charts/cloudflare-tunnel-gateway-controller/crds")
	if err != nil {
		t.Fatalf("reading crds dir: %v", err)
	}

	count := 0

	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") {
			count++
		}
	}

	if count != 3 {
		t.Fatalf("expected 3 shipped CRDs (GatewayClassConfig, ExternalBackend, GatewayConfig); found %d — update this test and the docs", count)
	}

	raw, err := os.ReadFile("../../docs/reference/crd-reference.md")
	if err != nil {
		t.Fatalf("reading crd-reference.md: %v", err)
	}

	doc := string(raw)
	if strings.Contains(doc, "ships two project-owned CRDs") {
		t.Error("crd-reference.md says 'two project-owned CRDs' but three ship")
	}

	if !strings.Contains(doc, "GatewayConfig") {
		t.Error("crd-reference.md must mention GatewayConfig")
	}
}
