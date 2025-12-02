# CRD Reference

This document provides the API reference for Custom Resource Definitions (CRDs)
used by the Cloudflare Tunnel Gateway Controller.

## GatewayClassConfig

**API Version**: `cf.k8s.lex.la/v1alpha1`
**Kind**: `GatewayClassConfig`
**Scope**: Cluster

GatewayClassConfig provides tunnel configuration for the controller. It is
referenced by a GatewayClass via `spec.parametersRef`.

### Spec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `tunnelID` | string | Yes | Cloudflare Tunnel UUID |
| `accountID` | string | No | Cloudflare Account ID (auto-detected if not specified) |
| `cloudflareCredentialsSecretRef` | SecretKeySelector | Yes | Reference to Secret containing API token |
| `tunnelTokenSecretRef` | SecretKeySelector | Conditional | Reference to Secret containing tunnel token (required when `cloudflared.enabled=true`) |
| `cloudflared` | CloudflaredSpec | No | cloudflared deployment configuration |

### CloudflaredSpec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Deploy cloudflared via Helm |
| `awg` | AWGSpec | - | AmneziaWG sidecar configuration |

### AWGSpec

| Field | Type | Description |
|-------|------|-------------|
| `secretName` | string | Name of Secret containing AWG configuration |

### SecretKeySelector

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | - | Secret name |
| `key` | string | `api-token` or `tunnel-token` | Key within the Secret |

### Example

```yaml
apiVersion: cf.k8s.lex.la/v1alpha1
kind: GatewayClassConfig
metadata:
  name: cloudflare-tunnel-config
spec:
  tunnelID: "550e8400-e29b-41d4-a716-446655440000"
  # accountID: "1234567890abcdef"  # Optional, auto-detected
  cloudflareCredentialsSecretRef:
    name: cloudflare-credentials
    key: api-token
  tunnelTokenSecretRef:
    name: cloudflare-tunnel-token
    key: tunnel-token
  cloudflared:
    enabled: true
    awg:
      secretName: awg-config  # Optional
```

### Status

GatewayClassConfig does not have a status subresource.

## Gateway API Resources

The controller watches standard Gateway API resources. For their full
specification, see the
[Gateway API documentation](https://gateway-api.sigs.k8s.io/).

### GatewayClass

Standard Gateway API GatewayClass with `parametersRef` pointing to
GatewayClassConfig:

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
| `Accepted` | `False` | `NoMatchingParent` | Sync to Cloudflare failed |
| `ResolvedRefs` | `True` | `ResolvedRefs` | Backend references resolved |
| `ResolvedRefs` | `False` | `RefNotPermitted` | Cross-namespace reference denied |

## API Versions

| Resource | API Group | Version | Status |
|----------|-----------|---------|--------|
| GatewayClassConfig | `cf.k8s.lex.la` | `v1alpha1` | Alpha |
| GatewayClass | `gateway.networking.k8s.io` | `v1` | GA |
| Gateway | `gateway.networking.k8s.io` | `v1` | GA |
| HTTPRoute | `gateway.networking.k8s.io` | `v1` | GA |
| GRPCRoute | `gateway.networking.k8s.io` | `v1` | GA |
| ReferenceGrant | `gateway.networking.k8s.io` | `v1beta1` | Beta |

## Installing CRDs

### Gateway API CRDs

```bash
kubectl apply --filename https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml
```

### GatewayClassConfig CRD

Installed automatically by the Helm chart. For manual installation:

```bash
kubectl apply --filename deploy/crds/
```
