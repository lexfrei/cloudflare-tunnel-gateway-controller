package docsdrift_test

import (
	"encoding/json"
	"os"
	"testing"
)

// TestValuesSchemaNamespaceSelectorClosed pins the hostnameOwnershipPolicy
// schema as CLOSED: a typo like `matchlabels:` must fail helm lint/install,
// not silently turn the selector into match-all — which would police every
// namespace in the cluster (fail-closed everywhere, denying routes of other
// Gateway implementations too).
func TestValuesSchemaNamespaceSelectorClosed(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../charts/cloudflare-tunnel-gateway-controller/values.schema.json")
	if err != nil {
		t.Fatalf("reading values.schema.json: %v", err)
	}

	var schema struct {
		Properties struct {
			HostnameOwnershipPolicy struct {
				AdditionalProperties *bool `json:"additionalProperties"`
				Properties           struct {
					NamespaceSelector struct {
						AdditionalProperties *bool `json:"additionalProperties"`
						Properties           struct {
							MatchExpressions struct {
								Items struct {
									AdditionalProperties *bool `json:"additionalProperties"`
								} `json:"items"`
							} `json:"matchExpressions"`
						} `json:"properties"`
					} `json:"namespaceSelector"`
				} `json:"properties"`
			} `json:"hostnameOwnershipPolicy"`
		} `json:"properties"`
	}

	err = json.Unmarshal(raw, &schema)
	if err != nil {
		t.Fatalf("parsing values.schema.json: %v", err)
	}

	policy := schema.Properties.HostnameOwnershipPolicy
	if policy.AdditionalProperties == nil || *policy.AdditionalProperties {
		t.Error("hostnameOwnershipPolicy must set additionalProperties: false")
	}

	selector := policy.Properties.NamespaceSelector
	if selector.AdditionalProperties == nil || *selector.AdditionalProperties {
		t.Error("hostnameOwnershipPolicy.namespaceSelector must set additionalProperties: false — a key typo must not become match-all")
	}

	items := selector.Properties.MatchExpressions.Items
	if items.AdditionalProperties == nil || *items.AdditionalProperties {
		t.Error("namespaceSelector.matchExpressions items must set additionalProperties: false")
	}
}
