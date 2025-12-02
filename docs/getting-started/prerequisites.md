# Prerequisites

Before installing the Cloudflare Tunnel Gateway Controller, ensure you have
the following prerequisites in place.

## Kubernetes Cluster

You need a Kubernetes cluster with:

- Kubernetes version 1.25 or later
- `kubectl` configured to access the cluster
- Helm 3.x installed

## Gateway API CRDs

The controller requires Gateway API Custom Resource Definitions (CRDs) to be
installed in your cluster:

```bash
kubectl apply --filename https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml
```

!!! tip "Version Compatibility"

    The controller is tested with Gateway API v1.4.0. Using older versions
    may result in missing features or compatibility issues.

## Cloudflare Account

You need a Cloudflare account with:

- A domain managed by Cloudflare (for DNS)
- Access to Cloudflare Zero Trust dashboard

## Create Cloudflare Tunnel

Before deploying the controller, create a Cloudflare Tunnel:

1. Go to [Cloudflare Zero Trust Dashboard](https://one.dash.cloudflare.com/)
2. Navigate to **Networks** > **Tunnels**
3. Click **Create a tunnel**
4. Choose **Cloudflared** connector type
5. Name your tunnel and save:
    - **Tunnel ID** - UUID identifying the tunnel
    - **Tunnel Token** - Used by cloudflared to authenticate

!!! info "Controller vs cloudflared"

    The controller manages tunnel ingress configuration via API. You can either:

    - Let the controller deploy cloudflared automatically (default behavior)
    - Deploy cloudflared yourself using the tunnel token
      (`cloudflared.enabled: false` in Helm values)

## Cloudflare API Token

Create an API token at [Cloudflare API Tokens](https://dash.cloudflare.com/profile/api-tokens)
with the following permissions:

| Scope | Permission | Access |
|-------|------------|--------|
| Account | Cloudflare Tunnel | Edit |

!!! note "Account ID"

    Account ID is auto-detected from the API token when not explicitly provided
    (works if the token has access to a single account).

### Creating the API Token

1. Go to [Cloudflare API Tokens](https://dash.cloudflare.com/profile/api-tokens)
2. Click **Create Token**
3. Click **Create Custom Token**
4. Configure the token:
    - **Token name**: `cloudflare-tunnel-gateway-controller`
    - **Permissions**: Account > Cloudflare Tunnel > Edit
    - **Account Resources**: Include > Your Account
5. Click **Continue to summary** and **Create Token**
6. Copy the token value (you won't be able to see it again)

## Secrets Preparation

Prepare the following secrets for the controller:

### API Token Secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: cloudflare-credentials
  namespace: cloudflare-tunnel-system
type: Opaque
stringData:
  api-token: "YOUR_API_TOKEN"
```

### Tunnel Token Secret (if controller manages cloudflared)

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: cloudflare-tunnel-token
  namespace: cloudflare-tunnel-system
type: Opaque
stringData:
  tunnel-token: "YOUR_TUNNEL_TOKEN"
```

## Next Steps

Once you have all prerequisites in place, proceed to [Installation](installation.md).
