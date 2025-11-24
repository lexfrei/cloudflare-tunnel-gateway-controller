# cloudflare-tunnel-gateway-controller

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.1](https://img.shields.io/badge/AppVersion-0.0.1-informational?style=flat-square)

## Status

[![Lint and Test Chart](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/test-chart.yaml/badge.svg)](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/test-chart.yaml)
[![Release](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/release.yaml/badge.svg)](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/actions/workflows/release.yaml)

Kubernetes Gateway API controller for Cloudflare Tunnel

**Homepage:** <https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| lexfrei | <f@lex.la> | <https://github.com/lexfrei> |

## Source Code

* <https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/>

## Requirements

Kubernetes: `>=1.25.0-0`

## Installation

### Prerequisites

- Kubernetes 1.25+
- Helm 3.0+
- Gateway API CRDs installed
- Cloudflare Tunnel created

### Install from OCI Registry

```bash
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller \
  --version 0.1.0 \
  --namespace cloudflare-tunnel-system \
  --create-namespace \
  --set cloudflare.tunnelId="YOUR_TUNNEL_ID" \
  --set cloudflare.apiToken="YOUR_API_TOKEN"
```

### Verify Chart Signature

Charts are signed with cosign. Verify before installing:

```bash
cosign verify ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller:0.1.0 \
  --certificate-identity-regexp="https://github.com/lexfrei/cloudflare-tunnel-gateway-controller" \
  --certificate-oidc-issuer="https://token.actions.githubusercontent.com"
```

## Configuration Examples

Complete example values files are available in the [examples/](https://github.com/lexfrei/cloudflare-tunnel-gateway-controller/tree/master/charts/cloudflare-tunnel-gateway-controller/examples) directory:

| Example | Description |
|---------|-------------|
| [basic-values.yaml](examples/basic-values.yaml) | Minimal configuration |
| [external-secrets-values.yaml](examples/external-secrets-values.yaml) | Using existing Kubernetes Secret |
| [managed-cloudflared-values.yaml](examples/managed-cloudflared-values.yaml) | Helm-managed cloudflared deployment |
| [awg-sidecar-values.yaml](examples/awg-sidecar-values.yaml) | AmneziaWG VPN sidecar |
| [production-values.yaml](examples/production-values.yaml) | Production-ready with HA, monitoring, and security |

### Quick Start

```yaml
cloudflare:
  tunnelId: "your-tunnel-id"
  apiToken: "your-api-token"
```

## Usage

After installation, create Gateway and HTTPRoute resources:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: cloudflare-gateway
spec:
  gatewayClassName: cloudflare-tunnel
  listeners:
    - name: https
      protocol: HTTPS
      port: 443
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: my-app
spec:
  parentRefs:
    - name: cloudflare-gateway
  hostnames:
    - "app.example.com"
  rules:
    - backendRefs:
        - name: my-app-service
          port: 80
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules for pod scheduling |
| awg | object | `{"interfaceName":"awg-cfd-gw-ctrl0","secretName":""}` | AmneziaWG sidecar configuration for traffic routing through VPN |
| awg.interfaceName | string | `"awg-cfd-gw-ctrl0"` | AWG interface name (unique to avoid conflicts) |
| awg.secretName | string | `""` | Secret name containing AWG config (enables AWG sidecar) |
| cloudflare | object | `{"accountId":"","apiToken":"","apiTokenSecretName":"","tunnelId":""}` | Cloudflare configuration |
| cloudflare.accountId | string | `""` | Cloudflare Account ID (auto-detected if not specified) |
| cloudflare.apiToken | string | `""` | Cloudflare API token with Tunnel permissions Required scopes: Account.Cloudflare Tunnel:Edit, Zone.DNS:Edit |
| cloudflare.apiTokenSecretName | string | `""` | Existing secret name containing API token (key: api-token) If set, apiToken value is ignored |
| cloudflare.tunnelId | string | `""` | Cloudflare Tunnel ID (required at install time) Get from: Zero Trust Dashboard > Networks > Tunnels |
| controller | object | `{"clusterDomain":"cluster.local","controllerName":"cloudflare.com/tunnel-controller","gatewayClassName":"cloudflare-tunnel","logFormat":"json","logLevel":"info"}` | Controller configuration |
| controller.clusterDomain | string | `"cluster.local"` | Kubernetes cluster domain for service DNS resolution |
| controller.controllerName | string | `"cloudflare.com/tunnel-controller"` | Controller name for GatewayClass (must be unique in cluster) |
| controller.gatewayClassName | string | `"cloudflare-tunnel"` | GatewayClass name to watch |
| controller.logFormat | string | `"json"` | Log format (json, text) |
| controller.logLevel | string | `"info"` | Log level (debug, info, warn, error) |
| fullnameOverride | string | `""` | Override the full release name |
| gatewayClass | object | `{"create":true}` | GatewayClass configuration |
| gatewayClass.create | bool | `true` | Create GatewayClass resource |
| image | object | `{"pullPolicy":"IfNotPresent","repository":"ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller","tag":""}` | Container image configuration |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| image.repository | string | `"ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller"` | Image repository |
| image.tag | string | `""` | Image tag (defaults to appVersion) |
| imagePullSecrets | list | `[]` | Image pull secrets for private registries |
| manageCloudflared | object | `{"enabled":false,"namespace":"cloudflare-tunnel-system","protocol":"","tunnelToken":"","tunnelTokenSecretName":""}` | Cloudflared deployment management via Helm |
| manageCloudflared.enabled | bool | `false` | Deploy and manage cloudflared via Helm |
| manageCloudflared.namespace | string | `"cloudflare-tunnel-system"` | Namespace for cloudflared deployment |
| manageCloudflared.protocol | string | `""` | Transport protocol (auto, quic, http2) |
| manageCloudflared.tunnelToken | string | `""` | Tunnel token for remote-managed mode Get from: Zero Trust Dashboard > Networks > Tunnels > Configure |
| manageCloudflared.tunnelTokenSecretName | string | `""` | Existing secret name containing tunnel token (key: tunnel-token) |
| nameOverride | string | `""` | Override the chart name |
| networkPolicy | object | `{"enabled":false}` | NetworkPolicy configuration |
| networkPolicy.enabled | bool | `false` | Enable NetworkPolicy for controller pods |
| nodeSelector | object | `{}` | Node selector for pod scheduling |
| podAnnotations | object | `{}` | Annotations to add to pods |
| podLabels | object | `{}` | Additional labels to add to pods |
| podSecurityContext | object | See values.yaml | Pod security context (secure defaults) |
| priorityClassName | string | `""` | Priority class name for pod scheduling priority |
| replicaCount | int | `1` | Number of controller replicas |
| resources | object | No resources specified | Container resource requests and limits |
| securityContext | object | See values.yaml | Container security context (secure defaults) |
| service | object | `{"healthPort":8081,"metricsPort":8080,"type":"ClusterIP"}` | Service configuration |
| service.healthPort | int | `8081` | Health check endpoint port |
| service.metricsPort | int | `8080` | Metrics endpoint port |
| service.type | string | `"ClusterIP"` | Service type |
| serviceAccount | object | `{"annotations":{},"name":""}` | Service account configuration |
| serviceAccount.annotations | object | `{}` | Annotations to add to the service account |
| serviceAccount.name | string | `""` | The name of the service account to use If not set, a name is generated using the fullname template |
| serviceMonitor | object | `{"enabled":false,"interval":"","labels":{}}` | ServiceMonitor configuration for Prometheus Operator |
| serviceMonitor.enabled | bool | `false` | Enable ServiceMonitor creation |
| serviceMonitor.interval | string | `""` | Scrape interval (uses Prometheus default if empty) |
| serviceMonitor.labels | object | `{}` | Additional labels for ServiceMonitor (for Prometheus selector) |
| tolerations | list | `[]` | Tolerations for pod scheduling |
| topologySpreadConstraints | list | `[]` | Topology spread constraints for pod distribution |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
