# CRD Reference

This document provides the API reference for Custom Resource Definitions (CRDs) used by the Cloudflare Tunnel Gateway Controller. The controller ships three project-owned CRDs — `GatewayClassConfig`, `ExternalBackend`, and `GatewayConfig` (per-Gateway data planes) — and watches the standard Gateway API resources.

## GatewayClassConfig

**API Version**: `cf.k8s.lex.la/v1alpha1` **Kind**: `GatewayClassConfig` **Scope**: Cluster

GatewayClassConfig provides tunnel configuration for the controller. It is referenced by a GatewayClass via `spec.parametersRef`.

### Spec

Starting v3 the spec carries only the contract the controller needs for Cloudflare API calls. Proxy-side configuration (tunnel token, replicas, liveness probes) lives in the Helm chart `proxy.*` values; see [Helm chart reference](helm-chart.md). The AmneziaWG sidecar that v2 attached to the controller-managed cloudflared deployment is **not** available in v3 — see [Upgrading v2 → v3](../upgrading/v2-to-v3.md).

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `tunnelID` | string | Yes | Cloudflare Tunnel UUID. Must match the pattern `^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$` |
| `accountId` | string | No | Cloudflare Account ID. If unset, it is read from the `account-id` key in the credentials Secret; if that key is also absent, it is auto-detected from the Cloudflare API when the token has access to a single account. When set, it must be a 32-character lowercase hexadecimal string (validated by a CRD-level CEL rule) |
| `cloudflareCredentialsSecretRef` | SecretReference | Yes | Reference to the Secret containing the Cloudflare API token |

### SecretReference

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | - | Secret name (required) |
| `namespace` | string | controller namespace | Namespace of the Secret. Defaults to the controller's own namespace; set it to place the Secret in a different namespace |
| `key` | string | `api-token` | Key within the Secret |

### Example

```yaml
apiVersion: cf.k8s.lex.la/v1alpha1
kind: GatewayClassConfig
metadata:
  name: cloudflare-tunnel-config
spec:
  tunnelID: "550e8400-e29b-41d4-a716-446655440000"
  # accountId: "0123456789abcdef0123456789abcdef"  # Optional 32-char hex; auto-detected if omitted
  cloudflareCredentialsSecretRef:
    name: cloudflare-credentials
    key: api-token
```

### Status

GatewayClassConfig has a `status.conditions` subresource. The reconciler emits:

- `SecretsResolved` — `True` when the referenced credentials Secret exists and carries the expected key, `False` otherwise.
- `Valid` — `True` when all validation checks pass; `False` with the first failure message otherwise.

## GatewayConfig

`GatewayConfig` is a namespaced CRD carrying per-Gateway data-plane parameters, referenced from `Gateway.spec.infrastructure.parametersRef` (group `cf.k8s.lex.la`, kind `GatewayConfig`, same namespace). Its presence opts the Gateway into a dedicated proxy Deployment and a dedicated Cloudflare Tunnel.

### GatewayConfig Spec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `tunnelTokenSecretRef` | object | Yes | Connector-token Secret in the same namespace (`name`, optional `key`, default `tunnel-token`). The tunnel ID and account are parsed from the token. |
| `cloudflareCredentialsSecretRef` | object | No | API-token override for this Gateway's tunnel-document writes, from a Secret in the SAME namespace (key `api-token` by default); defaults to the GatewayClass → GatewayClassConfig credentials. |
| `authTokenSecretRef` | object | No | Bearer token (same namespace, default key `auth-token`) protecting this data plane's config API. |
| `replicas` | integer | No | Fixed proxy replica count (default 2, max 100). Mutually exclusive with `autoscaling` (CEL-enforced). |
| `autoscaling` | object | No | `minReplicas` (default 2, max 100), `maxReplicas` (max 100), `targetInflightPerPod`, optional `metricName` — renders an HPA on the proxy's in-flight gauge. |
| `resources` | object | No | Proxy container resource requirements. |
| `image` | string | No | Proxy image override; defaults to the controller's `--proxy-image`. |

Replica counts (`replicas`, `minReplicas`, `maxReplicas`) are capped at 100: they are tenant-controlled input on a shared cluster, and an unbounded value is a noisy-neighbour attack. The cap bounds one Gateway, not a tenant — use a per-namespace ResourceQuota for the aggregate.

### GatewayConfig Example

```yaml
apiVersion: cf.k8s.lex.la/v1alpha1
kind: GatewayConfig
metadata:
  name: edge-config
  namespace: tenant-a
spec:
  tunnelTokenSecretRef:
    name: edge-tunnel-token
  autoscaling:
    maxReplicas: 10
    targetInflightPerPod: 50
```

See the [Per-Gateway Isolation guide](../guides/per-gateway-isolation.md) for the full workflow.

## ExternalBackend

**API Version**: `cf.k8s.lex.la/v1alpha1` **Kind**: `ExternalBackend` **Scope**: Namespaced

ExternalBackend defines an out-of-cluster HTTP(S) endpoint that an HTTPRoute or GRPCRoute may target as a `backendRef`. The in-process L7 proxy ultimately dials a URL, so a route can point at an arbitrary external origin without a Service standing in for it. Unlike a Service of type `ExternalName` (which only carries a DNS name and infers the scheme from the port), ExternalBackend makes the scheme explicit and lets the host be an address that is not a valid Service name. It is a spec-only resource (no status): a route referencing a missing or malformed ExternalBackend surfaces the failure on the route's own `ResolvedRefs` condition, mirroring how an unresolvable Service backendRef is reported. See [ExternalBackend](../gateway-api/external-backend.md) for usage details.

### ExternalBackend Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `scheme` | string | Yes | Protocol used to dial the backend: `http` or `https` |
| `host` | string | Yes | Backend hostname or IP address (no scheme, port, or path). IPv6 literals must be bracketed, e.g. `[2001:db8::1]` |
| `port` | integer | Yes | Backend TCP port (1-65535) |
| `path` | string | No | Optional base path prepended to the request path; must begin with `/`. May include a query string whose parameters merge into every dialed request (the request's own parameters win on a key conflict) |

### ExternalBackend Example

```yaml
apiVersion: cf.k8s.lex.la/v1alpha1
kind: ExternalBackend
metadata:
  name: external-api
  namespace: default
spec:
  scheme: https
  host: api.example.com
  port: 443
  path: /v1
```

## Gateway API Resources

The controller watches standard Gateway API resources. For their full specification, see the [Gateway API documentation](https://gateway-api.sigs.k8s.io/).

### GatewayClass

Standard Gateway API GatewayClass with `parametersRef` pointing to GatewayClassConfig:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: cloudflare-tunnel
spec:
  controllerName: cf.k8s.lex.la/tunnel-controller
  parametersRef:
    group: cf.k8s.lex.la
    kind: GatewayClassConfig
    name: cloudflare-tunnel-config
```

### Gateway

Standard Gateway API Gateway:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: cloudflare-tunnel
  namespace: cloudflare-tunnel-system
spec:
  gatewayClassName: cloudflare-tunnel
  listeners:
    - name: http
      port: 80
      protocol: HTTP
```

### HTTPRoute

Standard Gateway API HTTPRoute:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: my-app
  namespace: default
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - app.example.com
  rules:
    - backendRefs:
        - name: my-service
          port: 80
```

### GRPCRoute

GRPCRoute is served by the in-process L7 proxy — gRPC service/method matches map onto `/{service}/{method}` path rules and the upstream hop is h2c. See [GRPCRoute](../gateway-api/grpcroute.md) for the full field support matrix.

Standard Gateway API GRPCRoute:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata:
  name: my-grpc-service
  namespace: default
spec:
  parentRefs:
    - name: cloudflare-tunnel
      namespace: cloudflare-tunnel-system
  hostnames:
    - grpc.example.com
  rules:
    - backendRefs:
        - name: grpc-server
          port: 50051
```

### ReferenceGrant

Standard Gateway API ReferenceGrant for cross-namespace references:

```yaml
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-cross-namespace
  namespace: backend
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: frontend
  to:
    - group: ""
      kind: Service
```

## Status Conditions

### Gateway Status

| Condition | Status | Reason | Description |
|-----------|--------|--------|-------------|
| `Accepted` | `True` | `Accepted` | Gateway accepted by controller |
| `Accepted` | `False` | `ListenersNotValid` | Gateway has conflicted own listeners (one or more own listeners carry `Conflicted: True`); per-listener status reports the conflict |
| `Accepted` | `False` | `InvalidParameters` | GatewayClassConfig referenced by the GatewayClass cannot be resolved |
| `Programmed` | `True` | `Programmed` | Gateway configured in Cloudflare |
| `Programmed` | `False` | `Invalid` | GatewayClassConfig referenced by the GatewayClass cannot be resolved |

### HTTPRoute/GRPCRoute Status

| Condition | Status | Reason | Description |
|-----------|--------|--------|-------------|
| `Accepted` | `True` | `Accepted` | Route accepted and synced |
| `Accepted` | `False` | `NoMatchingParent` | No listener matched the parentRef's `sectionName` or `port`; also fires when hostname is the failure reason and the parentRef pinned a `sectionName` or `port` |
| `Accepted` | `False` | `NoMatchingListenerHostname` | Route hostnames do not intersect with any listener hostname (no `sectionName`/`port` pin on the parentRef) |
| `Accepted` | `False` | `NotAllowedByListeners` | Route namespace or kind not allowed by listener |
| `Accepted` | `False` | `Pending` | Sync to the Cloudflare Tunnel API failed; reconcile will retry. Proxy-push failures are best-effort: they are logged and counted via the `cftunnel_sync_errors_total{error_type="proxy_push"}` counter but do **not** flip `Accepted` to False / Reason=`Pending` |
| `Accepted` | `False` | `UnsupportedProtocol` | GRPCRoute only: gRPC cannot be served over an explicit `proxy.tunnel.protocol: quic` tunnel (cloudflared drops HTTP trailers over QUIC, losing `grpc-status`). Switch to `http2`, or `auto`/unset which the proxy upgrades to `http2` for gRPC |
| `Accepted` | `False` | `Conflicted` | An HTTPRoute and a GRPCRoute conflict on the same Gateway with intersecting hostnames; the oldest Route by `creationTimestamp` (ties broken by `{namespace}/{name}`) is accepted and the other is rejected |
| `ResolvedRefs` | `True` | `ResolvedRefs` | Backend references resolved |
| `ResolvedRefs` | `False` | `RefNotPermitted` | Cross-namespace reference denied |
| `ResolvedRefs` | `False` | `BackendNotFound` | Backend Service not found |
| `ResolvedRefs` | `False` | `InvalidKind` | Backend ref group/kind is not a core Service |

## API Versions

| Resource | API Group | Version | Status |
|----------|-----------|---------|--------|
| GatewayClassConfig | `cf.k8s.lex.la` | `v1alpha1` | Alpha |
| ExternalBackend | `cf.k8s.lex.la` | `v1alpha1` | Alpha |
| GatewayConfig | `cf.k8s.lex.la` | `v1alpha1` | Alpha |
| GatewayClass | `gateway.networking.k8s.io` | `v1` | GA |
| Gateway | `gateway.networking.k8s.io` | `v1` | GA |
| HTTPRoute | `gateway.networking.k8s.io` | `v1` | GA |
| GRPCRoute | `gateway.networking.k8s.io` | `v1` | GA |
| ReferenceGrant | `gateway.networking.k8s.io` | `v1beta1` | Beta |

## Installing CRDs

### Gateway API CRDs

```bash
kubectl apply --filename https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml
```

### Project CRDs (GatewayClassConfig and ExternalBackend)

Installed automatically by the Helm chart. For manual installation:

```bash
kubectl apply --filename charts/cloudflare-tunnel-gateway-controller/crds/
```
