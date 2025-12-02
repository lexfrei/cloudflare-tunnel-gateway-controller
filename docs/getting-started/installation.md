# Installation

This guide covers installing the Cloudflare Tunnel Gateway Controller using
Helm.

## Helm Installation

Helm is the only supported installation method. It handles CRD installation,
RBAC setup, and provides a simple upgrade path.

### Basic Installation

```bash
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller/chart \
  --namespace cloudflare-tunnel-system \
  --create-namespace \
  --set config.tunnelID=YOUR_TUNNEL_ID \
  --set config.apiToken=YOUR_API_TOKEN
```

### Installation with Values File

Create a `values.yaml` file:

```yaml
config:
  tunnelID: "550e8400-e29b-41d4-a716-446655440000"

  # Use existing secrets instead of inline values
  existingSecrets:
    apiToken:
      name: cloudflare-credentials
      key: api-token
    tunnelToken:
      name: cloudflare-tunnel-token
      key: tunnel-token

# cloudflared deployment settings
cloudflared:
  enabled: true
  replicas: 2

# Controller settings
controller:
  replicas: 2
  resources:
    limits:
      memory: 128Mi
    requests:
      cpu: 100m
      memory: 64Mi
```

Then install:

```bash
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller/chart \
  --namespace cloudflare-tunnel-system \
  --create-namespace \
  --values values.yaml
```

## Verify Installation

Check that the controller is running:

```bash
kubectl get pods --namespace cloudflare-tunnel-system
```

Expected output:

```text
NAME                                                      READY   STATUS    RESTARTS   AGE
cloudflare-tunnel-gateway-controller-7d8f9b6c5d-x2j9k     1/1     Running   0          30s
cloudflare-tunnel-cloudflared-5c4d8b7f6c-m8n3l            1/1     Running   0          30s
```

Check GatewayClass:

```bash
kubectl get gatewayclass cloudflare-tunnel
```

Expected output:

```text
NAME               CONTROLLER                                   ACCEPTED   AGE
cloudflare-tunnel  github.com/lexfrei/cloudflare-tunnel-gateway  True       30s
```

## Upgrading

To upgrade to a newer version:

```bash
helm upgrade cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller/chart \
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

    Uninstalling the Helm release will remove the controller and cloudflared
    pods. The tunnel configuration in Cloudflare will remain. To fully clean
    up, delete the tunnel from the Cloudflare dashboard.

## Alternative: External Secrets

For production deployments, consider using [external-secrets](https://external-secrets.io/)
to manage Cloudflare credentials:

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

After installation, proceed to [Quick Start](quickstart.md) to create your
first HTTPRoute.
