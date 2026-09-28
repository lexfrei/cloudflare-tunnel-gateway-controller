package render_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

func envValue(container *corev1.Container, name string) (string, bool) {
	for _, env := range container.Env {
		if env.Name == name {
			return env.Value, true
		}
	}

	return "", false
}

// TestProxyDeployment_ConfigTLSMountsTheLeaf pins the TLS wiring of a
// per-Gateway plane: the leaf Secret is mounted read-only, the proxy is
// pointed at it, and the probes follow the listener to HTTPS.
func TestProxyDeployment_ConfigTLSMountsTheLeaf(t *testing.T) {
	t.Parallel()

	input := testInput("edge")
	input.ConfigTLSSecretName = render.ConfigTLSSecretName(input.Gateway, 3)

	spec := render.ProxyDeployment(input).Spec.Template.Spec
	container := spec.Containers[0]

	require.Len(t, spec.Volumes, 1)
	require.NotNil(t, spec.Volumes[0].Secret)
	assert.Equal(t, "cf-proxy-edge-config-tls-3", spec.Volumes[0].Secret.SecretName)

	require.Len(t, container.VolumeMounts, 1)
	assert.True(t, container.VolumeMounts[0].ReadOnly)
	assert.Equal(t, spec.Volumes[0].Name, container.VolumeMounts[0].Name)

	certFile, ok := envValue(&container, "PROXY_CONFIG_TLS_CERT_FILE")
	require.True(t, ok)
	assert.Equal(t, container.VolumeMounts[0].MountPath+"/tls.crt", certFile)

	keyFile, ok := envValue(&container, "PROXY_CONFIG_TLS_KEY_FILE")
	require.True(t, ok)
	assert.Equal(t, container.VolumeMounts[0].MountPath+"/tls.key", keyFile)

	for _, probe := range []*corev1.Probe{container.StartupProbe, container.LivenessProbe, container.ReadinessProbe} {
		assert.Equal(t, corev1.URISchemeHTTPS, probe.HTTPGet.Scheme)
	}
}

// TestProxyDeployment_NoConfigTLSKeepsThePlaintextWire pins the opt-out: with
// no leaf nothing TLS is rendered and the probes stay HTTP.
func TestProxyDeployment_NoConfigTLSKeepsThePlaintextWire(t *testing.T) {
	t.Parallel()

	spec := render.ProxyDeployment(testInput("edge")).Spec.Template.Spec
	container := spec.Containers[0]

	assert.Empty(t, spec.Volumes)
	assert.Empty(t, container.VolumeMounts)

	_, ok := envValue(&container, "PROXY_CONFIG_TLS_CERT_FILE")
	assert.False(t, ok)

	for _, probe := range []*corev1.Probe{container.StartupProbe, container.LivenessProbe, container.ReadinessProbe} {
		assert.Equal(t, corev1.URISchemeHTTP, probe.HTTPGet.Scheme)
	}
}

func TestConfigTLSEndpointURL(t *testing.T) {
	t.Parallel()

	gateway := testInput("edge").Gateway

	assert.Equal(t, "https://cf-proxy-edge-config.tenant-a.svc.cluster.local:8081/config",
		render.ConfigTLSEndpointURL(gateway, "cluster.local", 0))
	assert.Equal(t, "cf-proxy-edge-config.tenant-a.svc.cluster.local",
		render.ConfigServerName(gateway, "cluster.local"))
}

// TestConfigTLSSecretIndex_ReadsTheMountedSlot pins how the reconciler learns
// which leaf a running plane mounts, so issuance never steps backwards.
func TestConfigTLSSecretIndex_ReadsTheMountedSlot(t *testing.T) {
	t.Parallel()

	input := testInput("edge")
	input.ConfigTLSSecretName = render.ConfigTLSSecretName(input.Gateway, 7)

	index, ok := render.ConfigTLSSecretIndex(input.Gateway, render.ProxyDeployment(input))
	require.True(t, ok)
	assert.Equal(t, 7, index)

	_, ok = render.ConfigTLSSecretIndex(input.Gateway, render.ProxyDeployment(testInput("edge")))
	assert.False(t, ok)

	_, ok = render.ConfigTLSSecretIndex(input.Gateway, &appsv1.Deployment{})
	assert.False(t, ok)
}

// TestConfigTLSSecretSlot_ReadsTheIndexFromTheName pins that a slot name maps
// back to its index for short and truncated Gateway names alike, and that a
// name belonging to another Gateway or to no slot at all maps to nothing.
func TestConfigTLSSecretSlot_ReadsTheIndexFromTheName(t *testing.T) {
	t.Parallel()

	short := testInput("edge").Gateway
	long := testInput(strings.Repeat("g", 80)).Gateway

	for _, gateway := range []*gatewayv1.Gateway{short, long} {
		for _, index := range []int{0, 7, 10, render.MaxConfigTLSSlot - 1} {
			name := render.ConfigTLSSecretName(gateway, index)
			assert.LessOrEqual(t, len(name), render.MaxDNSLabelLength)

			got, ok := render.ConfigTLSSecretSlot(gateway, name)
			require.True(t, ok, "slot %d of %q must be recognised", index, gateway.Name)
			assert.Equal(t, index, got)
		}
	}

	other := testInput("edge-config-tls-1").Gateway

	for _, name := range []string{
		render.ConfigTLSSecretName(long, 3),
		render.ConfigTLSSecretName(other, 0),
		render.ConfigTLSSecretName(short, 3) + "x",
		"cf-proxy-edge-config-tls-03",
		"cf-proxy-edge-config-tls-+3",
		"cf-proxy-edge-config-tls-",
		"cf-proxy-edge-config-tls-" + strconv.Itoa(render.MaxConfigTLSSlot),
		render.GeneratedAuthSecretName(short),
	} {
		_, ok := render.ConfigTLSSecretSlot(short, name)
		assert.False(t, ok, "%q is not a slot of %q", name, short.Name)
	}
}

// TestConfigTLSSecretName_StaysDistinctForLongNames pins that slots of a
// Gateway whose name is truncated still get distinct Secret names.
func TestConfigTLSSecretName_StaysDistinctForLongNames(t *testing.T) {
	t.Parallel()

	long := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{
		Name: "a-very-long-gateway-name-that-overflows-the-kubernetes-name-limit-easily", Namespace: "tenant-a",
	}}

	seen := make(map[string]bool)

	for index := range 20 {
		name := render.ConfigTLSSecretName(long, index)
		assert.LessOrEqual(t, len(name), render.MaxDNSLabelLength)
		assert.False(t, seen[name], "slot %d reuses a name", index)
		seen[name] = true
	}
}
