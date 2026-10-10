# Prerequisites

Before installing the Cloudflare Tunnel Gateway Controller, ensure you have the following prerequisites in place.

## Kubernetes Cluster

You need a Kubernetes cluster with:

- Kubernetes version 1.31 or later (see [Compatibility](#compatibility))
- `kubectl` configured to access the cluster
- Helm 3.x installed

## Compatibility

| Component | Supported |
| --- | --- |
| Kubernetes | 1.31+ |
| Gateway API CRDs | Standard channel (Gateway API v1.6.3) |

- **Kubernetes 1.31** is the `kubeVersion` constraint in the chart's `Chart.yaml`, and Helm refuses to install on an older cluster. The floor comes from the Gateway API standard bundle the controller is built against, which an older API server rejects in part. The bundle ships a ValidatingAdmissionPolicy under `admissionregistration.k8s.io/v1`, which the API server serves from 1.30. Its TLSRoute CRD has a validation rule that calls the CEL `isIP` function, and a newly created CRD's rules can use that function from 1.31: the library was added in 1.30, but a 1.30 API server compiles new rules against the 1.29 function set. Upstream Gateway API states the same 1.31 requirement for TLSRoute.
- The chart's own optional ValidatingAdmissionPolicies use the same `admissionregistration.k8s.io/v1` API, so the 1.31 floor covers them. They are rendered by `ruleNameUniquenessPolicy.enabled` and by `hostnameOwnershipPolicy.enabled` while `hostnameOwnershipPolicy.admissionPolicy` keeps its default of `true`.
- Upstream Gateway API supports at least the five most recent Kubernetes minor versions at the time of each release. The bundle's own version thresholds for this controller are described [below](#gateway-api-crds).

## Gateway API CRDs

The controller requires Gateway API Custom Resource Definitions (CRDs) to be installed in your cluster:

```bash
kubectl apply --filename https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.3/standard-install.yaml
```

As of Gateway API v1.6 the standard bundle also installs the `TCPRoute` and `UDPRoute` CRDs (GA); this controller does not implement them — Cloudflare Tunnel exposes HTTP(S) only — so they are inert.

!!! warning "Two version thresholds: v1.5.0 to start, v1.6.x to be supported"

    The controller watches `ListenerSet` resources as part of its core reconcile loop. The `listenersets.gateway.networking.k8s.io` CRD entered the **Standard** channel in Gateway API v1.5.0, so with any older bundle (v1.4.x or earlier) the manager cannot start at all because the watch target is missing.

    Being able to start is not the same as being supported: the controller is built against v1.6.3, and its GatewayClass `SupportedVersion` condition compares the installed bundle's `major.minor` against that version — any other minor, including v1.5.x, is reported as `SupportedVersion=False` with reason `UnsupportedVersion` while the controller keeps running.

    If you are on an older Gateway API bundle, apply the v1.6.3 standard bundle before installing this controller.

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

!!! info "Controller and proxy"

    The controller manages Cloudflare-side tunnel ingress configuration via API; tunnel traffic itself is terminated by the in-process L7 proxy that the Helm chart deploys alongside the controller. Supply the tunnel token via `proxy.tunnelTokenSecretRef` — see the [Helm values reference](../configuration/helm-values.md) for the full set of proxy knobs.

## Cloudflare API Token

Create an API token at [Cloudflare Account API Tokens](https://dash.cloudflare.com/?to=/:account/api-tokens) with the following permissions:

| Scope | Permission | Access |
|-------|------------|--------|
| Account | Cloudflare Tunnel | Edit |

The same permission covers checking a dedicated Gateway's tunnel claim, which reads that tunnel's connector token from the Cloudflare API — see [Proving a tunnel claim](../guides/per-gateway-isolation.md#proving-a-tunnel-claim).

!!! note "Account ID"

    Account ID is auto-detected from the API token when not explicitly provided (works if the token has access to a single account).

### Creating the API Token

1. Go to [Cloudflare Account API Tokens](https://dash.cloudflare.com/?to=/:account/api-tokens)
2. Click **Create Token**
3. Click **Get Started** (on the right-hand side of **Create Custom Token**)
4. Configure the token:
    - **Token name**: `cloudflare-tunnel-gateway-controller`
    - **Permissions**: Account > Cloudflare Tunnel > Edit
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

### Tunnel Token Secret

The L7 proxy pod consumes this Secret via the chart's `proxy.tunnelTokenSecretRef` value. The Secret must exist before `helm install` (the chart references it but does not create it).

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
