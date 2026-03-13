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
  oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system \
  --values values.yaml
```

!!! tip "Version Pinning"

    Pin to specific versions in production:

    ```bash
    helm upgrade cloudflare-tunnel-gateway-controller \
      oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
      --version 0.8.0 \
      --namespace cloudflare-tunnel-system \
      --values values.yaml
    ```

## L7 Proxy Configuration

The `proxy` section configures the v2 L7 reverse proxy deployment. When
enabled, the proxy runs in-process inside cloudflared and provides full Gateway
API HTTPRoute support (header matching, traffic splitting, filters).

### Core Settings

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.enabled` | bool | `false` | Enable the L7 proxy deployment |
| `proxy.replicas` | int | `2` | Number of proxy pod replicas |
| `proxy.image.repository` | string | `ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-proxy` | Proxy container image repository |
| `proxy.image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `proxy.image.tag` | string | `""` (appVersion) | Image tag override |
| `proxy.configAPIPort` | int | `8081` | Port where the controller pushes configuration |
| `proxy.proxyPort` | int | `8080` | Internal proxy port (traffic arrives through tunnel) |

### Tunnel Token

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.tunnelTokenSecretRef.name` | string | `""` | Name of the Secret containing the tunnel token (required when proxy is enabled) |
| `proxy.tunnelTokenSecretRef.key` | string | `"tunnel-token"` | Key in the Secret containing the tunnel token |

### Resources

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.resources.limits.cpu` | string | `500m` | CPU limit |
| `proxy.resources.limits.memory` | string | `512Mi` | Memory limit |
| `proxy.resources.requests.cpu` | string | `100m` | CPU request |
| `proxy.resources.requests.memory` | string | `128Mi` | Memory request |

### Security Contexts

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.podSecurityContext.runAsNonRoot` | bool | `true` | Require non-root user |
| `proxy.podSecurityContext.runAsUser` | int | `65534` | UID to run as (nobody) |
| `proxy.podSecurityContext.seccompProfile.type` | string | `RuntimeDefault` | Seccomp profile type |
| `proxy.securityContext.allowPrivilegeEscalation` | bool | `false` | Disallow privilege escalation |
| `proxy.securityContext.readOnlyRootFilesystem` | bool | `true` | Read-only root filesystem |

### Health Probes

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.healthProbes.startupProbe.enabled` | bool | `true` | Enable startup probe (gives tunnel time to connect) |
| `proxy.healthProbes.startupProbe.failureThreshold` | int | `30` | Startup probe failure threshold |
| `proxy.healthProbes.livenessProbe.enabled` | bool | `true` | Enable liveness probe |
| `proxy.healthProbes.livenessProbe.periodSeconds` | int | `20` | Liveness probe interval |
| `proxy.healthProbes.readinessProbe.enabled` | bool | `true` | Enable readiness probe (ready when config loaded) |
| `proxy.healthProbes.readinessProbe.periodSeconds` | int | `10` | Readiness probe interval |

### Networking and Service

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.service.annotations` | object | `{}` | Service annotations |
| `proxy.networkPolicy.enabled` | bool | `false` | Enable NetworkPolicy for proxy pods |
| `proxy.networkPolicy.ingress.from` | list | `[]` | Ingress source configuration |

### Scheduling

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.nodeSelector` | object | `{}` | Node selector for pod scheduling |
| `proxy.tolerations` | list | `[]` | Tolerations for pod scheduling |
| `proxy.affinity` | object | `{}` | Affinity rules for pod scheduling |
| `proxy.topologySpreadConstraints` | list | `[]` | Topology spread constraints for pod distribution |
| `proxy.podAnnotations` | object | `{}` | Annotations to add to proxy pods |
| `proxy.podLabels` | object | `{}` | Additional labels to add to proxy pods |

### Example

```yaml
proxy:
  enabled: true
  replicas: 3
  tunnelTokenSecretRef:
    name: cloudflare-tunnel-token
    key: tunnel-token
  resources:
    limits:
      cpu: 500m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi
  networkPolicy:
    enabled: true
```

For architecture details and usage examples, see the
[L7 Proxy Guide](../guides/l7-proxy.md).

## Full Reference

For the complete list of all available values with descriptions, see the
[Helm Chart README](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/blob/master/charts/cloudflare-tunnel-gateway-controller/README.md).
