# Configuration Reference

This document describes all configuration options for the controller binary. For Helm chart values, see the [Helm chart documentation](../charts/cloudflare-tunnel-gateway-controller/README.md).

## Command Line Flags

| Flag | Environment Variable | Default | Description |
|------|---------------------|---------|-------------|
| `--gateway-class-name` | `CF_GATEWAY_CLASS_NAME` | `cloudflare-tunnel` | GatewayClass name to watch |
| `--controller-name` | `CF_CONTROLLER_NAME` | `cf.k8s.lex.la/tunnel-controller` | Controller name for GatewayClass |
| `--cluster-domain` | `CF_CLUSTER_DOMAIN` | (auto-detect) | Kubernetes cluster domain (fallback: `cluster.local`) |
| `--metrics-addr` | `CF_METRICS_ADDR` | `:8080` | Metrics endpoint address |
| `--health-addr` | `CF_HEALTH_ADDR` | `:8081` | Health probe endpoint address |
| `--log-level` | `CF_LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |
| `--log-format` | `CF_LOG_FORMAT` | `json` | Log format (json, text) |
| `--leader-elect` | `CF_LEADER_ELECT` | `false` | Enable leader election for HA |
| `--leader-election-namespace` | `CF_LEADER_ELECTION_NAMESPACE` | | Namespace for leader election lease |
| `--leader-election-name` | `CF_LEADER_ELECTION_NAME` | `cloudflare-tunnel-gateway-controller-leader` | Leader election lease name |

## Environment Variables

All flags can be set via environment variables with the `CF_` prefix. Dashes in flag names are replaced with underscores.

Examples:

- `--gateway-class-name` → `CF_GATEWAY_CLASS_NAME`
- `--log-level` → `CF_LOG_LEVEL`

## Cluster Domain Auto-Detection

The controller automatically detects the Kubernetes cluster domain from `/etc/resolv.conf` search domains. If detection fails, it falls back to `cluster.local`.

To override auto-detection, set `--cluster-domain` or `CF_CLUSTER_DOMAIN`.

## Leader Election

For high availability deployments with multiple controller replicas, enable leader election:

```bash
--leader-elect=true
--leader-election-namespace=cloudflare-tunnel-system
```

Only the leader processes events; other replicas remain on standby.
