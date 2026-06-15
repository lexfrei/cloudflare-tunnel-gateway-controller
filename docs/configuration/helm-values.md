# Helm Values

This document provides an overview of the Helm chart configuration. For the complete reference (every value with default and description), see the [Helm Chart README](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/blob/master/charts/cloudflare-tunnel-gateway-controller/README.md) — that file is generated from `values.yaml` via `helm-docs` and is always in sync with the chart.

## Quick Reference

### Essential Values

The v3 chart deploys both the controller and the in-process L7 proxy. The minimum viable values file looks like this:

```yaml
gatewayClassConfig:
  create: true
  tunnelID: "550e8400-e29b-41d4-a716-446655440000"
  cloudflareCredentialsSecretRef:
    name: cloudflare-credentials

proxy:
  tunnelTokenSecretRef:
    name: cloudflare-tunnel-token
```

The `cloudflare-credentials` Secret must contain an `api-token` key; the `cloudflare-tunnel-token` Secret must contain a `tunnel-token` key. See [GatewayClassConfig](gatewayclassconfig.md) for the full credential layout.

### Controller Configuration

The controller binary itself is configured via top-level chart values:

```yaml
replicaCount: 2

resources:
  limits:
    cpu: 200m
    memory: 256Mi
  requests:
    cpu: 100m
    memory: 128Mi

controller:
  logLevel: info       # debug | info | warn | error
  logFormat: json      # json | text
  gatewayClassName: cloudflare-tunnel
  controllerName: cf.k8s.lex.la/tunnel-controller
  # clusterDomain: ""  # Auto-detected from /etc/resolv.conf when empty

leaderElection:
  enabled: true        # Required when replicaCount > 1
  leaseName: cloudflare-tunnel-gateway-controller-leader
```

### High Availability

```yaml
replicaCount: 2

leaderElection:
  enabled: true

proxy:
  replicas: 2
  tunnelTokenSecretRef:
    name: cloudflare-tunnel-token

podDisruptionBudget:
  enabled: true
  minAvailable: 1
```

### Prometheus Monitoring

```yaml
serviceMonitor:
  enabled: true       # opt-in (default false); when true creates two ServiceMonitors: one for the controller (Prometheus /metrics on port 8080) and one for the proxy (config-API /metrics on port 8081, requires proxy.metrics.enabled)
  interval: 30s
  labels:
    prometheus: kube-prometheus
```

## L7 Proxy Configuration

The `proxy` section configures the in-process L7 reverse proxy. The proxy embeds cloudflared transport and is the only data plane in v3 — the chart always renders the proxy Deployment, Service, and headless Service. `proxy.tunnelTokenSecretRef.name` is **required**: the chart's `required` check fails install otherwise.

### Core Settings

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.replicas` | int | `2` | Number of proxy pod replicas |
| `proxy.image.repository` | string | `ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-proxy` | Proxy container image repository |
| `proxy.image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `proxy.image.tag` | string | `""` (appVersion) | Image tag override |
| `proxy.configAPIPort` | int | `8081` | Port where the controller pushes configuration |
| `proxy.proxyPort` | int | `8080` | Internal proxy port (tunnel traffic arrives here) |

### Tunnel Token (required)

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.tunnelTokenSecretRef.name` | string | `""` | Name of the Secret containing the tunnel token (REQUIRED) |
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
| `proxy.healthProbes.startupProbe.enabled` | bool | `true` | Enable startup probe (gives the tunnel time to connect) |
| `proxy.healthProbes.startupProbe.failureThreshold` | int | `30` | Startup probe failure threshold |
| `proxy.healthProbes.livenessProbe.enabled` | bool | `true` | Enable liveness probe |
| `proxy.healthProbes.livenessProbe.periodSeconds` | int | `20` | Liveness probe interval |
| `proxy.healthProbes.readinessProbe.enabled` | bool | `true` | Enable readiness probe (ready when config is loaded and, in tunnel mode, the tunnel has connected to the edge) |
| `proxy.healthProbes.readinessProbe.periodSeconds` | int | `10` | Readiness probe interval |

### Access Log

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.accessLog.enabled` | bool | `false` | Enable per-request structured JSON logging |
| `proxy.accessLog.samplingRate` | float | `1` | Fraction of non-5xx requests to log when enabled, in `[0, 1]` |
| `proxy.accessLog.stripQuery` | bool | `false` | Strip the request URL query string from log lines |

### WebSocket Timeouts

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.websocket.dialTimeout` | string | `""` (proxy default 30s) | Go-duration cap on the backend dial during the WebSocket upgrade |
| `proxy.websocket.handshakeTimeout` | string | `""` (proxy default 30s) | Go-duration cap on waiting for the backend's `101 Switching Protocols` |

### Networking and Service

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.service.annotations` | object | `{}` | Service annotations |
| `proxy.metrics.enabled` | bool | `true` | Expose request-level proxy metrics on the config-API port |
| `proxy.networkPolicy.enabled` | bool | `true` | Render the proxy NetworkPolicy (ingress-only; locks the config-API port to the controller namespace) |
| `proxy.networkPolicy.egressRestricted` | bool | `false` | Also restrict egress to DNS + the Cloudflare edge + cluster services |
| `proxy.networkPolicy.ingress.from` | list | `[]` | Extra namespaces/pods allowed to reach the config-API port, added to the controller namespace |
| `proxy.networkPolicy.monitoringNamespaceSelector` | object | `{}` | LabelSelector for namespaces additionally allowed to reach the per-Gateway proxies' config-API/metrics port |
| `proxy.authTokenSecretRef.name` | string | `""` | Secret name for the controller→proxy config-API Bearer token |
| `proxy.authTokenSecretRef.key` | string | `"auth-token"` | Key in the auth-token Secret |

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
  replicas: 3
  tunnelTokenSecretRef:
    name: cloudflare-tunnel-token
  resources:
    limits:
      cpu: 500m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi
  networkPolicy:
    enabled: true
  accessLog:
    enabled: true
    samplingRate: 0.1
```

For architecture details, see the [L7 Proxy Guide](../guides/l7-proxy.md).

### Metrics

```yaml
proxy:
  metrics:
    enabled: true   # /metrics on the config API port; also exposes cloudflared connector metrics
```

### Graceful Drain

```yaml
proxy:
  gracePeriodSeconds: 30   # connector drain window; pod terminationGracePeriodSeconds = this + 15
```

## Multi-Tenancy

```yaml
# Per-namespace hostname ownership, enforced twice (admission + controller).
hostnameOwnershipPolicy:
  enabled: false
  labelKey: cf.k8s.lex.la/hostname-suffix
  namespaceSelector: {}     # empty polices EVERY namespace — scope deliberately
  admissionPolicy: true     # set false on clusters older than Kubernetes 1.30
```

See the [Multi-Tenancy guide](../guides/multi-tenancy.md) for the namespace-label convention and fail-closed semantics, and the [Per-Gateway Isolation guide](../guides/per-gateway-isolation.md) for dedicated data planes (configured via the `GatewayConfig` CRD, not Helm values).

## Upgrading

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
      --version 1.0.0 \
      --namespace cloudflare-tunnel-system \
      --values values.yaml
    ```

Upgrading from a v2.x chart requires the [v2 → v3 migration steps](../upgrading/v2-to-v3.md).

## Full Reference

For the complete list of all available values with descriptions, see the [Helm Chart README](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/blob/master/charts/cloudflare-tunnel-gateway-controller/README.md).
