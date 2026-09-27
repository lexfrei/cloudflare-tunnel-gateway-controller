package render_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

const controllerNetworkPolicyTemplate = "../../charts/cloudflare-tunnel-gateway-controller/templates/networkpolicy.yaml"

// TestControllerNetworkPolicyMatchesRenderedPlanes pins the chart's egress rule
// for per-Gateway config pushes to what the renderer actually produces. The
// template cannot import Go, so it repeats the pod label; if it moves here and
// not there, the controller policy stops admitting pushes to every per-Gateway
// plane. The port is the chart value the controller renders the planes with.
func TestControllerNetworkPolicyMatchesRenderedPlanes(t *testing.T) {
	t.Parallel()

	deployment := render.ProxyDeployment(testInput("edge"))

	name := deployment.Spec.Template.Labels["app.kubernetes.io/name"]
	require.NotEmpty(t, name, "rendered pod template carries no app.kubernetes.io/name label")

	raw, err := os.ReadFile(controllerNetworkPolicyTemplate)
	require.NoError(t, err)

	// The per-Gateway rule is the only one whose peer spans all namespaces.
	_, rule, found := strings.Cut(string(raw), "- namespaceSelector: {}")
	require.True(t, found, "per-Gateway egress rule not found in %s", controllerNetworkPolicyTemplate)

	rule, _, _ = strings.Cut(rule, "{{- end }}")

	// Whole-line matches, so a value that is a prefix of the other side's
	// (a shortened label) still counts as drift.
	assert.Regexp(t, `(?m)^\s*app\.kubernetes\.io/name: `+regexp.QuoteMeta(name)+`\s*$`, rule,
		"chart egress rule selects a different pod label than the renderer sets")
	assert.Regexp(t, `(?m)^\s*port: \{\{ \.Values\.proxy\.configAPIPort \}\}\s*$`, rule,
		"chart egress rule opens a port other than the one the controller renders planes with")
}
