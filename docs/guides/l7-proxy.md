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
| `TUNNEL_TOKEN` | Required for tunnel mode; omit for standalone/dev mode | Cloudflare tunnel token (base64). Omitting it selects standalone mode; setting it to an empty value is a broken configuration and refuses to start, rather than selecting standalone silently. |
| `PROXY_CONFIG_ADDR` | `:8081` | Config API listen address |
| `PROXY_ADDR` | `:8080` | Proxy listen address |
| `PROXY_AUTH_TOKEN` | unset | Bearer token for config push API authentication. In tunnel mode an unset token refuses to start, because the config API listens on every interface and a successful push replaces the whole routing table; standalone mode keeps running unauthenticated. Set but empty is a broken configuration in either mode, not a choice to run open, and also refuses to start; see [Config API Authentication](#config-api-authentication). |
| `PROXY_ALLOW_UNAUTHENTICATED_CONFIG_API` | `false` | Run tunnel mode with an unauthenticated config API anyway. Separate from `PROXY_AUTH_TOKEN` on purpose: an empty token is what a broken Secret produces, and a misconfiguration must not be able to spell the same thing as consent. |
| `PROXY_METRICS_ENABLED` | `true` | Expose the data-plane Prometheus metrics at `/metrics` on the config API port. Set `false`/`0` to disable. |
| `PROXY_GRACE_PERIOD` | `30s` | Connector drain window on shutdown (Go duration, capped at 3m): the proxy unregisters from the edge and gives in-flight requests this long before exiting. |
| `PROXY_TUNNEL_PROTOCOL` | `auto` | Edge transport: `auto`, `http2`, or `quic`. `auto` dials QUIC with HTTP/2 fallback. gRPC needs `http2` because QUIC drops trailers; the proxy upgrades `auto` to `http2` only when the first pushed config carries a GRPCRoute. |
| `PROXY_TUNNEL_PROTOCOL_WAIT` | `0` (no wait) | In `auto` mode, how long (Go duration) to wait for the first pushed config before serving, so the protocol is chosen from real routes. |
| `PROXY_WS_DIAL_TIMEOUT` | `""` (proxy default 30s) | Go-duration cap on the backend dial during a WebSocket upgrade. |
| `PROXY_WS_HANDSHAKE_TIMEOUT` | `""` (proxy default 30s) | Go-duration cap on waiting for the backend's `101 Switching Protocols`. |
| `PROXY_ACCESS_LOG_ENABLED` | `false` | Enable per-request structured JSON access logging on stdout. |
| `PROXY_ACCESS_LOG_SAMPLING_RATE` | `1` | Fraction of non-5xx requests to log when access logging is enabled, in `[0, 1]` (5xx are always logged). |
| `PROXY_ACCESS_LOG_STRIP_QUERY` | `false` | Strip the request URL query string from access-log lines. |
| `PROXY_ALLOW_X_ORIGINAL_HOST` | `false` | Trust the client-supplied `X-Original-Host` header as the routing key and backend `Host`. Test deployments only — see the warning below. |
| `PROXY_TRACING_ENABLED` | `false` | Enable OpenTelemetry tracing of proxied requests. |
| `PROXY_TRACING_ENDPOINT` | `""` | OTLP exporter endpoint for traces (when tracing is enabled). |
| `PROXY_TRACING_SAMPLE_RATE` | `1` | Trace sampling fraction in `[0, 1]` (when tracing is enabled). |

!!! danger "`PROXY_ALLOW_X_ORIGINAL_HOST` is for test deployments only"

    The proxy strips `X-Original-Host` from every request unless this is set. It exists because the Gateway API conformance suite drives domains that are not registered on the Cloudflare account: the edge rejects them by `Host`, so the suite addresses the edge hostname and carries its intended host in that header instead.

    The edge forwards arbitrary `X-*` headers from any client, so a proxy that trusts this header lets a client that reaches one hostname be served by a different hostname's backend — with the intended hostname's edge policy (Access, WAF, rate limits) evaluated against the wrong name, and the backend seeing a `Host` of the caller's choosing. Enable it only in a throwaway conformance or e2e deployment. The chart value is `proxy.allowXOriginalHost`, and the proxy logs a warning at startup whenever it is on.

### Config API Authentication

The config API is always authenticated when deployed via the chart: leave `proxy.authTokenSecretRef.name` empty (the default) and the controller itself generates a random token into a Secret named `<fullname>-proxy-auth-token` (`<fullname>` is the Helm release fullname, typically `<release>-cloudflare-tunnel-gateway-controller`, or just `<release>` when the release name already contains the chart name) as one of its first startup actions, uses it directly for its own push auth, and the proxy reads the same Secret via a pod-level `secretKeyRef`. The token is created once and reused on every restart, never rotated -- so neither an upgrade nor a controller restart rolls the proxy on its own. Set `proxy.authTokenSecretRef.name` to point at your own Secret instead, for example to manage rotation externally -- the controller resolves it the same way (directly via the API, never a `secretKeyRef` on its own pod) and never creates or modifies it: a missing bring-your-own Secret is a configuration error, not something silently papered over.

On a brand-new install, the proxy Deployment's pod template references the generated Secret before the controller has had a chance to create it -- the affected proxy pod(s) sit briefly in `CreateContainerConfigError` until the controller finishes starting, and kubelet's normal retry picks up the Secret once it exists. This is a one-time, self-resolving startup race, not a failure to act on.

Outside the chart, tunnel mode refuses to start when `PROXY_AUTH_TOKEN` is not set at all. The config API binds `:8081` on every interface and a successful `PUT /config` replaces the entire routing table, so an unauthenticated one is not a state a deployment should reach by leaving a variable out. Standalone mode is a development server and still starts without a token.

If you deliberately run tunnel mode with no authentication — the config API reachable only over a loopback or an equally closed path — set `PROXY_ALLOW_UNAUTHENTICATED_CONFIG_API=1`. It is a second variable rather than an empty `PROXY_AUTH_TOKEN` because an empty token is what a broken Secret produces, and a misconfiguration must not be able to spell the same thing as consent. The proxy logs a warning at startup whenever it is running open.

!!! note "Upgrading from a release with no auth token"

    If you were pushing config to the proxy directly (bypassing the controller) with no `Authorization` header, that stops working after upgrading to a chart version with this default: find the exact Secret name your release rendered with `helm get manifest <release> | grep -m1 'proxy-auth-secret-ref'`, then read the token with `kubectl get secret <fullname>-proxy-auth-token -o jsonpath='{.data.auth-token}' | base64 -d` and send it as `Authorization: Bearer <token>`. During the rollout itself there is a brief window where the new, already-authenticated proxy pod rejects pushes from an old controller pod that has not yet rolled (401), and the reverse (an old, unauthenticated proxy pod accepting an authenticated push, since it never checks the header). Both resolve on their own once the rolling update finishes and both sides are on the new pod template -- no manual step is needed.

The generated Secret is created by the controller directly, not rendered by Helm, so `helm uninstall` leaves it behind (see [Uninstalling](../getting-started/installation.md#uninstalling)). A Secret at this name with a missing or empty `auth-token` key fails the controller closed at startup instead of running with an unusable token. The proxy fails closed the same way on its own side: a `PROXY_AUTH_TOKEN` set to an empty value refuses to start in either mode rather than silently serving the config API to anyone, and an unset one refuses to start in tunnel mode as described above -- see [Config API Auth Secret Missing or Broken](../operations/troubleshooting.md#config-api-auth-secret-missing-or-broken) for the exact error and the recovery command. The token authenticates the push, it does not encrypt it: the push is plain HTTP carrying that token and, where a route's Gateway configures backend mTLS, the client certificate's private key — wire confidentiality is the cluster's to provide, and the full boundary is written out under [Config API Authentication](../reference/security.md#config-api-authentication).

### Health Endpoints

| Endpoint | Port | Description |
| --- | --- | --- |
| `/healthz` | Config API | Liveness check |
| `/readyz` | Config API | Readiness: config loaded at least once AND, in tunnel mode, the tunnel has connected to the Cloudflare edge (standalone mode latches the tunnel condition at startup) |

In tunnel mode, a bootstrap dial failure (cluster DNS unreachable, the edge briefly unreachable) retries with jittered exponential backoff (2s up to a 30s cap) instead of exiting — the pod stays `Running` and reports `/readyz` false throughout, rather than crash-looping. See [Proxy Pod Stuck NotReady After a Restart](../operations/troubleshooting.md#proxy-pod-stuck-notready-after-a-restart) for diagnosis.

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
