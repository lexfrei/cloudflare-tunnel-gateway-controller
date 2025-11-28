package helm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCloudflaredValues_BuildValues_Basic(t *testing.T) {
	t.Parallel()

	values := &CloudflaredValues{
		TunnelToken:  "test-token",
		ReplicaCount: 2,
	}

	result := values.BuildValues()

	require.NotNil(t, result)

	// Check cloudflare section
	cloudflare, ok := result["cloudflare"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "remote", cloudflare["mode"])
	assert.Equal(t, "test-token", cloudflare["tunnelToken"])

	// Check replica count
	assert.Equal(t, 2, result["replicaCount"])

	// No protocol should be set
	_, hasProtocol := result["protocol"]
	assert.False(t, hasProtocol)

	// No sidecar should be set
	_, hasSidecar := result["sidecar"]
	assert.False(t, hasSidecar)
}

func TestCloudflaredValues_BuildValues_WithProtocol(t *testing.T) {
	t.Parallel()

	values := &CloudflaredValues{
		TunnelToken:  "test-token",
		Protocol:     "quic",
		ReplicaCount: 1,
	}

	result := values.BuildValues()

	assert.Equal(t, "quic", result["protocol"])
}

func TestCloudflaredValues_BuildValues_WithSidecar(t *testing.T) {
	t.Parallel()

	values := &CloudflaredValues{
		TunnelToken:  "test-token",
		ReplicaCount: 1,
		Sidecar: &SidecarConfig{
			ConfigSecretName: "awg-config-secret",
			InterfacePrefix:  "custom-prefix",
		},
	}

	result := values.BuildValues()

	sidecar, ok := result["sidecar"].(map[string]any)
	require.True(t, ok)
	require.NotNil(t, sidecar)

	// Check that sidecar has expected structure
	containers, ok := sidecar["containers"].([]any)
	require.True(t, ok)
	require.Len(t, containers, 1)

	container, ok := containers[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "amneziawg", container["name"])
	assert.Equal(t, "ghcr.io/zeozeozeo/amneziawg-client:latest", container["image"])
}

func TestCloudflaredValues_BuildValues_WithSidecarDefaultPrefix(t *testing.T) {
	t.Parallel()

	values := &CloudflaredValues{
		TunnelToken:  "test-token",
		ReplicaCount: 1,
		Sidecar: &SidecarConfig{
			ConfigSecretName: "awg-config-secret",
			// InterfacePrefix not set - should default to "awg-cfd"
		},
	}

	result := values.BuildValues()

	sidecar, ok := result["sidecar"].(map[string]any)
	require.True(t, ok)

	// Verify sidecar structure
	containers, ok := sidecar["containers"].([]any)
	require.True(t, ok)
	require.Len(t, containers, 1)

	// Verify the wrapper script contains default prefix
	container, ok := containers[0].(map[string]any)
	require.True(t, ok)

	command, ok := container["command"].([]any)
	require.True(t, ok)
	require.Len(t, command, 3)

	script, ok := command[2].(string)
	require.True(t, ok)
	assert.Contains(t, script, "awg-cfd")
}

func TestBuildSidecarValues(t *testing.T) {
	t.Parallel()

	sidecar := &SidecarConfig{
		ConfigSecretName: "my-awg-secret",
		InterfacePrefix:  "test-iface",
	}

	result := buildSidecarValues(sidecar)

	// Check initContainers
	initContainers, ok := result["initContainers"].([]any)
	require.True(t, ok)
	require.Len(t, initContainers, 1)

	initContainer := initContainers[0].(map[string]any)
	assert.Equal(t, "wait-for-awg", initContainer["name"])
	assert.Equal(t, "busybox:1.36", initContainer["image"])

	// Check containers
	containers, ok := result["containers"].([]any)
	require.True(t, ok)
	require.Len(t, containers, 1)

	container := containers[0].(map[string]any)
	assert.Equal(t, "amneziawg", container["name"])
	assert.Equal(t, "ghcr.io/zeozeozeo/amneziawg-client:latest", container["image"])
	assert.Equal(t, "IfNotPresent", container["imagePullPolicy"])
	assert.True(t, container["stdin"].(bool))
	assert.True(t, container["tty"].(bool))

	// Check security context
	secCtx := container["securityContext"].(map[string]any)
	assert.True(t, secCtx["privileged"].(bool))
	assert.Equal(t, 0, secCtx["runAsUser"])
	assert.False(t, secCtx["runAsNonRoot"].(bool))

	// Check lifecycle
	lifecycle := container["lifecycle"].(map[string]any)
	preStop := lifecycle["preStop"].(map[string]any)
	exec := preStop["exec"].(map[string]any)
	preStopCmd := exec["command"].([]any)
	require.Len(t, preStopCmd, 3)
	assert.Equal(t, "sh", preStopCmd[0])
	assert.Equal(t, "-c", preStopCmd[1])

	// Check volume mounts
	volumeMounts := container["volumeMounts"].([]any)
	require.Len(t, volumeMounts, 3)

	awgConfig := volumeMounts[0].(map[string]any)
	assert.Equal(t, "awg-config", awgConfig["name"])
	assert.Equal(t, "/config", awgConfig["mountPath"])
	assert.True(t, awgConfig["readOnly"].(bool))

	awgRuntime := volumeMounts[1].(map[string]any)
	assert.Equal(t, "awg-runtime", awgRuntime["name"])
	assert.Equal(t, "/run/awg", awgRuntime["mountPath"])

	tunDevice := volumeMounts[2].(map[string]any)
	assert.Equal(t, "tun-device", tunDevice["name"])
	assert.Equal(t, "/dev/net/tun", tunDevice["mountPath"])

	// Check resources
	resources := container["resources"].(map[string]any)
	requests := resources["requests"].(map[string]any)
	assert.Equal(t, "10m", requests["cpu"])
	assert.Equal(t, "32Mi", requests["memory"])
	limits := resources["limits"].(map[string]any)
	assert.Equal(t, "64Mi", limits["memory"])

	// Check extraVolumes
	extraVolumes, ok := result["extraVolumes"].([]any)
	require.True(t, ok)
	require.Len(t, extraVolumes, 3)

	awgConfigVol := extraVolumes[0].(map[string]any)
	assert.Equal(t, "awg-config", awgConfigVol["name"])
	secret := awgConfigVol["secret"].(map[string]any)
	assert.Equal(t, "my-awg-secret", secret["secretName"])

	awgRuntimeVol := extraVolumes[1].(map[string]any)
	assert.Equal(t, "awg-runtime", awgRuntimeVol["name"])
	emptyDir := awgRuntimeVol["emptyDir"].(map[string]any)
	assert.Equal(t, "Memory", emptyDir["medium"])

	tunDeviceVol := extraVolumes[2].(map[string]any)
	assert.Equal(t, "tun-device", tunDeviceVol["name"])
	hostPath := tunDeviceVol["hostPath"].(map[string]any)
	assert.Equal(t, "/dev/net/tun", hostPath["path"])
	assert.Equal(t, "CharDevice", hostPath["type"])
}

func TestBuildSidecarValues_WrapperScriptContent(t *testing.T) {
	t.Parallel()

	sidecar := &SidecarConfig{
		ConfigSecretName: "awg-secret",
		InterfacePrefix:  "my-iface",
	}

	result := buildSidecarValues(sidecar)

	containers := result["containers"].([]any)
	container := containers[0].(map[string]any)
	command := container["command"].([]any)
	script := command[2].(string)

	// Verify key parts of wrapper script
	assert.Contains(t, script, "#!/bin/sh")
	assert.Contains(t, script, "set -e")
	assert.Contains(t, script, "my-iface")
	assert.Contains(t, script, "find_and_reserve_interface")
	assert.Contains(t, script, "ip link add dev")
	assert.Contains(t, script, "awg setconf")
	assert.Contains(t, script, "setup_routing")
	assert.Contains(t, script, "exec sleep infinity")
}

func TestBuildSidecarValues_PreStopScriptContent(t *testing.T) {
	t.Parallel()

	sidecar := &SidecarConfig{
		ConfigSecretName: "awg-secret",
		InterfacePrefix:  "my-iface",
	}

	result := buildSidecarValues(sidecar)

	containers := result["containers"].([]any)
	container := containers[0].(map[string]any)
	lifecycle := container["lifecycle"].(map[string]any)
	preStop := lifecycle["preStop"].(map[string]any)
	exec := preStop["exec"].(map[string]any)
	command := exec["command"].([]any)
	script := command[2].(string)

	// Verify preStop script content
	assert.Contains(t, script, "/run/awg/interface-name")
	assert.Contains(t, script, "ip link set")
	assert.Contains(t, script, "ip link delete")
	assert.Contains(t, script, "pkill")
	assert.Contains(t, script, "amneziawg-go")
}

func TestSidecarConfig_EmptyPrefix(t *testing.T) {
	t.Parallel()

	sidecar := &SidecarConfig{
		ConfigSecretName: "test-secret",
		InterfacePrefix:  "", // Empty, should default to "awg-cfd"
	}

	result := buildSidecarValues(sidecar)

	containers := result["containers"].([]any)
	container := containers[0].(map[string]any)
	command := container["command"].([]any)
	script := command[2].(string)

	// Should use default prefix
	assert.Contains(t, script, "awg-cfd")
}

func TestCloudflaredValues_EmptyValues(t *testing.T) {
	t.Parallel()

	values := &CloudflaredValues{
		TunnelToken:  "",
		ReplicaCount: 0,
	}

	result := values.BuildValues()

	cloudflare := result["cloudflare"].(map[string]any)
	assert.Empty(t, cloudflare["tunnelToken"])
	assert.Equal(t, "remote", cloudflare["mode"])
	assert.Equal(t, 0, result["replicaCount"])
}
