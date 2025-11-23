# Cloudflare Tunnel Gateway Controller

Kubernetes controller implementing Gateway API for Cloudflare Tunnel.

Enables routing traffic through Cloudflare Tunnel using standard Gateway API resources (Gateway, HTTPRoute).

## Features

- Standard Gateway API implementation (GatewayClass, Gateway, HTTPRoute)
- Hot reload of tunnel configuration (no cloudflared restart required)
- Multi-arch container images (amd64, arm64)
- Signed container images with cosign

## Prerequisites

- Kubernetes cluster with Gateway API CRDs installed
- Cloudflare account with Cloudflare Tunnel configured
- Cloudflare API token with tunnel permissions

### Cloudflare API Token Permissions

Create an API token with the following permissions:

- Account > Cloudflare Tunnel > Edit
- Account > Cloudflare Tunnel > Read

## Installation

### 1. Install Gateway API CRDs

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml
```

### 2. Create namespace

```bash
kubectl create namespace cloudflare-tunnel-system
```

### 3. Configure credentials

Edit `deploy/controller/deployment.yaml` and set your values in ConfigMap and Secret:

- `tunnel-id`: Your Cloudflare tunnel ID
- `api-token`: Your Cloudflare API token
- `account-id`: (optional) Your Cloudflare account ID - auto-detected if token has access to single account
- `cluster-domain`: Your cluster domain (default: `cluster.local`)

### 4. Deploy the controller

```bash
kubectl apply -f deploy/rbac/
kubectl apply -f deploy/controller/
```

### 5. Create GatewayClass and Gateway

```bash
kubectl apply -f deploy/samples/gatewayclass.yaml
kubectl apply -f deploy/samples/gateway.yaml
```

## Usage

Create an HTTPRoute to expose your service through Cloudflare Tunnel:

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
    - matches:
        - path:
            type: PathPrefix
            value: /
      backendRefs:
        - name: my-service
          port: 80
```

The controller will automatically update Cloudflare Tunnel configuration. Changes are applied instantly without restarting cloudflared.

## Configuration

| Flag | Environment Variable | Default | Description |
|------|---------------------|---------|-------------|
| `--account-id` | `CF_ACCOUNT_ID` | (auto-detect) | Cloudflare account ID (optional if token has access to single account) |
| `--tunnel-id` | `CF_TUNNEL_ID` | | Cloudflare tunnel ID |
| `--api-token` | `CF_API_TOKEN` | | Cloudflare API token |
| `--cluster-domain` | `CF_CLUSTER_DOMAIN` | `cluster.local` | Kubernetes cluster domain |
| `--gateway-class-name` | `CF_GATEWAY_CLASS_NAME` | `cloudflare-tunnel` | GatewayClass name to watch |
| `--controller-name` | `CF_CONTROLLER_NAME` | `cloudflare.com/tunnel-controller` | Controller name |
| `--metrics-addr` | `CF_METRICS_ADDR` | `:8080` | Metrics endpoint address |
| `--health-addr` | `CF_HEALTH_ADDR` | `:8081` | Health probe endpoint address |
| `--log-level` | `CF_LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |
| `--log-format` | `CF_LOG_FORMAT` | `json` | Log format (json, text) |

## Gateway API Support

### Supported Features

| Resource | Field | Support |
|----------|-------|---------|
| HTTPRoute | `hostnames` | Yes |
| HTTPRoute | `rules.matches.path` (PathPrefix, Exact) | Yes |
| HTTPRoute | `rules.backendRefs` | Yes |
| Gateway | `gatewayClassName` | Yes |
| Gateway | `listeners` | Yes (informational) |

### Unsupported Features

The following Gateway API features are not supported due to Cloudflare Tunnel limitations:

- Header matching (`rules.matches.headers`)
- Query parameter matching (`rules.matches.queryParams`)
- Method matching (`rules.matches.method`)
- Request/Response header modification
- Request redirects

## Architecture

```text
┌─────────────┐    watch     ┌─────────────────────────┐
│ HTTPRoute   │─────────────>│ CF-Tunnel-Gateway       │
│ CRD         │              │ Controller              │
└─────────────┘              └───────────┬─────────────┘
                                         │ Cloudflare API
┌─────────────┐    watch                 │
│ Gateway     │─────────────>│           │
│ CRD         │              │           ▼
└─────────────┘              │  ┌─────────────────┐
                             │  │ Cloudflare API  │
                             │  │ (tunnel config) │
                             │  └────────┬────────┘
                             │           │ push (automatic)
                             │           ▼
                             │  ┌─────────────────┐
                             │  │ cloudflared     │
                             │  │ (hot update!)   │
                             │  └─────────────────┘
```

## License

MIT License
