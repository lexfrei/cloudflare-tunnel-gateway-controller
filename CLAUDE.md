# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Kubernetes controller implementing Gateway API for Cloudflare Tunnel. Watches Gateway and HTTPRoute resources, automatically configures Cloudflare Tunnel ingress rules via API. Supports hot reload without cloudflared restart.

## Build and Development Commands

```bash
# Build binary
go build -o bin/controller ./cmd/controller

# Build with version info
go build -ldflags "-X main.Version=v0.0.1 -X main.Gitsha=$(git rev-parse HEAD)" -o bin/controller ./cmd/controller

# Run tests
go test -v -race -coverprofile=coverage.out ./...

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

### Supporting Packages

- **internal/ingress/builder.go**: Converts HTTPRoute specs to Cloudflare tunnel ingress rules. Handles hostnames, path matching (prefix/exact), backend service resolution.

- **internal/helm/manager.go**: Helm SDK wrapper for installing/upgrading cloudflared from OCI registry (`oci://ghcr.io/lexfrei/charts/cloudflare-tunnel`). Includes chart version caching.

### Key Dependencies

- `sigs.k8s.io/controller-runtime` - Kubernetes controller framework
- `sigs.k8s.io/gateway-api` - Gateway API types
- `github.com/cloudflare/cloudflare-go/v4` - Cloudflare API client
- `helm.sh/helm/v3` - Helm SDK for cloudflared deployment
- `github.com/cockroachdb/errors` - Error wrapping

### Configuration

Controller reads config via CLI flags or `CF_*` environment variables (viper). Key settings:
- `--tunnel-id` / `CF_TUNNEL_ID` (required)
- `--api-token` / `CF_API_TOKEN` (required)
- `--account-id` / `CF_ACCOUNT_ID` (auto-detected if single account)
- `--manage-cloudflared` - Deploy cloudflared via Helm
- `--tunnel-token` - Required when manage-cloudflared enabled

## Project Structure

```
cmd/controller/          # Entrypoint and CLI (cobra/viper)
internal/
  controller/            # Kubernetes controllers (Gateway, HTTPRoute)
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
