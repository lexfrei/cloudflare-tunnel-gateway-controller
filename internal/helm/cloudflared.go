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

//nolint:funlen,dupword // sidecar config structure is verbose but readable; shell script has nested loops
func buildSidecarValues(sidecar *SidecarConfig) map[string]any {
	interfacePrefix := sidecar.InterfacePrefix
	if interfacePrefix == "" {
		interfacePrefix = "awg-cfd"
	}

	// Wrapper script that:
	// 1. Uses flock to atomically find and reserve interface number
	// 2. Writes the interface name to shared file for preStop
	// 3. Copies config with correct interface name
	// 4. Starts AWG with DNS preservation (backup/restore resolv.conf)
	// 5. Keeps container running
	wrapperScript := `#!/bin/sh
set -e
PREFIX="` + interfacePrefix + `"
IFACE_FILE="/run/awg/interface-name"
CONFIG_DIR="/run/awg"
LOCK_DIR="/tmp/awg-locks"
RESOLV_BACKUP="/run/awg/resolv.conf.bak"

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

# Process AWG config: strip wg-quick directives that awg setconf doesn't understand
# Extracts Address for manual application, removes DNS/PostUp/PostDown/etc
process_awg_config() {
  src_conf="$1"
  dst_conf="$2"
  addr_file="$3"

  # Extract Address lines for later use
  grep -i "^Address" "$src_conf" | sed 's/^[Aa]ddress[[:space:]]*=[[:space:]]*//' > "$addr_file" || true

  # Copy config, stripping wg-quick specific directives
  # awg setconf only understands [Interface]/[Peer] sections with WireGuard-native options
  grep -vEi "^[[:space:]]*(Address|DNS|MTU|Table|PreUp|PostUp|PreDown|PostDown|SaveConfig)[[:space:]]*=" "$src_conf" > "$dst_conf" || true
}

# Apply interface addresses extracted from config
apply_addresses() {
  iface="$1"
  addr_file="$2"

  if [ -s "$addr_file" ]; then
    while IFS=',' read -r addr_list; do
      for addr in $addr_list; do
        addr=$(echo "$addr" | tr -d ' ')
        [ -n "$addr" ] && ip addr add "$addr" dev "$iface" 2>/dev/null || true
      done
    done < "$addr_file"
  fi
}

# Backup/restore resolv.conf to preserve cluster DNS
backup_resolv() {
  [ -f /etc/resolv.conf ] && cp /etc/resolv.conf "$RESOLV_BACKUP"
}

restore_resolv() {
  # Use cat > to handle symlinks (K8s mounts resolv.conf as symlink)
  [ -f "$RESOLV_BACKUP" ] && cat "$RESOLV_BACKUP" > /etc/resolv.conf 2>/dev/null || true
}

IFACE=$(find_and_reserve_interface)
if [ -z "$IFACE" ]; then
  exit 1
fi

# Write interface name for preStop hook
echo "$IFACE" > "$IFACE_FILE"
echo "Using AWG interface: $IFACE"

ADDR_FILE="${CONFIG_DIR}/${IFACE}.addr"

# Process config: extract addresses, strip wg-quick directives
for conf in /config/*.conf; do
  if [ -f "$conf" ]; then
    process_awg_config "$conf" "${CONFIG_DIR}/${IFACE}.conf" "$ADDR_FILE"
    break
  fi
done

# Start AWG with DNS protection
backup_resolv

# Create interface: try kernel module first, fall back to userspace
if ip link add dev "$IFACE" type amneziawg 2>/dev/null; then
  echo "Using kernel module"
  ip link set "$IFACE" up
  awg setconf "$IFACE" "${CONFIG_DIR}/${IFACE}.conf"
  apply_addresses "$IFACE" "$ADDR_FILE"
else
  echo "Kernel module unavailable, using userspace implementation"
  # Start amneziawg-go in background (creates tun device)
  amneziawg-go "$IFACE" &
  sleep 1
  # Bring interface up, apply config and addresses
  ip link set "$IFACE" up
  awg setconf "$IFACE" "${CONFIG_DIR}/${IFACE}.conf"
  apply_addresses "$IFACE" "$ADDR_FILE"
fi

# Set up routing based on AllowedIPs configuration
# awg setconf does NOT add routes (unlike wg-quick), so we must add them manually
# Full tunnel (0.0.0.0/0): replace default route, keep endpoint route via original GW
# Split tunnel (specific subnets): add routes for each AllowedIP subnet
setup_routing() {
  iface="$1"
  conf_file="$2"

  # Get AWG endpoint from config file (awg show may be empty before handshake)
  WG_ENDPOINT=$(grep -i "^Endpoint" "$conf_file" 2>/dev/null | head -1 | sed 's/^[Ee]ndpoint[[:space:]]*=[[:space:]]*//' | cut -d: -f1)

  # Resolve hostname to IP if needed (ip route requires IP address)
  if [ -n "$WG_ENDPOINT" ]; then
    if ! echo "$WG_ENDPOINT" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
      echo "Resolving endpoint hostname: $WG_ENDPOINT"
      WG_ENDPOINT_IP=$(getent hosts "$WG_ENDPOINT" | awk '{print $1}' | head -1)
      if [ -n "$WG_ENDPOINT_IP" ]; then
        echo "Resolved $WG_ENDPOINT -> $WG_ENDPOINT_IP"
        WG_ENDPOINT="$WG_ENDPOINT_IP"
      else
        echo "ERROR: Cannot resolve endpoint hostname $WG_ENDPOINT"
        return 1
      fi
    fi
  fi

  # Get original default gateway
  ORIG_GW=$(ip route | grep '^default via' | awk '{print $3}' | head -1)

  # Check if full tunnel mode (AllowedIPs contains 0.0.0.0/0)
  if grep -qi "^AllowedIPs.*0\.0\.0\.0/0" "$conf_file" 2>/dev/null; then
    FULL_TUNNEL="yes"
    echo "Full tunnel mode detected (AllowedIPs includes 0.0.0.0/0)"
  else
    FULL_TUNNEL="no"
    echo "Split tunnel mode detected (specific AllowedIPs)"
  fi

  echo "Routing setup: endpoint=$WG_ENDPOINT gateway=$ORIG_GW full_tunnel=$FULL_TUNNEL"

  if [ -n "$WG_ENDPOINT" ] && [ -n "$ORIG_GW" ]; then
    # Always add route to AWG endpoint via original gateway
    ip route add "$WG_ENDPOINT" via "$ORIG_GW" 2>/dev/null || true

    if [ "$FULL_TUNNEL" = "yes" ]; then
      # Full tunnel: replace default route to go through AWG
      ip route replace default dev "$iface"
      echo "Routing configured: default via $iface, $WG_ENDPOINT via $ORIG_GW"
    else
      # Split tunnel: add routes for each AllowedIP subnet (awg setconf doesn't do this)
      ALLOWED_IPS=$(grep -i "^AllowedIPs" "$conf_file" 2>/dev/null | sed 's/^[Aa]llowed[Ii][Pp]s[[:space:]]*=[[:space:]]*//')
      echo "Adding routes for AllowedIPs: $ALLOWED_IPS"
      echo "$ALLOWED_IPS" | tr ',' '\n' | while read -r subnet; do
        subnet=$(echo "$subnet" | tr -d ' ')
        [ -z "$subnet" ] && continue
        # Skip IPv6 subnets
        echo "$subnet" | grep -q ':' && continue
        # Skip 0.0.0.0/0 (handled above)
        [ "$subnet" = "0.0.0.0/0" ] && continue
        echo "Adding route: $subnet via $iface"
        ip route add "$subnet" dev "$iface" 2>/dev/null || true
      done
      echo "Routing configured: AllowedIPs via $iface, $WG_ENDPOINT via $ORIG_GW"
    fi
  else
    echo "WARNING: Could not configure routing (endpoint=$WG_ENDPOINT, gw=$ORIG_GW)"
  fi
}

# Use original config file to get Endpoint (before it was stripped)
for conf in /config/*.conf; do
  if [ -f "$conf" ]; then
    setup_routing "$IFACE" "$conf"
    break
  fi
done

restore_resolv

# Keep container running
exec sleep infinity
`

	// preStop script reads interface name from shared file and tears down interface
	preStopScript := `IFACE=$(cat /run/awg/interface-name 2>/dev/null || echo "")
if [ -n "$IFACE" ]; then
  ip link set "$IFACE" down 2>/dev/null || true
  ip link delete "$IFACE" 2>/dev/null || true
  # Kill userspace process if running
  pkill -f "amneziawg-go $IFACE" 2>/dev/null || true
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
