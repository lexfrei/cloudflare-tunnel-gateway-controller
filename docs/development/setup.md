# Development Setup

This guide covers setting up a development environment for the Cloudflare Tunnel Gateway Controller.

## Prerequisites

- Go 1.26.4 or later
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

The controller binary takes no credential environment variables. In v3, Cloudflare credentials and the tunnel UUID live in a Kubernetes Secret referenced by the GatewayClassConfig CRD, not in the controller process. The only mandatory flag is `--proxy-endpoints`, which points the controller at the in-process L7 proxy's config API.

```bash
# Run controller (--proxy-endpoints is required in v3)
./bin/controller \
  --proxy-endpoints=http://127.0.0.1:8081/config \
  --log-level=debug \
  --log-format=text
```

### In Cluster (Development)

```bash
# Install Gateway API CRDs
kubectl apply --filename https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml

# Create namespace
kubectl create namespace cloudflare-tunnel-system

# Create secret with credentials
kubectl create secret generic cloudflare-credentials \
  --namespace=cloudflare-tunnel-system \
  --from-literal=api-token="${CF_API_TOKEN}"

# Apply RBAC
kubectl apply --filename deploy/rbac/

# Run controller locally against cluster.
# --proxy-endpoints is mandatory in v3; point it at the proxy headless
# Service in the cluster (or run the proxy binary locally on :8081).
./bin/controller \
  --controller-name=cf.k8s.lex.la/tunnel-controller \
  --proxy-endpoints=http://127.0.0.1:8081/config \
  --log-level=debug
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

### Key Linter Settings

| Linter | Setting | Description |
|--------|---------|-------------|
| `funlen` | max 60 | Maximum function length |
| `gocyclo` | max 15 | Maximum cyclomatic complexity |
| `gosec` | enabled | Security checks |
| `nolintlint` | enabled | Requires `//nolint` explanations |

## Debugging

### Enable Debug Logging

```bash
./bin/controller --log-level=debug --log-format=text
```

### Inspect Controller State

```bash
# Check Gateway status
kubectl get gateway --all-namespaces --output yaml

# Check HTTPRoute status
kubectl get httproute --all-namespaces --output yaml

# Check controller logs
kubectl logs --namespace cloudflare-tunnel-system \
  deployment/cloudflare-tunnel-gateway-controller

# Check events
kubectl get events --namespace cloudflare-tunnel-system \
  --sort-by='.lastTimestamp'
```

### Debug with Delve

```bash
# Install delve
go install github.com/go-delve/delve/cmd/dlv@latest

# Run with debugger
dlv debug ./cmd/controller -- \
  --controller-name=cf.k8s.lex.la/tunnel-controller \
  --proxy-endpoints=http://127.0.0.1:8081/config \
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
   - Add environment variables the controller reads via viper (the `CF` prefix maps each flag, with `-` replaced by `_`): `CF_PROXY_ENDPOINTS` (required), and optionally `CF_CONTROLLER_NAME`, `CF_LOG_LEVEL`, `CF_LOG_FORMAT`
   - Set working directory to project root

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
go get -u github.com/cloudflare/cloudflare-go/v7@latest
go mod tidy
```

### Helm Chart Versioning

!!! warning "Do Not Manually Bump Versions"

    Do not manually change `version` or `appVersion` in `Chart.yaml`. The release workflow automatically sets both values based on the release tag. Manual changes will cause conflicts.

### Generate Godoc

```bash
godoc -http=:6060
# Open http://localhost:6060/pkg/github.com/lexfrei/cloudflare-tunnel-gateway-controller/
```
