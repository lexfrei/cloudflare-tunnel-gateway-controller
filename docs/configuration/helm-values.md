# Helm Values

This document provides an overview of the Helm chart configuration.
For the complete reference, see the
[Helm Chart README](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/blob/master/charts/cloudflare-tunnel-gateway-controller/README.md).

## Quick Reference

### Essential Values

```yaml
# Cloudflare Tunnel configuration
config:
  tunnelID: "550e8400-e29b-41d4-a716-446655440000"
  apiToken: "your-api-token"
  tunnelToken: "your-tunnel-token"
  # accountID: "auto-detected"  # Optional, auto-detected from API token
```

### Using Existing Secrets

For production deployments, use existing secrets instead of inline values:

```yaml
config:
  tunnelID: "550e8400-e29b-41d4-a716-446655440000"
  existingSecrets:
    apiToken:
      name: cloudflare-credentials
      key: api-token
    tunnelToken:
      name: cloudflare-tunnel-token
      key: tunnel-token
```

### Controller Configuration

```yaml
controller:
  replicas: 2

  resources:
    limits:
      memory: 128Mi
    requests:
      cpu: 100m
      memory: 64Mi

  # Controller flags
  extraArgs:
    - --log-level=debug
```

### cloudflared Configuration

```yaml
cloudflared:
  enabled: true  # Set to false to manage cloudflared externally
  replicas: 2

  resources:
    limits:
      memory: 256Mi
    requests:
      cpu: 100m
      memory: 128Mi
```

### High Availability

```yaml
controller:
  replicas: 2
  leaderElection:
    enabled: true

cloudflared:
  replicas: 2

podDisruptionBudget:
  enabled: true
  minAvailable: 1
```

### Prometheus Monitoring

```yaml
serviceMonitor:
  enabled: true
  interval: 30s
  labels:
    release: prometheus
```

## Common Configurations

### Minimal Production Setup

```yaml
config:
  tunnelID: "YOUR_TUNNEL_ID"
  existingSecrets:
    apiToken:
      name: cloudflare-credentials
      key: api-token
    tunnelToken:
      name: cloudflare-tunnel-token
      key: tunnel-token

controller:
  replicas: 2
  leaderElection:
    enabled: true
  resources:
    limits:
      memory: 128Mi
    requests:
      cpu: 100m
      memory: 64Mi

cloudflared:
  replicas: 2
  resources:
    limits:
      memory: 256Mi
    requests:
      cpu: 100m
      memory: 128Mi

serviceMonitor:
  enabled: true
```

### Development Setup

```yaml
config:
  tunnelID: "YOUR_TUNNEL_ID"
  apiToken: "YOUR_API_TOKEN"
  tunnelToken: "YOUR_TUNNEL_TOKEN"

controller:
  replicas: 1
  extraArgs:
    - --log-level=debug
    - --log-format=text

cloudflared:
  replicas: 1
```

### External cloudflared

When managing cloudflared separately (e.g., on edge nodes):

```yaml
config:
  tunnelID: "YOUR_TUNNEL_ID"
  existingSecrets:
    apiToken:
      name: cloudflare-credentials
      key: api-token

cloudflared:
  enabled: false  # Don't deploy cloudflared via Helm
```

### With AmneziaWG Sidecar

```yaml
config:
  tunnelID: "YOUR_TUNNEL_ID"
  existingSecrets:
    apiToken:
      name: cloudflare-credentials
      key: api-token
    tunnelToken:
      name: cloudflare-tunnel-token
      key: tunnel-token

cloudflared:
  awg:
    enabled: true
    secretName: awg-config
```

See [AmneziaWG Sidecar Guide](../guides/awg-sidecar.md) for details.

## Upgrading

When upgrading the Helm release:

```bash
helm upgrade cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller/chart \
  --namespace cloudflare-tunnel-system \
  --values values.yaml
```

!!! tip "Version Pinning"

    Pin to specific versions in production:

    ```bash
    helm upgrade cloudflare-tunnel-gateway-controller \
      oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller/chart \
      --version 0.8.0 \
      --namespace cloudflare-tunnel-system \
      --values values.yaml
    ```

## Full Reference

For the complete list of all available values with descriptions, see the
[Helm Chart README](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/blob/master/charts/cloudflare-tunnel-gateway-controller/README.md).
