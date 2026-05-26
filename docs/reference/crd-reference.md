# CRD Reference

This document provides the API reference for Custom Resource Definitions (CRDs) used by the Cloudflare Tunnel Gateway Controller.

## GatewayClassConfig

**API Version**: `cf.k8s.lex.la/v1alpha1` **Kind**: `GatewayClassConfig` **Scope**: Cluster

GatewayClassConfig provides tunnel configuration for the controller. It is referenced by a GatewayClass via `spec.parametersRef`.

### Spec

Starting v3 the spec carries only the contract the controller needs for Cloudflare API calls. Proxy-side configuration (tunnel token, replicas, liveness probes) lives in the Helm chart `proxy.*` values; see [Helm chart reference](helm-chart.md). The AmneziaWG sidecar that v2 attached to the controller-managed cloudflared deployment is **not** available in v3 — see [Upgrading v2 → v3](../upgrading/v2-to-v3.md).

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `tunnelID` | string | Yes | Cloudflare Tunnel UUID |
| `accountId` | string | No | Cloudflare Account ID (auto-detected if not specified) |
| `cloudflareCredentialsSecretRef` | SecretKeySelector | Yes | Reference to Secret containing API token |

### SecretKeySelector

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | - | Secret name |
| `key` | string | `api-token` | Key within the Secret |

### Example

```yaml
apiVersion: cf.k8s.lex.la/v1alpha1
kind: GatewayClassConfig
metadata:
  name: cloudflare-tunnel-config
spec:
  tunnelID: "550e8400-e29b-41d4-a716-446655440000"
  # accountId: "1234567890abcdef"  # Optional, auto-detected
  cloudflareCredentialsSecretRef:
    name: cloudflare-credentials
    key: api-token
```

### Status

GatewayClassConfig has a `status.conditions` subresource. The reconciler emits:

- `SecretsResolved` — `True` when the referenced credentials Secret exists and carries the expected key, `False` otherwise.
- `Valid` — `True` when all validation checks pass; `False` with the first failure message otherwise.

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

!!! warning "Not supported in v3"

    GRPCRoute is recognised as a CRD kind but does not route in v3 — the in-process L7 proxy has no gRPC matcher and requests return `404`. The example below is preserved as a v0.8-era reference; use HTTPRoute as a v3 workaround, or stay on the v2.x chart line. See the [GRPCRoute limitation](../gateway-api/limitations.md#grpcroute-is-not-supported-in-v3) for the full explanation.

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
| `Programmed` | `True` | `Programmed` | Gateway configured in Cloudflare |

### HTTPRoute/GRPCRoute Status

| Condition | Status | Reason | Description |
|-----------|--------|--------|-------------|
| `Accepted` | `True` | `Accepted` | Route accepted and synced |
| `Accepted` | `False` | `NoMatchingParent` | No listener matched the parentRef's `sectionName` or `port`; also fires when hostname is the failure reason and the parentRef pinned a `sectionName` or `port` |
| `Accepted` | `False` | `NoMatchingListenerHostname` | Route hostnames do not intersect with any listener hostname (no `sectionName`/`port` pin on the parentRef) |
| `Accepted` | `False` | `NotAllowedByListeners` | Route namespace or kind not allowed by listener |
| `Accepted` | `False` | `Pending` | Sync to the Cloudflare Tunnel API failed; reconcile will retry. Proxy-push failures are best-effort: they are logged and counted via the `proxy_push` sync-error metric but do **not** flip `Accepted` to False / Reason=`Pending` |
| `ResolvedRefs` | `True` | `ResolvedRefs` | Backend references resolved |
| `ResolvedRefs` | `False` | `RefNotPermitted` | Cross-namespace reference denied |
| `ResolvedRefs` | `False` | `BackendNotFound` | Backend Service not found |
| `ResolvedRefs` | `False` | `InvalidKind` | Backend ref group/kind is not a core Service |

## API Versions

| Resource | API Group | Version | Status |
|----------|-----------|---------|--------|
| GatewayClassConfig | `cf.k8s.lex.la` | `v1alpha1` | Alpha |
| GatewayClass | `gateway.networking.k8s.io` | `v1` | GA |
| Gateway | `gateway.networking.k8s.io` | `v1` | GA |
| HTTPRoute | `gateway.networking.k8s.io` | `v1` | GA |
| GRPCRoute | `gateway.networking.k8s.io` | `v1` | GA (not consumed in v3 — see [limitations](../gateway-api/limitations.md#grpcroute-is-not-supported-in-v3)) |
| ReferenceGrant | `gateway.networking.k8s.io` | `v1beta1` | Beta |

## Installing CRDs

### Gateway API CRDs

```bash
kubectl apply --filename https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.0/standard-install.yaml
```

### GatewayClassConfig CRD

Installed automatically by the Helm chart. For manual installation:

```bash
kubectl apply --filename charts/cloudflare-tunnel-gateway-controller/crds/
```
