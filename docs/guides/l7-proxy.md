# L7 Proxy Setup

The L7 proxy runs cloudflared tunnel transport with a built-in reverse proxy that implements full Gateway API HTTPRoute routing in-process, removing the limitations of the Cloudflare Tunnel ingress API.

## Architecture

```mermaid
flowchart TB
    subgraph Internet
        USER[Client]
    end

    subgraph Cloudflare["Cloudflare Edge"]
        EDGE[Edge Network]
    end

    subgraph Kubernetes["Kubernetes Cluster"]
        subgraph Controller["Control Plane"]
            CTRL[Controller]
        end

        subgraph Proxy["Data Plane (N replicas)"]
            CFD[cloudflared transport]
            L7[L7 Reverse Proxy]
            CAPI[Config API]
        end

        SVC[Backend Services]
        HR[HTTPRoute]
        GW[Gateway]
    end

    USER -->|HTTPS| EDGE
    EDGE -->|QUIC tunnel| CFD
    CFD --> L7
    L7 -->|route| SVC

    HR -->|watch| CTRL
    GW -->|watch| CTRL
    CTRL -->|PUT /config| CAPI
    CAPI -->|atomic swap| L7
```

## Prerequisites

- Kubernetes 1.25+
- Gateway API CRDs installed
- Cloudflare Tunnel created with a valid token
- Helm 3.x

## Installation

### 1. Create tunnel token Secret

```bash
kubectl create secret generic tunnel-token \
  --from-literal=tunnel-token=YOUR_BASE64_TUNNEL_TOKEN \
  --namespace cloudflare-tunnel-system
```

### 2. Configure the proxy in Helm values

The L7 proxy is always rendered by the v3 chart. Point it at the tunnel-token Secret and pick a replica count:

```yaml
proxy:
  replicas: 2
  tunnelTokenSecretRef:
    name: tunnel-token
```

### 3. Install or upgrade

```bash
helm upgrade --install cloudflare-tunnel \
  oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system \
  --create-namespace \
  --values values.yaml
```

## Features Enabled by L7 Proxy

The L7 proxy enables the following Gateway API features that are not available with only the Cloudflare Tunnel API:

- Exact path matching
- Header matching
- Query parameter matching
- HTTP method matching
- Request header modification
- Response header modification
- URL rewriting
- Request redirect
- Request mirroring
- Weighted traffic splitting
- Regex path matching
- Per-route timeouts

## Configuration

The controller automatically discovers proxy pod endpoints via the headless Service and pushes routing configuration whenever HTTPRoute resources change.

### Environment Variables

The proxy binary accepts the following environment variables:

| Variable | Default | Description |
| --- | --- | --- |
| `TUNNEL_TOKEN` | Required for tunnel mode; omit for standalone/dev mode | Cloudflare tunnel token (base64) |
| `PROXY_CONFIG_ADDR` | `:8081` | Config API listen address |
| `PROXY_ADDR` | `:8080` | Proxy listen address |
| `PROXY_AUTH_TOKEN` | `""` (empty, no auth) | Bearer token for config push API authentication. If unset, the API is unauthenticated -- this is the binary's own default for standalone/manual use; the Helm chart always sets it (see below). |
| `PROXY_METRICS_ENABLED` | `true` | Expose the data-plane Prometheus metrics at `/metrics` on the config API port. Set `false`/`0` to disable. |
| `PROXY_GRACE_PERIOD` | `30s` | Connector drain window on shutdown (Go duration, capped at 3m): the proxy unregisters from the edge and gives in-flight requests this long before exiting. |
| `PROXY_TUNNEL_PROTOCOL` | `auto` | Edge transport: `auto`, `http2`, or `quic`. gRPC needs `http2` (QUIC drops trailers); `auto` is upgraded to `http2` by the proxy. |
| `PROXY_TUNNEL_PROTOCOL_WAIT` | `0` (no wait) | In `auto` mode, how long (Go duration) to wait for the first pushed config before serving, so the protocol is chosen from real routes. |
| `PROXY_WS_DIAL_TIMEOUT` | `""` (proxy default 30s) | Go-duration cap on the backend dial during a WebSocket upgrade. |
| `PROXY_WS_HANDSHAKE_TIMEOUT` | `""` (proxy default 30s) | Go-duration cap on waiting for the backend's `101 Switching Protocols`. |
| `PROXY_ACCESS_LOG_ENABLED` | `false` | Enable per-request structured JSON access logging on stdout. |
| `PROXY_ACCESS_LOG_SAMPLING_RATE` | `1` | Fraction of non-5xx requests to log when access logging is enabled, in `[0, 1]` (5xx are always logged). |
| `PROXY_ACCESS_LOG_STRIP_QUERY` | `false` | Strip the request URL query string from access-log lines. |
| `PROXY_TRACING_ENABLED` | `false` | Enable OpenTelemetry tracing of proxied requests. |
| `PROXY_TRACING_ENDPOINT` | `""` | OTLP exporter endpoint for traces (when tracing is enabled). |
| `PROXY_TRACING_SAMPLE_RATE` | `1` | Trace sampling fraction in `[0, 1]` (when tracing is enabled). |

### Config API Authentication

The config API is always authenticated when deployed via the chart: leave `proxy.authTokenSecretRef.name` empty (the default) and the controller itself generates a random token into a Secret named `<release>-proxy-auth-token` as one of its first startup actions, uses it directly for its own push auth, and the proxy reads the same Secret via a pod-level `secretKeyRef`. The token is created once and reused on every restart, never rotated -- so neither an upgrade nor a controller restart rolls the proxy on its own. Set `proxy.authTokenSecretRef.name` to point at your own Secret instead, for example to manage rotation externally -- the controller resolves it the same way (directly via the API, never a `secretKeyRef` on its own pod) and never creates or modifies it: a missing bring-your-own Secret is a configuration error, not something silently papered over.

On a brand-new install, the proxy Deployment's pod template references the generated Secret before the controller has had a chance to create it -- the affected proxy pod(s) sit briefly in `CreateContainerConfigError` until the controller finishes starting, and kubelet's normal retry picks up the Secret once it exists. This is a one-time, self-resolving startup race, not a failure to act on.

!!! note "Upgrading from a release with no auth token"

    If you were pushing config to the proxy directly (bypassing the controller) with no `Authorization` header, that stops working after upgrading to a chart version with this default: read the generated token with `kubectl get secret <release>-proxy-auth-token -o jsonpath='{.data.auth-token}' | base64 -d` and send it as `Authorization: Bearer <token>`. During the rollout itself there is a brief window where the new, already-authenticated proxy pod rejects pushes from an old controller pod that has not yet rolled (401), and the reverse (an old, unauthenticated proxy pod accepting an authenticated push, since it never checks the header). Both resolve on their own once the rolling update finishes and both sides are on the new pod template -- no manual step is needed.

The generated Secret is created by the controller directly, not rendered by Helm, so `helm uninstall` leaves it behind (see [Uninstalling](../getting-started/installation.md#uninstalling)). A Secret at this name with a missing or empty `auth-token` key fails the controller closed at startup instead of running with an unusable token; see [Config API Auth Secret Missing or Broken](../operations/troubleshooting.md#config-api-auth-secret-missing-or-broken) for the exact error and the recovery command.

### Health Endpoints

| Endpoint | Port | Description |
| --- | --- | --- |
| `/healthz` | Config API | Liveness check |
| `/readyz` | Config API | Readiness: config loaded at least once AND, in tunnel mode, the tunnel has connected to the Cloudflare edge (standalone mode latches the tunnel condition at startup) |

## Example HTTPRoute

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: advanced-routing
spec:
  parentRefs:
    - name: cloudflare-tunnel
  hostnames:
    - app.example.com
  rules:
    - matches:
        - path:
            type: Exact
            value: /api/v2/health
          headers:
            - name: X-API-Version
              value: "2"
          method: GET
      filters:
        - type: ResponseHeaderModifier
          responseHeaderModifier:
            add:
              - name: X-Proxy
                value: cloudflare-tunnel-gateway
      backendRefs:
        - name: api-v2
          port: 8080
          weight: 80
        - name: api-v2-canary
          port: 8080
          weight: 20
```

## Monitoring

The proxy does not expose a Prometheus `/metrics` endpoint — its config API serves only `GET /config`, `PUT /config`, `GET /healthz`, and `GET /readyz`. Prometheus metrics are emitted by the controller, which exposes `/metrics` on its dedicated metrics port (via controller-runtime).

Setting `serviceMonitor.enabled: true` renders two ServiceMonitors: one targeting the controller's `metrics` port (the real Prometheus endpoint) and one targeting the proxy's `config-api` port (health and config API only — there is nothing to scrape there yet):

```yaml
serviceMonitor:
  enabled: true
```

## Troubleshooting

### Proxy pods not becoming ready

Check that the tunnel token is valid:

```bash
kubectl logs --selector app.kubernetes.io/component=proxy \
  --namespace cloudflare-tunnel-system
```

### Routes not updating

Verify the controller can reach the proxy config API:

```bash
kubectl get endpoints --selector app.kubernetes.io/component=proxy \
  --namespace cloudflare-tunnel-system
```

### Config API returns stale version

The controller pushes config atomically. Check controller logs for push errors:

```bash
kubectl logs --selector app.kubernetes.io/name=cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system
```
