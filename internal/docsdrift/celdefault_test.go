package docsdrift_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// TestGatewayConfigCRDMaxReplicasCELMatchesDefault pins the autoscaling
// maxReplicas CEL rule to DefaultProxyReplicas. The rule hardcodes the HA
// default ("self.maxReplicas >= 2") as the floor when minReplicas is unset; if
// DefaultProxyReplicas ever changes, the literal in the kubebuilder marker
// would silently disagree with the Go default. Assert the generated CRD still
// references the current constant so the two move together.
func TestGatewayConfigCRDMaxReplicasCELMatchesDefault(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../charts/cloudflare-tunnel-gateway-controller/crds/cf.k8s.lex.la_gatewayconfigs.yaml")
	if err != nil {
		t.Fatalf("reading GatewayConfig CRD: %v", err)
	}

	want := fmt.Sprintf("self.maxReplicas >= %d", v1alpha1.DefaultProxyReplicas)
	if !strings.Contains(string(raw), want) {
		t.Errorf("GatewayConfig CRD maxReplicas CEL rule must reference DefaultProxyReplicas (%d); "+
			"expected the rule to contain %q — update the kubebuilder marker and regenerate the CRD",
			v1alpha1.DefaultProxyReplicas, want)
	}
}
