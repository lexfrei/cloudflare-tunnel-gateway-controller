# Manual Installation

!!! warning "Not Recommended"

    The recommended installation method is via Helm chart. Use manual
    installation only if you have specific requirements that prevent
    using Helm.

## Prerequisites

- Kubernetes cluster with kubectl configured
- Gateway API CRDs installed
- Cloudflare account with a pre-created tunnel

## Installation Steps

### 1. Install Gateway API CRDs

```bash
kubectl apply --filename https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml
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

# Tunnel token (required for managed cloudflared)
kubectl create secret generic cloudflare-tunnel-token \
  --namespace cloudflare-tunnel-system \
  --from-literal=tunnel-token="YOUR_TUNNEL_TOKEN"
```

### 4. Deploy the Controller

```bash
kubectl apply --filename deploy/rbac/
kubectl apply --filename deploy/controller/
```

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
│   ├── serviceaccount.yaml
│   ├── clusterrole.yaml
│   └── clusterrolebinding.yaml
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
  tunnelTokenSecretRef:
    name: cloudflare-tunnel-token
  cloudflared:
    enabled: true
```

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
    - name: http
      port: 80
      protocol: HTTP
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

For production deployments, the Helm chart is strongly recommended for its
ease of use, upgrade path, and validation features.
