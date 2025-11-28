# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Kubernetes controller implementing Gateway API for Cloudflare Tunnel. Watches Gateway and HTTPRoute resources, automatically configures Cloudflare Tunnel ingress rules via API. Supports hot reload without cloudflared restart. Optional AmneziaWG (AWG) sidecar support for traffic obfuscation.

## Build and Development Commands

```bash
# Build binary
go build -o bin/controller ./cmd/controller

# Build with version info
go build -ldflags "-X main.Version=v0.0.1 -X main.Gitsha=$(git rev-parse HEAD)" -o bin/controller ./cmd/controller

# Run tests
go test -v -race -coverprofile=coverage.out ./...

# Run single test
go test -v -race ./internal/dns/... -run TestDetectClusterDomain

# Run linter (all errors must be fixed before committing)
golangci-lint run --timeout=5m

# Lint markdown files
markdownlint-cli2 '**/*.md'

# Build container
podman build --tag cloudflare-tunnel-gateway-controller:dev --file Containerfile .
```

## Helm Chart Commands

```bash
# Package chart
helm package charts/cloudflare-tunnel-gateway-controller

# Run helm-unittest
helm unittest charts/cloudflare-tunnel-gateway-controller

# Lint chart
helm lint charts/cloudflare-tunnel-gateway-controller

# Template locally (for debugging)
helm template test charts/cloudflare-tunnel-gateway-controller --values charts/cloudflare-tunnel-gateway-controller/examples/basic-values.yaml
```

## Architecture

### Controllers (controller-runtime based)

- **GatewayReconciler** (`internal/controller/gateway_controller.go`): Watches Gateway resources matching `cloudflare-tunnel` GatewayClass. Manages cloudflared deployment via Helm when `--manage-cloudflared` enabled. Updates Gateway status with tunnel CNAME address.

- **HTTPRouteReconciler** (`internal/controller/httproute_controller.go`): Watches HTTPRoute resources referencing managed Gateways. Performs full sync of all relevant routes to Cloudflare Tunnel configuration on any change. Updates HTTPRoute status.

### Custom Resource Definition

- **GatewayClassConfig** (`api/v1alpha1/`): Cluster-scoped CRD for configuring tunnel credentials and cloudflared deployment. Referenced by GatewayClass via `parametersRef`. Supports AWG sidecar configuration.

### Supporting Packages

- **internal/config/resolver.go**: Resolves GatewayClassConfig from GatewayClass parametersRef, reads credentials from Secrets, auto-detects account ID via Cloudflare API.

- **internal/ingress/builder.go**: Converts HTTPRoute specs to Cloudflare tunnel ingress rules. Handles hostnames, path matching (prefix/exact), backend service resolution.

- **internal/helm/manager.go**: Helm SDK wrapper for installing/upgrading cloudflared from OCI registry (`oci://ghcr.io/lexfrei/charts/cloudflare-tunnel`). Includes chart version caching.

- **internal/dns/detect.go**: Auto-detects Kubernetes cluster domain from `/etc/resolv.conf` search domains.

### Key Dependencies

- `sigs.k8s.io/controller-runtime` - Kubernetes controller framework
- `sigs.k8s.io/gateway-api` - Gateway API types
- `github.com/cloudflare/cloudflare-go/v4` - Cloudflare API client
- `helm.sh/helm/v3` - Helm SDK for cloudflared deployment
- `github.com/cockroachdb/errors` - Error wrapping

### Configuration

Configuration is provided via GatewayClassConfig CRD (referenced by GatewayClass parametersRef):

- `cloudflareCredentialsSecretRef` - Secret with `api-token` key (required)
- `tunnelID` - Cloudflare Tunnel UUID (required)
- `accountId` - Auto-detected if API token has single account access
- `tunnelTokenSecretRef` - Secret with `tunnel-token` key (required when cloudflared.enabled)
- `cloudflared.enabled` - Deploy cloudflared via Helm (default: true)
- `cloudflared.awg.secretName` - AWG config secret for traffic obfuscation

## Project Structure

```text
api/v1alpha1/            # GatewayClassConfig CRD types
cmd/controller/          # Entrypoint and CLI (cobra/viper)
internal/
  config/                # GatewayClassConfig resolver and credential handling
  controller/            # Kubernetes controllers (Gateway, HTTPRoute, GatewayClassConfig)
  dns/                   # Cluster domain auto-detection
  helm/                  # Helm SDK operations for cloudflared
  ingress/               # HTTPRoute → Cloudflare ingress rule conversion
charts/                  # Helm chart with helm-unittest tests
deploy/                  # Raw Kubernetes manifests for manual deployment
```

## Linting Configuration

golangci-lint v2 config in `.golangci.yaml`:

- `funlen` limit: 60 lines/statements
- `gocyclo/cyclop` complexity: 15
- All linters enabled by default with specific exclusions
- Test files have relaxed rules for funlen, dupl, complexity
