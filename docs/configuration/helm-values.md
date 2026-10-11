# Helm Values

This document provides an overview of the Helm chart configuration. For the complete reference (every value with default and description), see the [Helm Chart README](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/blob/master/charts/cloudflare-tunnel-gateway-controller/README.md) — that file is generated from `values.yaml` via `helm-docs` and is always in sync with the chart.

## Quick Reference

### Essential Values

The chart deploys both the controller and the in-process L7 proxy. The minimum viable values file looks like this:

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

### Controller NetworkPolicy

These values shape the controller pod's own policy. The proxy's policy has its own keys under [Networking and Service](#networking-and-service).

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `networkPolicy.enabled` | bool | `false` | Render a NetworkPolicy for the controller pods: ingress to the metrics and health ports from the sources in `networkPolicy.ingress.from`, egress to DNS, the Kubernetes API ports, Cloudflare on 443 and the proxies' config-API port |
| `networkPolicy.ingress.from` | list | `[]` | Sources admitted to the metrics and health ports; empty admits no in-cluster source. Kubelet probes come from the node, which most CNIs admit regardless of policy; on a CNI that enforces policy on node-to-pod traffic, list the node source here too or the probes fail |
| `networkPolicy.kubernetesApiIpBlocks` | list | `[]` | Destination CIDRs for the Kubernetes API egress rule (TCP 443 and 6443); empty permits those ports to every destination, which leaves the Cloudflare rule without effect on 443 |
| `networkPolicy.cloudflareIpRanges` | object | Cloudflare's published ranges | `ipv4` and `ipv6` CIDR lists for the Cloudflare egress rule, shared with the proxy's edge-egress rule; emptying both stops the render while `networkPolicy.enabled` is on, or while `proxy.networkPolicy.egressRestricted` is on with the proxy policy rendered |

See [Egress Requirements](../reference/security.md#egress-requirements) for what a CIDR peer does and does not match.

### High Availability

```yaml
replicaCount: 2

leaderElection:
  enabled: true

proxy:
  replicas: 2
  tunnelTokenSecretRef:
    name: cloudflare-tunnel-token
  # Budget for the proxy pods, which carry the traffic.
  podDisruptionBudget:
    enabled: true
    minAvailable: 1

# Budget for the controller pods only.
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

The `proxy` section configures the in-process L7 reverse proxy. The proxy embeds cloudflared transport and is the only data plane — the chart always renders the proxy Deployment, Service, and headless Service. `proxy.tunnelTokenSecretRef.name` is **required**: the chart's `required` check fails install otherwise.

### Core Settings

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.replicas` | int | `2` | Number of proxy pod replicas |
| `proxy.image.repository` | string | `ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-proxy` | Proxy container image repository |
| `proxy.image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `proxy.image.tag` | string | `""` (appVersion) | Image tag override |
| `proxy.image.digest` | string | `""` | Digest used instead of `tag` when set; also the default image for per-Gateway data planes ([Container Image Verification](../reference/security.md#container-image-verification)) |
| `proxy.configAPIPort` | int | `8081` | Port where the controller pushes configuration, for the shared plane and every per-Gateway plane |
| `proxy.configAPITLS.enabled` | bool | `true` | Serve the config API (and the probes and `/metrics` that share its port) over TLS with certificates the controller issues from its own runtime-generated CA; `false` keeps plain HTTP ([Config API TLS](../reference/security.md#config-api-tls)) |
| `proxy.proxyPort` | int | `8080` | Internal proxy port (tunnel traffic arrives here) |
| `proxy.allowXOriginalHost` | bool | `false` | Trust the client-supplied `X-Original-Host` header as the routing key and backend `Host`, and `X-Original-Proto` / `X-Original-Port` as the request scheme and port. Test deployments only — leaving it on in production lets a client be served by another hostname's backend ([details](../guides/l7-proxy.md)). Also applies to per-Gateway data planes |

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
| `proxy.serviceAccount.create` | bool | `false` | Create a dedicated ServiceAccount for the proxy pods |
| `proxy.serviceAccount.name` | string | `""` | ServiceAccount to run the proxy on: the created account's name (defaults to the proxy fullname) or, without `create`, an existing one. Empty with `create` off keeps the namespace's default ServiceAccount |
| `proxy.serviceAccount.annotations` | object | `{}` | Annotations on the created ServiceAccount |

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
| `proxy.websocket.handshakeTimeout` | string | `""` (proxy default 30s) | Go-duration cap on waiting for the backend's `101 Switching Protocols`; when the backend refuses the upgrade instead, also the longest the refusal body may go without a byte |
| `proxy.websocket.idleTimeout` | string | `""` (proxy default 1h) | Go-duration bound on an established session that carries no bytes in either direction; any traffic resets it |

A duration written as zero, such as `"0s"` or `"0h0m0s"`, fails schema validation for all three: the proxy only honours a positive value and would otherwise fall back to its default without saying so.

### Request Mirror

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.mirror.maxInFlight` | int | `0` (proxy default 64) | Mirror dispatches each `RequestMirror` filter may keep in flight before it drops further copies. Each holds its buffered body, up to 1 MiB. Also passed to the controller as `--mirror-max-in-flight`, so per-Gateway data planes get the same limit |

### Networking and Service

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `proxy.service.annotations` | object | `{}` | Service annotations |
| `proxy.metrics.enabled` | bool | `true` | Expose request-level proxy metrics on the config-API port |
| `proxy.networkPolicy.enabled` | bool | `true` | Render the proxy NetworkPolicy (ingress-only; locks the config-API port to the controller pod) |
| `proxy.networkPolicy.egressRestricted` | bool | `false` | Also restrict egress to DNS + the Cloudflare edge + cluster services |
| `proxy.networkPolicy.ingress.from` | list | `[]` | Extra namespaces/pods allowed to reach the config-API port, admitted alongside the controller pod |
| `proxy.networkPolicy.monitoringNamespaceSelector` | object | `{}` | LabelSelector for namespaces additionally allowed to reach the per-Gateway proxies' config-API/metrics port |
| `proxy.authTokenSecretRef.name` | string | `""` | Secret name for the controller→proxy config-API Bearer token; leave empty to let the controller generate and manage the token automatically at startup (the config API is always authenticated) |
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
| `proxy.podDisruptionBudget.enabled` | bool | `false` | Render a PodDisruptionBudget for the proxy pods. The top-level `podDisruptionBudget` covers only the controller |
| `proxy.podDisruptionBudget.minAvailable` | int or percentage | `1` | Minimum available proxy pods; set to `null` when using `maxUnavailable` |
| `proxy.podDisruptionBudget.maxUnavailable` | int or percentage | `null` | Maximum unavailable proxy pods; the render fails if both bounds are set, zero included |
| `proxy.podDisruptionBudget.unhealthyPodEvictionPolicy` | string | `IfHealthyBudget` | `IfHealthyBudget` or `AlwaysAllow` |

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
  admissionPolicy: true     # false keeps only the controller-side layer
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
