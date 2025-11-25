# Development Guide

This guide covers setting up a development environment for the Cloudflare Tunnel Gateway Controller.

## Prerequisites

- Go 1.25.4 or later
- kubectl configured with cluster access
- A Kubernetes cluster (kind, minikube, or remote)
- Cloudflare account with a tunnel configured
- golangci-lint (for linting)

## Quick Start

```bash
# Clone the repository
git clone https://github.com/lexfrei/cloudflare-tunnel-gateway-controller.git
cd cloudflare-tunnel-gateway-controller

# Install dependencies
go mod download

# Build
go build -o bin/controller ./cmd/controller

# Run tests
go test -v ./...

# Run linter
golangci-lint run
```

## Building

### Binary

```bash
# Development build
go build -o bin/controller ./cmd/controller

# Production build with version info
VERSION=v0.1.0
GITSHA=$(git rev-parse HEAD)
go build -ldflags "-s -w -X main.Version=${VERSION} -X main.Gitsha=${GITSHA}" \
  -trimpath -o bin/controller ./cmd/controller
```

### Container Image

```bash
# Build with podman
podman build --tag cloudflare-tunnel-gateway-controller:dev --file Containerfile .

# Build with docker
docker build --tag cloudflare-tunnel-gateway-controller:dev --file Containerfile .

# Multi-arch build
docker buildx build --platform linux/amd64,linux/arm64 \
  --tag ghcr.io/lexfrei/cloudflare-tunnel-gateway-controller:dev \
  --file Containerfile .
```

## Running Locally

### With kubeconfig

```bash
# Set required environment variables
export CF_TUNNEL_ID="your-tunnel-id"
export CF_API_TOKEN="your-api-token"

# Run controller
./bin/controller \
  --log-level=debug \
  --log-format=text \
  --gateway-class-name=cloudflare-tunnel
```

### In Cluster (Development)

```bash
# Install Gateway API CRDs
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml

# Create namespace
kubectl create namespace cloudflare-tunnel-system

# Create secret with credentials
kubectl create secret generic cloudflare-credentials \
  --namespace=cloudflare-tunnel-system \
  --from-literal=api-token="${CF_API_TOKEN}"

# Apply RBAC
kubectl apply -f deploy/rbac/

# Run controller locally against cluster
./bin/controller \
  --tunnel-id="${CF_TUNNEL_ID}" \
  --log-level=debug
```

## Testing

### Unit Tests

```bash
# Run all tests
go test -v ./...

# Run with race detector
go test -race ./...

# Run with coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

### Helm Chart Tests

```bash
# Install helm-unittest plugin
helm plugin install https://github.com/helm-unittest/helm-unittest

# Run chart tests
helm unittest charts/cloudflare-tunnel-gateway-controller

# Lint chart
helm lint charts/cloudflare-tunnel-gateway-controller
```

## Linting

The project uses golangci-lint with strict configuration:

```bash
# Run linter
golangci-lint run

# Run with timeout for CI
golangci-lint run --timeout=5m

# Auto-fix issues
golangci-lint run --fix
```

### Linter Configuration

Key settings in `.golangci.yaml`:

- `funlen`: Max 60 lines/statements per function
- `gocyclo`: Max complexity 15
- `gosec`: Security checks enabled
- `nolintlint`: Requires explanations for `//nolint` directives

## Debugging

### Enable Debug Logging

```bash
./bin/controller --log-level=debug --log-format=text
```

### Inspect Controller State

```bash
# Check Gateway status
kubectl get gateway -A -o yaml

# Check HTTPRoute status
kubectl get httproute -A -o yaml

# Check controller logs
kubectl logs -n cloudflare-tunnel-system deployment/cloudflare-tunnel-gateway-controller

# Check events
kubectl get events -n cloudflare-tunnel-system --sort-by='.lastTimestamp'
```

### Debug with Delve

```bash
# Install delve
go install github.com/go-delve/delve/cmd/dlv@latest

# Run with debugger
dlv debug ./cmd/controller -- \
  --tunnel-id="${CF_TUNNEL_ID}" \
  --log-level=debug
```

## IDE Setup

### VS Code

Recommended extensions:

- Go (golang.go)
- YAML (redhat.vscode-yaml)
- Kubernetes (ms-kubernetes-tools.vscode-kubernetes-tools)

Settings (`.vscode/settings.json`):

```json
{
  "go.lintTool": "golangci-lint",
  "go.lintFlags": ["--fast"],
  "go.useLanguageServer": true,
  "gopls": {
    "ui.semanticTokens": true
  }
}
```

### GoLand

1. Enable golangci-lint integration:
   - Settings → Go → Linters → Enable golangci-lint
2. Configure run configuration:
   - Add environment variables: `CF_TUNNEL_ID`, `CF_API_TOKEN`
   - Set working directory to project root

## Project Structure

```text
.
├── cmd/controller/       # Main entry point
├── internal/
│   ├── controller/       # Kubernetes controllers
│   ├── ingress/          # Route → Cloudflare conversion
│   └── helm/             # Helm SDK integration
├── charts/               # Helm chart
├── deploy/               # Raw Kubernetes manifests
├── docs/                 # Documentation
└── .github/              # CI/CD workflows
```

## Common Tasks

### Add a New Flag

1. Add flag in `cmd/controller/cmd/root.go`:

   ```go
   rootCmd.Flags().String("my-flag", "default", "Description")
   ```

2. Add viper binding and default:

   ```go
   viper.SetDefault("my-flag", "default")
   ```

3. Add to `controller.Config` struct in `internal/controller/manager.go`

4. Pass value in `runController()` function

### Update Dependencies

```bash
# Update all dependencies
go get -u ./...
go mod tidy

# Update specific dependency
go get -u github.com/cloudflare/cloudflare-go/v4@latest
go mod tidy
```

### Generate Godoc

```bash
# View locally
godoc -http=:6060
# Open http://localhost:6060/pkg/github.com/lexfrei/cloudflare-tunnel-gateway-controller/
```
