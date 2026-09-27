package render_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// A moved config-API port reaches every place a per-Gateway plane uses it: the
// proxy's listen address, the container port, the headless Service, the
// NetworkPolicy and the URL the controller pushes to.
func TestConfigAPIPort_MovesEveryPerGatewayUse(t *testing.T) {
	t.Parallel()

	input := testInput("edge")
	input.Defaults.ConfigAPIPort = 9091

	container := render.ProxyDeployment(input).Spec.Template.Spec.Containers[0]
	assert.Contains(t, container.Env, corev1.EnvVar{Name: "PROXY_CONFIG_ADDR", Value: ":9091"})
	assert.Contains(t, container.Ports,
		corev1.ContainerPort{Name: "config-api", ContainerPort: 9091, Protocol: corev1.ProtocolTCP})

	service := render.ConfigService(input)
	require.Len(t, service.Spec.Ports, 1)
	assert.Equal(t, int32(9091), service.Spec.Ports[0].Port)

	netpol := render.ProxyNetworkPolicy(render.NetworkPolicyInput{
		Gateway:             input.Gateway,
		ControllerNamespace: "cf-system",
		ConfigAPIPort:       9091,
	})
	require.Len(t, netpol.Spec.Ingress, 1)
	require.Len(t, netpol.Spec.Ingress[0].Ports, 1)
	assert.Equal(t, intstr.FromInt32(9091), *netpol.Spec.Ingress[0].Ports[0].Port)

	assert.Equal(t, "http://cf-proxy-edge-config.tenant-a.svc.cluster.local:9091/config",
		render.ConfigEndpointURL(input.Gateway, "cluster.local", 9091))
}

// The default port renders no listen-address env, so upgrading does not roll
// every per-Gateway plane for a value the proxy already defaults to.
func TestConfigAPIPort_DefaultRendersNoEnv(t *testing.T) {
	t.Parallel()

	for _, port := range []int32{0, 8081} {
		input := testInput("edge")
		input.Defaults.ConfigAPIPort = port

		container := render.ProxyDeployment(input).Spec.Template.Spec.Containers[0]
		for _, env := range container.Env {
			assert.NotEqual(t, "PROXY_CONFIG_ADDR", env.Name, "port %d", port)
		}

		assert.Equal(t, int32(8081), container.Ports[0].ContainerPort, "port %d", port)
		assert.Equal(t, int32(8081), render.ConfigService(input).Spec.Ports[0].Port, "port %d", port)
		assert.Equal(t, "http://cf-proxy-edge-config.tenant-a.svc.cluster.local:8081/config",
			render.ConfigEndpointURL(input.Gateway, "cluster.local", port))
	}

	netpol := render.ProxyNetworkPolicy(render.NetworkPolicyInput{
		Gateway: testInput("edge").Gateway, ControllerNamespace: "cf-system",
	})
	assert.Equal(t, intstr.FromInt32(8081), *netpol.Spec.Ingress[0].Ports[0].Port)
}
