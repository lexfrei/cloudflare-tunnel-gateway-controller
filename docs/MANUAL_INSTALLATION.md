# Manual Installation

> **Note:** The recommended installation method is via [Helm chart](../charts/cloudflare-tunnel-gateway-controller/README.md). Use manual installation only if you have specific requirements that prevent using Helm.

## Prerequisites

- Kubernetes cluster with kubectl configured
- Gateway API CRDs installed
- Cloudflare account with a pre-created tunnel (see main [README](../README.md#create-cloudflare-tunnel))

## Installation Steps

### 1. Install Gateway API CRDs

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml
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

See [deploy/samples/secrets.yaml](../deploy/samples/secrets.yaml) for complete examples including AWG configuration.

### 4. Deploy the Controller

```bash
kubectl apply -f deploy/rbac/
kubectl apply -f deploy/controller/
```

### 5. Create GatewayClassConfig, GatewayClass, and Gateway

Edit [deploy/samples/gatewayclassconfig.yaml](../deploy/samples/gatewayclassconfig.yaml) with your tunnel ID:

```bash
kubectl apply -f deploy/samples/gatewayclassconfig.yaml
kubectl apply -f deploy/samples/gatewayclass.yaml
kubectl apply -f deploy/samples/gateway.yaml
```

## Upgrading

When upgrading manually, ensure you apply manifests in the correct order:

1. Update CRDs (if changed)
2. Update RBAC resources
3. Update controller deployment

## Uninstalling

```bash
kubectl delete -f deploy/samples/gateway.yaml
kubectl delete -f deploy/samples/gatewayclass.yaml
kubectl delete -f deploy/samples/gatewayclassconfig.yaml
kubectl delete -f deploy/controller/
kubectl delete -f deploy/rbac/
kubectl delete namespace cloudflare-tunnel-system
```
