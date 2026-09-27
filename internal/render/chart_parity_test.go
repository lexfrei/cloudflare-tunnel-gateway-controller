package render_test

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

const controllerNetworkPolicyTemplate = "../../charts/cloudflare-tunnel-gateway-controller/templates/networkpolicy.yaml"

// TestControllerNetworkPolicyMatchesRenderedPlanes pins the chart's egress rule
// for per-Gateway config pushes to what the renderer actually produces. The
// template cannot import Go, so it repeats the pod label and the config-API
// port; if either moves here and not there, the controller policy stops
// admitting pushes to every per-Gateway plane.
func TestControllerNetworkPolicyMatchesRenderedPlanes(t *testing.T) {
	t.Parallel()

	deployment := render.ProxyDeployment(testInput("edge"))

	name := deployment.Spec.Template.Labels["app.kubernetes.io/name"]
	require.NotEmpty(t, name, "rendered pod template carries no app.kubernetes.io/name label")

	var configPort int32

	for _, port := range deployment.Spec.Template.Spec.Containers[0].Ports {
		if port.Name == "config-api" {
			configPort = port.ContainerPort
		}
	}

	require.NotZero(t, configPort, "rendered proxy container has no config-api port")

	raw, err := os.ReadFile(controllerNetworkPolicyTemplate)
	require.NoError(t, err)

	// The per-Gateway rule is the only one whose peer spans all namespaces.
	_, rule, found := strings.Cut(string(raw), "- namespaceSelector: {}")
	require.True(t, found, "per-Gateway egress rule not found in %s", controllerNetworkPolicyTemplate)

	rule, _, _ = strings.Cut(rule, "{{- end }}")

	// Whole-line matches, so a value that is a prefix of the other side's
	// (a shortened label, port 80 against 8081) still counts as drift.
	assert.Regexp(t, `(?m)^\s*app\.kubernetes\.io/name: `+regexp.QuoteMeta(name)+`\s*$`, rule,
		"chart egress rule selects a different pod label than the renderer sets")
	assert.Regexp(t, `(?m)^\s*port: `+strconv.Itoa(int(configPort))+`\s*$`, rule,
		"chart egress rule opens a different port than the rendered config API listens on")
}
