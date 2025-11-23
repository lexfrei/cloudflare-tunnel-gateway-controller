package helm

// CloudflaredValues holds configuration for cloudflare-tunnel Helm chart.
type CloudflaredValues struct {
	TunnelToken  string
	Protocol     string
	ReplicaCount int
	Sidecar      *SidecarConfig
}

// SidecarConfig holds AWG sidecar configuration.
type SidecarConfig struct {
	ConfigSecretName string
}

// BuildValues converts CloudflaredValues to Helm values map.
func (v *CloudflaredValues) BuildValues() map[string]any {
	values := map[string]any{
		"cloudflare": map[string]any{
			"mode":        "remote",
			"tunnelToken": v.TunnelToken,
		},
		"replicaCount": v.ReplicaCount,
	}

	if v.Protocol != "" {
		values["protocol"] = v.Protocol
	}

	if v.Sidecar != nil {
		values["sidecar"] = buildSidecarValues(v.Sidecar)
	}

	return values
}

//nolint:funlen // sidecar config structure is verbose but readable
func buildSidecarValues(sidecar *SidecarConfig) map[string]any {
	return map[string]any{
		"initContainers": []map[string]any{
			{
				"name":  "wait-for-awg",
				"image": "busybox:1.36",
				"command": []string{
					"sh", "-c", "echo 'Waiting for AWG...' && sleep 5 && echo 'Done'",
				},
				"securityContext": map[string]any{
					"runAsUser":    0,
					"runAsNonRoot": false,
				},
			},
		},
		"containers": []map[string]any{
			{
				"name":            "amneziawg",
				"image":           "ghcr.io/zeozeozeo/amneziawg-client:latest",
				"imagePullPolicy": "IfNotPresent",
				"stdin":           true,
				"tty":             true,
				"securityContext": map[string]any{
					"privileged":   true,
					"runAsUser":    0,
					"runAsNonRoot": false,
				},
				"volumeMounts": []map[string]any{
					{
						"name":      "awg-config",
						"mountPath": "/config",
						"readOnly":  true,
					},
					{
						"name":      "tun-device",
						"mountPath": "/dev/net/tun",
					},
				},
				"resources": map[string]any{
					"requests": map[string]any{
						"cpu":    "10m",
						"memory": "32Mi",
					},
					"limits": map[string]any{
						"memory": "64Mi",
					},
				},
			},
		},
		"extraVolumes": []map[string]any{
			{
				"name": "awg-config",
				"secret": map[string]any{
					"secretName": sidecar.ConfigSecretName,
				},
			},
			{
				"name": "tun-device",
				"hostPath": map[string]any{
					"path": "/dev/net/tun",
					"type": "CharDevice",
				},
			},
		},
	}
}
