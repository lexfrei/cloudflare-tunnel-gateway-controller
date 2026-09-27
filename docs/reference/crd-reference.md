# CRD Reference

This document provides the API reference for Custom Resource Definitions (CRDs) used by the Cloudflare Tunnel Gateway Controller. The controller ships three project-owned CRDs — `GatewayClassConfig`, `ExternalBackend`, and `GatewayConfig` (per-Gateway data planes) — and watches the standard Gateway API resources.

The field-by-field reference for the three project CRDs, with types and OpenAPI validation, is the [API Reference](api.md), generated from the Go types the CRDs are built from. It does not render CEL rules, such as the `accountId` format or the `replicas`/`autoscaling` exclusion; those live in the CRD schemas under `charts/cloudflare-tunnel-gateway-controller/crds/` as `x-kubernetes-validations`. This page covers what that one does not: examples, status conditions, and the Gateway API resources.

## GatewayClassConfig

**API Version**: `cf.k8s.lex.la/v1alpha1` **Kind**: `GatewayClassConfig` **Scope**: Cluster

GatewayClassConfig provides tunnel configuration for the controller. It is referenced by a GatewayClass via `spec.parametersRef`.

### Spec

The spec carries only the contract the controller needs for Cloudflare API calls: `tunnelID`, `cloudflareCredentialsSecretRef`, and the optional `accountId`, `allowSharedTunnels` and `maxDataPlanesPerNamespace`. Every field is described in [GatewayClassConfigSpec](api.md#gatewayclassconfigspec). Proxy-side configuration (tunnel token, replicas, liveness probes) lives in the Helm chart `proxy.*` values; see [Helm chart reference](helm-chart.md). For the data-plane cap, see also the [Per-Gateway Isolation guide](../guides/per-gateway-isolation.md).

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

The only required field is `tunnelTokenSecretRef`, a connector-token Secret in the same namespace; the tunnel ID and account are parsed from the token. `replicas` and `autoscaling` are mutually exclusive. Every field is described in [GatewayConfigSpec](api.md#gatewayconfigspec).

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

`scheme`, `host` and `port` are required; `path` is an optional base path. Every field is described in [ExternalBackendSpec](api.md#externalbackendspec).

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
| `Accepted` | `False` | `InvalidParameters` | The Gateway's configuration cannot be resolved: the GatewayClassConfig referenced by the GatewayClass is unreadable, the per-Gateway `parametersRef` is invalid, no proxy image is configured, or the Gateway claims a Cloudflare Tunnel it does not own |
| `Accepted` | `False` | `DataPlaneQuotaExceeded` | The Gateway's namespace already holds as many dedicated data planes as `maxDataPlanesPerNamespace` allows. Implementation-specific reason; the oldest Gateways by creation timestamp keep their planes |
| `Programmed` | `True` | `Programmed` | Gateway configured in Cloudflare |
| `Programmed` | `False` | `Invalid` | The Gateway's configuration cannot be resolved, or it was refused the tunnel it claimed (see the `Accepted` reason above) |
| `Programmed` | `False` | `NoResources` | The Gateway's namespace is at its dedicated data-plane cap, so no plane was scheduled for it |

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

### Controller-specific advisory conditions

Beyond the standard Gateway API conditions above, the controller surfaces domain-prefixed (`cf.k8s.lex.la/`) advisory conditions for situations the Gateway API defines no condition for — they are informational and do not flip `Accepted`/`Programmed`. On routes: `cf.k8s.lex.la/RouteShadowed`, `cf.k8s.lex.la/ProxyConfigPushed`, `cf.k8s.lex.la/TunnelShared`. On a Gateway listener or ListenerSet entry: `cf.k8s.lex.la/PermissiveHostname` (the `allowedRoutes.namespaces.from: All` + unpinned-hostname capture combination). Each is described in full on the [Limitations](../gateway-api/limitations.md) page.

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
kubectl apply --filename https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.2/standard-install.yaml
```

### Project CRDs

Installed automatically by the Helm chart. For manual installation:

```bash
kubectl apply --filename charts/cloudflare-tunnel-gateway-controller/crds/
```
