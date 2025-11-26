# cloudflare-tunnel-gateway-controller

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.0.2](https://img.shields.io/badge/AppVersion-0.0.2-informational?style=flat-square)

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

* Kubernetes 1.25+
* Helm 3.0+
* Gateway API CRDs installed
* Cloudflare Tunnel created

### Install from OCI Registry

```bash
helm install cloudflare-tunnel-gateway-controller \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-chart \
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

## Multi-Tunnel Setup

To use multiple Cloudflare Tunnels in the same cluster, deploy multiple instances of the controller with different GatewayClass names:

```bash
# First tunnel for production apps
helm install controller-prod \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-chart \
  --namespace cloudflare-system \
  --set controller.gatewayClassName=cloudflare-tunnel-prod \
  --set cloudflare.tunnelId="PROD_TUNNEL_ID" \
  --set cloudflare.apiToken="PROD_API_TOKEN"

# Second tunnel for staging apps
helm install controller-staging \
  oci://ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller-chart \
  --namespace cloudflare-system \
  --set controller.gatewayClassName=cloudflare-tunnel-staging \
  --set cloudflare.tunnelId="STAGING_TUNNEL_ID" \
  --set cloudflare.apiToken="STAGING_API_TOKEN"
```

Each controller instance manages its own GatewayClass and associated Gateways/HTTPRoutes independently.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules for pod scheduling |
| controller | object | `{"clusterDomain":"","controllerName":"cf.k8s.lex.la/tunnel-controller","gatewayClassName":"cloudflare-tunnel","logFormat":"json","logLevel":"info"}` | Controller configuration |
| controller.clusterDomain | string | auto-detected from /etc/resolv.conf, fallback: cluster.local | Kubernetes cluster domain for service DNS resolution |
| controller.controllerName | string | `"cf.k8s.lex.la/tunnel-controller"` | Controller name for GatewayClass (must be unique in cluster) |
| controller.gatewayClassName | string | `"cloudflare-tunnel"` | GatewayClass name to watch |
| controller.logFormat | string | `"json"` | Log format (json, text) |
| controller.logLevel | string | `"info"` | Log level (debug, info, warn, error) |
| dnsConfig | object | `{}` | Custom DNS configuration for pod Example for custom DNS servers:   nameservers:     - 1.1.1.1     - 8.8.8.8   searches:     - cloudflare-tunnel-system.svc.cluster.local     - svc.cluster.local     - cluster.local   options:     - name: ndots       value: "2" |
| dnsPolicy | string | `""` | DNS policy for pod (ClusterFirst, Default, ClusterFirstWithHostNet, None) Use "None" with dnsConfig for custom DNS configuration |
| fullnameOverride | string | `""` | Override the full release name |
| gatewayClass | object | `{"create":true}` | GatewayClass configuration |
| gatewayClass.create | bool | `true` | Create GatewayClass resource |
| gatewayClassConfig | object | `{"cloudflareCredentialsSecretRef":{"key":"","name":"","namespace":""},"cloudflared":{"awg":{"interfacePrefix":"awg-cfd","secretName":""},"enabled":true,"namespace":"cloudflare-tunnel-system","protocol":"","replicas":1},"create":false,"name":"","tunnelID":"","tunnelTokenSecretRef":{"key":"","name":"","namespace":""}}` | GatewayClassConfig configuration This is the main configuration section for Cloudflare Tunnel settings. All tunnel-specific settings are now in GatewayClassConfig CRD. |
| gatewayClassConfig.cloudflareCredentialsSecretRef | object | `{"key":"","name":"","namespace":""}` | Reference to Secret containing Cloudflare API credentials (REQUIRED) The Secret must contain an "api-token" key with a valid Cloudflare API token. Optionally, it can contain an "account-id" key; if not present, account ID is auto-detected. |
| gatewayClassConfig.cloudflareCredentialsSecretRef.key | string | `""` | Key in the Secret containing the API token (defaults to "api-token") |
| gatewayClassConfig.cloudflareCredentialsSecretRef.name | string | `""` | Name of the Secret containing API credentials |
| gatewayClassConfig.cloudflareCredentialsSecretRef.namespace | string | `""` | Namespace of the Secret (defaults to release namespace) |
| gatewayClassConfig.cloudflared | object | `{"awg":{"interfacePrefix":"awg-cfd","secretName":""},"enabled":true,"namespace":"cloudflare-tunnel-system","protocol":"","replicas":1}` | Cloudflared deployment configuration |
| gatewayClassConfig.cloudflared.awg | object | `{"interfacePrefix":"awg-cfd","secretName":""}` | AmneziaWG sidecar configuration |
| gatewayClassConfig.cloudflared.awg.interfacePrefix | string | `"awg-cfd"` | AWG interface name prefix (kernel auto-numbers: prefix0, prefix1, etc.) |
| gatewayClassConfig.cloudflared.awg.secretName | string | `""` | Secret name containing AWG config (enables AWG sidecar) |
| gatewayClassConfig.cloudflared.enabled | bool | `true` | Enable cloudflared deployment management (default: true) |
| gatewayClassConfig.cloudflared.namespace | string | `"cloudflare-tunnel-system"` | Namespace for cloudflared deployment |
| gatewayClassConfig.cloudflared.protocol | string | `""` | Transport protocol (auto, quic, http2) |
| gatewayClassConfig.cloudflared.replicas | int | `1` | Number of cloudflared replicas |
| gatewayClassConfig.create | bool | `false` | Create GatewayClassConfig resource |
| gatewayClassConfig.name | string | `""` | Name of the GatewayClassConfig (defaults to release fullname) |
| gatewayClassConfig.tunnelID | string | `""` | Cloudflare Tunnel ID (REQUIRED) Get from: Zero Trust Dashboard > Networks > Tunnels Example: "550e8400-e29b-41d4-a716-446655440000" |
| gatewayClassConfig.tunnelTokenSecretRef | object | `{"key":"","name":"","namespace":""}` | Reference to Secret containing tunnel token (REQUIRED when cloudflared.enabled is true) The Secret must contain a "tunnel-token" key. |
| gatewayClassConfig.tunnelTokenSecretRef.key | string | `""` | Key in the Secret containing the tunnel token (defaults to "tunnel-token") |
| gatewayClassConfig.tunnelTokenSecretRef.name | string | `""` | Name of the Secret containing tunnel token |
| gatewayClassConfig.tunnelTokenSecretRef.namespace | string | `""` | Namespace of the Secret (defaults to release namespace) |
| healthProbes | object | `{"livenessProbe":{"enabled":true,"failureThreshold":3,"initialDelaySeconds":15,"periodSeconds":20,"timeoutSeconds":5},"readinessProbe":{"enabled":true,"failureThreshold":3,"initialDelaySeconds":5,"periodSeconds":10,"timeoutSeconds":3},"startupProbe":{"enabled":true,"failureThreshold":12,"initialDelaySeconds":0,"periodSeconds":5,"timeoutSeconds":3}}` | Health probes configuration |
| healthProbes.livenessProbe | object | `{"enabled":true,"failureThreshold":3,"initialDelaySeconds":15,"periodSeconds":20,"timeoutSeconds":5}` | Liveness probe configuration Restarts container if probe fails |
| healthProbes.readinessProbe | object | `{"enabled":true,"failureThreshold":3,"initialDelaySeconds":5,"periodSeconds":10,"timeoutSeconds":3}` | Readiness probe configuration Removes pod from service endpoints if probe fails |
| healthProbes.startupProbe | object | `{"enabled":true,"failureThreshold":12,"initialDelaySeconds":0,"periodSeconds":5,"timeoutSeconds":3}` | Startup probe configuration Gives controller time to initialize before liveness probe starts |
| image | object | `{"pullPolicy":"IfNotPresent","repository":"ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller","tag":""}` | Container image configuration |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| image.repository | string | `"ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller"` | Image repository |
| image.tag | string | `""` | Image tag (defaults to appVersion) |
| imagePullSecrets | list | `[]` | Image pull secrets for private registries |
| leaderElection | object | `{"enabled":false,"leaseName":"cloudflare-tunnel-gateway-controller-leader","namespace":""}` | Leader election configuration for high availability |
| leaderElection.enabled | bool | `false` | Enable leader election (required for running multiple replicas) |
| leaderElection.leaseName | string | `"cloudflare-tunnel-gateway-controller-leader"` | Name of the leader election lease |
| leaderElection.namespace | string | `""` | Namespace for leader election lease (defaults to release namespace) |
| nameOverride | string | `""` | Override the chart name |
| networkPolicy | object | `{"cloudflareIpRanges":{"ipv4":["173.245.48.0/20","103.21.244.0/22","103.22.200.0/22","103.31.4.0/22","141.101.64.0/18","108.162.192.0/18","190.93.240.0/20","188.114.96.0/20","197.234.240.0/22","198.41.128.0/17","162.158.0.0/15","104.16.0.0/13","104.24.0.0/14","172.64.0.0/13","131.0.72.0/22"],"ipv6":["2606:4700::/32","2803:f800::/32","2405:b500::/32","2405:8100::/32","2a06:98c0::/29","2c0f:f248::/32"]},"enabled":false,"ingress":{"from":[]}}` | NetworkPolicy configuration |
| networkPolicy.cloudflareIpRanges | object | `{"ipv4":["173.245.48.0/20","103.21.244.0/22","103.22.200.0/22","103.31.4.0/22","141.101.64.0/18","108.162.192.0/18","190.93.240.0/20","188.114.96.0/20","197.234.240.0/22","198.41.128.0/17","162.158.0.0/15","104.16.0.0/13","104.24.0.0/14","172.64.0.0/13","131.0.72.0/22"],"ipv6":["2606:4700::/32","2803:f800::/32","2405:b500::/32","2405:8100::/32","2a06:98c0::/29","2c0f:f248::/32"]}` | Cloudflare IP ranges for egress NetworkPolicy Source: https://www.cloudflare.com/ips/ Last updated: 2025-11-25 To update: curl https://www.cloudflare.com/ips-v4 && curl https://www.cloudflare.com/ips-v6 |
| networkPolicy.enabled | bool | `false` | Enable NetworkPolicy for controller pods |
| networkPolicy.ingress | object | `{"from":[]}` | Ingress source configuration |
| networkPolicy.ingress.from | list | `[]` | Allow ingress from specific namespaces/pods Example for monitoring namespace:   from:     - namespaceSelector:         matchLabels:           name: monitoring     - podSelector:         matchLabels:           app: prometheus |
| nodeSelector | object | `{}` | Node selector for pod scheduling |
| podAnnotations | object | `{}` | Annotations to add to pods |
| podDisruptionBudget | object | `{"enabled":false,"maxUnavailable":null,"minAvailable":1,"unhealthyPodEvictionPolicy":"IfHealthyBudget"}` | PodDisruptionBudget configuration for high availability |
| podDisruptionBudget.enabled | bool | `false` | Enable PodDisruptionBudget |
| podDisruptionBudget.maxUnavailable | string | `nil` | Maximum number of unavailable pods during disruptions Must not be used together with minAvailable |
| podDisruptionBudget.minAvailable | int | `1` | Minimum number of available pods during disruptions Must not be used together with maxUnavailable |
| podDisruptionBudget.unhealthyPodEvictionPolicy | string | `"IfHealthyBudget"` | Policy for evicting unhealthy pods (IfHealthyBudget, AlwaysAllow) Requires Kubernetes 1.26+ |
| podLabels | object | `{}` | Additional labels to add to pods |
| podSecurityContext | object | See values.yaml | Pod security context (secure defaults) |
| priorityClassName | string | `""` | Priority class name for pod scheduling priority |
| replicaCount | int | `1` | Number of controller replicas |
| resources | object | See values.yaml for recommended production defaults | Container resource requests and limits When resources is empty ({}), the chart will use recommended defaults. Specify explicit values to override defaults. |
| securityContext | object | See values.yaml | Container security context (secure defaults) |
| service | object | `{"annotations":{},"healthPort":8081,"metricsPort":8080,"type":"ClusterIP"}` | Service configuration |
| service.annotations | object | `{}` | Service annotations Example: Prometheus scraping annotations   prometheus.io/scrape: "true"   prometheus.io/port: "8080"   prometheus.io/path: "/metrics" |
| service.healthPort | int | `8081` | Health check endpoint port |
| service.metricsPort | int | `8080` | Metrics endpoint port |
| service.type | string | `"ClusterIP"` | Service type |
| serviceAccount | object | `{"annotations":{},"name":""}` | Service account configuration |
| serviceAccount.annotations | object | `{}` | Annotations to add to the service account |
| serviceAccount.name | string | `""` | The name of the service account to use If empty, uses the fullname template (release-name-chart-name) Override this if you want to use a pre-existing service account |
| serviceMonitor | object | `{"enabled":false,"interval":"","labels":{}}` | ServiceMonitor configuration for Prometheus Operator |
| serviceMonitor.enabled | bool | `false` | Enable ServiceMonitor creation |
| serviceMonitor.interval | string | `""` | Scrape interval (uses Prometheus default if empty) |
| serviceMonitor.labels | object | `{}` | Additional labels for ServiceMonitor (for Prometheus selector) |
| terminationGracePeriodSeconds | int | `30` | Termination grace period in seconds for graceful shutdown |
| tolerations | list | `[]` | Tolerations for pod scheduling |
| topologySpreadConstraints | list | `[]` | Topology spread constraints for pod distribution |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
