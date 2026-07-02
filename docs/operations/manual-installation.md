# Manual Installation

!!! warning "Not Recommended"

    The recommended installation method is via Helm chart. Use manual installation only if you have specific requirements that prevent using Helm.

## Prerequisites

- Kubernetes cluster with kubectl configured
- Gateway API CRDs installed
- Cloudflare account with a pre-created tunnel

## Installation Steps

### 1. Install Gateway API CRDs

```bash
kubectl apply --filename https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.0/standard-install.yaml
```

### 2. Create Namespace

```bash
kubectl create namespace cloudflare-tunnel-system
```

### 3. Create Secrets

```bash
# Cloudflare API credentials
kubectl create secret generic cloudflare-credentials \
  --namespace cloudflare-tunnel-system \
  --from-literal=api-token="YOUR_API_TOKEN"

# Tunnel token (consumed by the L7 proxy pod via the chart's proxy.tunnelTokenSecretRef)
kubectl create secret generic cloudflare-tunnel-token \
  --namespace cloudflare-tunnel-system \
  --from-literal=tunnel-token="YOUR_TUNNEL_TOKEN"
```

### 4. Deploy the Controller

```bash
kubectl apply --filename deploy/rbac/
kubectl apply --filename deploy/controller/
```

!!! danger "Manual manifests ship the controller only — the L7 proxy is not included"

    Under v3 the in-process L7 proxy is the only data plane, and the controller pushes routing config to it at the `--proxy-endpoints` URL. The `deploy/` manifests intentionally ship only the controller (and the RBAC it needs); they do NOT include the proxy Deployment, Services, or config. Without the proxy running, no HTTPRoute or GRPCRoute takes effect and all traffic is silently non-functional — even though step 3 created the `cloudflare-tunnel-token` Secret. Use the [Helm chart](../getting-started/installation.md) for a complete, working v3 installation, or supply your own proxy Deployment that consumes the tunnel token and serves the config API at the `--proxy-endpoints` URL.

### 5. Create GatewayClassConfig, GatewayClass, and Gateway

Edit `deploy/samples/gatewayclassconfig.yaml` with your tunnel ID:

```bash
kubectl apply --filename deploy/samples/gatewayclassconfig.yaml
kubectl apply --filename deploy/samples/gatewayclass.yaml
kubectl apply --filename deploy/samples/gateway.yaml
```

## Manifest Structure

The `deploy/` directory contains:

```text
deploy/
├── rbac/
│   ├── service_account.yaml
│   ├── role.yaml
│   └── role_binding.yaml
├── controller/
│   └── deployment.yaml
└── samples/
    ├── secrets.yaml
    ├── gatewayclassconfig.yaml
    ├── gatewayclass.yaml
    └── gateway.yaml
```

## Sample GatewayClassConfig

```yaml
apiVersion: cf.k8s.lex.la/v1alpha1
kind: GatewayClassConfig
metadata:
  name: cloudflare-tunnel-config
spec:
  tunnelID: "YOUR_TUNNEL_UUID"
  cloudflareCredentialsSecretRef:
    name: cloudflare-credentials
  # accountId: "1234567890abcdef"  # Optional, auto-detected
```

The tunnel token is passed to the L7 proxy pod as the `TUNNEL_TOKEN` environment variable. In the Helm chart this is wired via `proxy.tunnelTokenSecretRef`; for a manual proxy deployment, expose the `cloudflare-tunnel-token` Secret to the proxy container the same way. The token is not part of the GatewayClassConfig CRD spec.

## Sample GatewayClass

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

## Sample Gateway

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: cloudflare-tunnel
  namespace: cloudflare-tunnel-system
spec:
  gatewayClassName: cloudflare-tunnel
  listeners:
    - name: https
      port: 443
      protocol: HTTPS
      # Allow HTTPRoutes from all namespaces
      allowedRoutes:
        namespaces:
          from: All
```

## Upgrading

When upgrading manually, apply manifests in order:

1. Update CRDs (if changed)
2. Update RBAC resources
3. Update controller deployment

```bash
kubectl apply --filename deploy/rbac/
kubectl apply --filename deploy/controller/
```

## Uninstalling

```bash
kubectl delete --filename deploy/samples/gateway.yaml
kubectl delete --filename deploy/samples/gatewayclass.yaml
kubectl delete --filename deploy/samples/gatewayclassconfig.yaml
kubectl delete --filename deploy/controller/
kubectl delete --filename deploy/rbac/
kubectl delete namespace cloudflare-tunnel-system
```

## Comparison with Helm

| Aspect | Helm | Manual |
|--------|------|--------|
| CRD management | Automatic | Manual |
| RBAC setup | Automatic | Manual |
| Upgrades | `helm upgrade` | `kubectl apply` |
| Rollback | `helm rollback` | Manual |
| Value validation | Schema validation | None |
| Templating | Built-in | None |

## When to Use Manual Installation

- Air-gapped environments without Helm access
- Strict policy requirements against Helm
- Custom modifications not supported by chart
- Educational/debugging purposes

For production deployments, the Helm chart is strongly recommended for its ease of use, upgrade path, and validation features.
