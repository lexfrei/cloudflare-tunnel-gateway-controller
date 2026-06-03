# Installation

This guide covers installing the Cloudflare Tunnel Gateway Controller using Helm.

## Helm Installation

Helm is the only supported installation method. It handles CRD installation, RBAC setup, and provides a simple upgrade path.

### Basic Installation

```bash
kubectl create namespace cloudflare-tunnel-system
kubectl create secret generic cloudflare-credentials \
  --namespace cloudflare-tunnel-system \
  --from-literal=api-token="YOUR_API_TOKEN"
kubectl create secret generic cloudflare-tunnel-token \
  --namespace cloudflare-tunnel-system \
  --from-literal=tunnel-token="YOUR_TUNNEL_TOKEN"

helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system \
  --set gatewayClassConfig.create=true \
  --set gatewayClassConfig.tunnelID=YOUR_TUNNEL_ID \
  --set gatewayClassConfig.cloudflareCredentialsSecretRef.name=cloudflare-credentials \
  --set proxy.tunnelTokenSecretRef.name=cloudflare-tunnel-token
```

### Installation with Values File

Create a `values.yaml` file:

```yaml
gatewayClassConfig:
  create: true
  tunnelID: "550e8400-e29b-41d4-a716-446655440000"
  cloudflareCredentialsSecretRef:
    name: cloudflare-credentials

# L7 proxy data plane (embeds cloudflared transport in-process).
# tunnelTokenSecretRef.name is REQUIRED — the chart will fail install otherwise.
proxy:
  replicas: 2
  tunnelTokenSecretRef:
    name: cloudflare-tunnel-token

# Controller deployment
replicaCount: 2
resources:
  limits:
    memory: 256Mi
  requests:
    cpu: 100m
    memory: 128Mi
leaderElection:
  enabled: true
```

Then install:

```bash
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system \
  --create-namespace \
  --values values.yaml
```

The chart always deploys the in-process L7 proxy alongside the controller (this is the only data plane in v3). For the full list of proxy knobs, see the [Helm values reference](../configuration/helm-values.md) and the [L7 Proxy Guide](../guides/l7-proxy.md).

## Verify Installation

Check that the controller and proxy pods are running:

```bash
kubectl get pods --namespace cloudflare-tunnel-system
```

Expected output:

```text
NAME                                                            READY   STATUS    RESTARTS   AGE
cloudflare-tunnel-gateway-controller-7d8f9b6c5d-x2j9k           1/1     Running   0          30s
cloudflare-tunnel-gateway-controller-proxy-5c4d8b7f6c-m8n3l     1/1     Running   0          30s
cloudflare-tunnel-gateway-controller-proxy-5c4d8b7f6c-pqr4t     1/1     Running   0          30s
```

Check GatewayClass:

```bash
kubectl get gatewayclass cloudflare-tunnel
```

Expected output:

```text
NAME               CONTROLLER                          ACCEPTED   AGE
cloudflare-tunnel  cf.k8s.lex.la/tunnel-controller     True       30s
```

## Upgrading

To upgrade to a newer version:

```bash
helm upgrade cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/charts/cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system \
  --values values.yaml
```

## Uninstalling

To remove the controller:

```bash
helm uninstall cloudflare-tunnel-gateway-controller \
  --namespace cloudflare-tunnel-system
```

!!! warning "Cleanup"

    Uninstalling the Helm release will remove the controller and proxy pods. The tunnel configuration in Cloudflare will remain. To fully clean up, delete the tunnel from the Cloudflare dashboard.

## Alternative: External Secrets

For production deployments, consider using [external-secrets](https://external-secrets.io/) to manage Cloudflare credentials:

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: cloudflare-credentials
  namespace: cloudflare-tunnel-system
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-backend
    kind: ClusterSecretStore
  target:
    name: cloudflare-credentials
  data:
    - secretKey: api-token
      remoteRef:
        key: cloudflare/api-token
```

## Next Steps

After installation, proceed to [Quick Start](quickstart.md) to create your first HTTPRoute.
