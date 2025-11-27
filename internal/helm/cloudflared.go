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
	InterfacePrefix  string // AWG interface name prefix (kernel auto-numbers: prefix0, prefix1, etc.)
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
	interfacePrefix := sidecar.InterfacePrefix
	if interfacePrefix == "" {
		interfacePrefix = "awg-cfd"
	}

	// Wrapper script that:
	// 1. Uses flock to atomically find and reserve interface number
	// 2. Writes the interface name to shared file for preStop
	// 3. Copies config with correct name and starts AWG
	wrapperScript := `#!/bin/sh
set -e
PREFIX="` + interfacePrefix + `"
IFACE_FILE="/run/awg/interface-name"
CONFIG_DIR="/run/awg"
LOCK_DIR="/tmp/awg-locks"

mkdir -p "$LOCK_DIR"

# Find and atomically reserve an interface number using flock
# This prevents TOCTOU race conditions when multiple pods start simultaneously
find_and_reserve_interface() {
  N=0
  while true; do
    LOCK_FILE="$LOCK_DIR/${PREFIX}${N}.lock"
    # Try to acquire exclusive lock (non-blocking)
    if (set -C; echo $$ > "$LOCK_FILE") 2>/dev/null; then
      # Successfully acquired lock, check if interface exists
      if ! ip link show "${PREFIX}${N}" >/dev/null 2>&1; then
        # Interface doesn't exist, we can use this number
        echo "${PREFIX}${N}"
        return 0
      fi
      # Interface exists, remove lock and try next
      rm -f "$LOCK_FILE"
    fi
    N=$((N + 1))
    # Safety limit to prevent infinite loop
    if [ "$N" -gt 100 ]; then
      echo "ERROR: Could not find available interface number" >&2
      return 1
    fi
  done
}

IFACE=$(find_and_reserve_interface)
if [ -z "$IFACE" ]; then
  exit 1
fi

# Write interface name for preStop hook
echo "$IFACE" > "$IFACE_FILE"
echo "Using AWG interface: $IFACE"

# Copy config with correct interface name
# AWG uses filename as interface name
for conf in /config/*.conf; do
  if [ -f "$conf" ]; then
    cp "$conf" "${CONFIG_DIR}/${IFACE}.conf"
    break
  fi
done

# Start AWG (awg-quick up exits after configuring interface)
/usr/bin/awg-quick up "${CONFIG_DIR}/${IFACE}.conf"

# Keep container running
exec sleep infinity
`

	// preStop script reads interface name from shared file
	preStopScript := `IFACE=$(cat /run/awg/interface-name 2>/dev/null || echo "")
if [ -n "$IFACE" ]; then
  /usr/bin/awg-quick down "/run/awg/${IFACE}.conf" 2>/dev/null || true
  ip link delete "$IFACE" 2>/dev/null || true
fi`

	return map[string]any{
		"initContainers": []any{
			map[string]any{
				"name":  "wait-for-awg",
				"image": "busybox:1.36",
				"command": []any{
					"sh", "-c", "echo 'Waiting for AWG...' && sleep 5 && echo 'Done'",
				},
				"securityContext": map[string]any{
					"runAsUser":    0,
					"runAsNonRoot": false,
				},
			},
		},
		"containers": []any{
			map[string]any{
				"name":            "amneziawg",
				"image":           "ghcr.io/zeozeozeo/amneziawg-client:latest",
				"imagePullPolicy": "IfNotPresent",
				"stdin":           true,
				"tty":             true,
				"command":         []any{"sh", "-c", wrapperScript},
				"securityContext": map[string]any{
					"privileged":   true,
					"runAsUser":    0,
					"runAsNonRoot": false,
				},
				"lifecycle": map[string]any{
					"preStop": map[string]any{
						"exec": map[string]any{
							"command": []any{"sh", "-c", preStopScript},
						},
					},
				},
				"volumeMounts": []any{
					map[string]any{
						"name":      "awg-config",
						"mountPath": "/config",
						"readOnly":  true,
					},
					map[string]any{
						"name":      "awg-runtime",
						"mountPath": "/run/awg",
					},
					map[string]any{
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
		"extraVolumes": []any{
			map[string]any{
				"name": "awg-config",
				"secret": map[string]any{
					"secretName": sidecar.ConfigSecretName,
				},
			},
			map[string]any{
				"name": "awg-runtime",
				"emptyDir": map[string]any{
					"medium": "Memory",
				},
			},
			map[string]any{
				"name": "tun-device",
				"hostPath": map[string]any{
					"path": "/dev/net/tun",
					"type": "CharDevice",
				},
			},
		},
	}
}
