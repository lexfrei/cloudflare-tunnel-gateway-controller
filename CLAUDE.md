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

# Generate README from values.yaml (REQUIRED before commit)
helm-docs charts/cloudflare-tunnel-gateway-controller

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

## Testing Standards

### Approach

- **TDD (Test-Driven Development)**: Write tests first, then implementation
- Follow RED → GREEN → REFACTOR cycle
- Commit test and implementation together per feature

### Testing Libraries

- `github.com/stretchr/testify/assert` - Assertions
- `github.com/stretchr/testify/require` - Fatal assertions (stops test on failure)
- `sigs.k8s.io/controller-runtime/pkg/client/fake` - Fake Kubernetes client for unit tests
- `sigs.k8s.io/controller-runtime/pkg/envtest` - Integration tests with real API server

### Test Patterns

- **Table-driven tests**: Use `[]struct{}` with named test cases
- **Parallel execution**: Always use `t.Parallel()` at test and subtest level
- **Fake client setup**: Create scheme, register types, build fake client
- **Helper functions**: Extract common setup (e.g., `setupFakeClient()`)

### Example Structure

```go
func TestFeature(t *testing.T) {
    t.Parallel()

    tests := []struct {
        name     string
        input    InputType
        expected OutputType
    }{
        {name: "case 1", input: ..., expected: ...},
        {name: "case 2", input: ..., expected: ...},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()
            // test logic
            require.NoError(t, err)
            assert.Equal(t, tt.expected, actual)
        })
    }
}
```

### Running Tests

```bash
# All tests with race detection
go test -race ./...

# Single package
go test -v -race ./internal/routebinding/...

# Single test by name
go test -v -race ./internal/controller/... -run TestHTTPRouteReconciler

# With coverage
go test -race -coverprofile=coverage.out ./...
```

## Documenting External Service Limitations

**CRITICAL: When discovering limitations of external services (Cloudflare API, Tunnel behavior, etc.), document them IMMEDIATELY.**

### Why This Matters

This controller integrates with Cloudflare Tunnel, which has its own behaviors and limitations that may differ from Gateway API expectations. These limitations:

- Are NOT bugs in our controller
- Cannot be fixed by us
- Must be documented for users
- Require workarounds in tests and usage examples

### When You Discover a Limitation

1. **Stop and document immediately** — don't defer documentation
2. **Add to `docs/gateway-api/limitations.md`** with:
   - Clear description of the limitation
   - Example of unexpected behavior
   - Workaround if available
3. **Add brief mention to README.md** in "Known Limitations" section
4. **Update tests** to work around the limitation (not test impossible behavior)
5. **Add code comments** where the limitation affects implementation

### Example: Cloudflare Tunnel Path Matching

Discovered during conformance testing:

- Cloudflare Tunnel does NOT support true exact path matching
- Paths with common prefixes may route unexpectedly

Documented in:

- `docs/gateway-api/limitations.md` — full explanation with workarounds
- `README.md` — brief "Known Limitations" section
- `conformance/cftunnel_test.go` — comments explaining test design choices

### Key Principle

If the controller generates correct configuration but external service behaves unexpectedly — that's a limitation to document, not a bug to fix.

## Documentation

Project documentation uses MkDocs with Material theme. Live site: https://cf.k8s.lex.la

### Commands

```bash
# Install dependencies
pip install --requirement requirements-docs.txt

# Local preview server
mkdocs serve

# Build static site
mkdocs build

# Lint markdown
markdownlint-cli2 '**/*.md'
```

### Structure

```text
docs/
├── index.md                 # Homepage
├── getting-started/         # Installation, prerequisites, quickstart
├── configuration/           # Controller options, Helm values, GatewayClassConfig
├── gateway-api/             # HTTPRoute, GRPCRoute, ReferenceGrant, limitations
├── guides/                  # AWG sidecar, external-DNS, cross-namespace
├── operations/              # Troubleshooting, metrics, manual installation
├── development/             # Setup, architecture, contributing, testing
└── reference/               # Helm chart, CRD reference, security
```

### Writing Guidelines

- **Section structure**: Each section must have `index.md` as landing page
- **Navigation**: Register all new pages in `nav:` section of `mkdocs.yml`
- **Diagrams**: Use Mermaid for architecture and flow diagrams
- **Code blocks**: Use syntax highlighting with language identifier
- **Admonitions**: Use `!!! note`, `!!! warning`, `!!! danger` for callouts
- **Links**: Use relative paths for internal links (`../configuration/helm-values.md`)

### Documentation TDD

**CRITICAL: Apply TDD methodology to documentation with obsessive attention to detail.**

**NEVER work directly on master branch. Create a feature branch for all documentation changes.**

Before writing any documentation:

1. **Verify every command works**
   - Run each command yourself before documenting
   - Test on clean environment when possible
   - Document exact versions and prerequisites

2. **Validate all code examples**
   - Copy-paste and execute every code snippet
   - Verify output matches documented expectations
   - Test edge cases mentioned in documentation

3. **Check all links and references**
   - Click every internal link
   - Verify external URLs are accessible
   - Confirm file paths exist

4. **Test the user journey**
   - Follow your own documentation step-by-step
   - Assume zero prior knowledge
   - Note every missing step or assumption

5. **Build and preview locally**
   - Run `mkdocs serve` before committing
   - Check rendering of all changed pages
   - Verify navigation and search work correctly

**If a command fails, a link is broken, or a step is missing — fix it before committing.**

## Linting Configuration

golangci-lint v2 config in `.golangci.yaml`:

- `funlen` limit: 60 lines/statements
- `gocyclo/cyclop` complexity: 15
- All linters enabled by default with specific exclusions
- Test files have relaxed rules for funlen, dupl, complexity

## Pull Request Guidelines

### Local CI Checks

**CRITICAL: Run CI-equivalent checks locally before each push, not just before PR creation.**

Run checks relevant to the files you changed:

| Changed Files | Required Checks |
|---------------|-----------------|
| `*.go` | `go test -race ./...` and `golangci-lint run --timeout=5m` |
| `charts/**` | `helm unittest`, `helm lint`, `helm-docs` |
| `**/*.md` | `markdownlint-cli2 '**/*.md'` |
| `docs/**` | `mkdocs build --strict` |

Quick reference commands:

```bash
# Go code changes
go test -race ./... && golangci-lint run --timeout=5m

# Helm chart changes
helm unittest charts/cloudflare-tunnel-gateway-controller && \
helm lint charts/cloudflare-tunnel-gateway-controller && \
helm-docs charts/cloudflare-tunnel-gateway-controller

# Markdown changes
markdownlint-cli2 '**/*.md'

# Documentation site
mkdocs build --strict
```

**Why this matters:** CI failures after push waste time, trigger unnecessary notifications, and delay reviews. Catching issues locally is faster and cheaper.

### Pre-PR Checklist

Before creating a PR, verify all checklist items from `.github/pull_request_template.md`:

1. **Testing**
   - All tests pass locally (`go test ./...`)
   - Linters pass locally (`golangci-lint run`)
   - Markdown linting passes (`markdownlint-cli2 '**/*.md'`)
   - Helm tests pass (`helm unittest charts/cloudflare-tunnel-gateway-controller`)
   - Helm lint passes (`helm lint charts/cloudflare-tunnel-gateway-controller`)
   - Helm README is up to date (`helm-docs charts/cloudflare-tunnel-gateway-controller`)
   - Manual testing completed (if applicable)

2. **Documentation**
   - README updated (if needed)
   - Code comments added for complex logic
   - CLAUDE.md updated (if workflow/standards changed)

3. **Code Quality**
   - Commit messages follow semantic format (`type(scope): description`)
   - No secrets or credentials in code
   - Breaking changes documented (if any)

### PR Creation

- Use template from `.github/pull_request_template.md`
- Fill all sections completely
- Check all applicable checkboxes honestly
- Do NOT check boxes for items not actually completed

## GitHub Issue Labels

When creating issues, apply labels from these categories:

### Type (required)

- `bug` — Something isn't working
- `enhancement` — New feature or request
- `documentation` — Documentation improvements
- `test` — Test coverage
- `ci` — CI/CD and automation
- `security` — Security-related

### Area (required)

- `area/controller` — Controller code
- `area/helm` — Helm chart
- `area/api` — CRD and API types
- `area/docs` — Documentation

### Priority (required)

- `priority/critical` — Blocks release, needs immediate attention
- `priority/high` — Important for milestone
- `priority/medium` — Should be done for milestone
- `priority/low` — Nice to have, can defer

### Status (required)

- `status/needs-triage` — Requires analysis
- `status/needs-design` — Requires design/RFC
- `status/ready` — Ready to work on
- `status/in-progress` — Currently being worked on
- `status/blocked` — Blocked by dependency
- `status/needs-info` — Waiting for clarification
- `status/needs-review` — Waiting for review/feedback

### Size (required)

- `size/XS` — < 1 hour
- `size/S` — 1-4 hours
- `size/M` — 1-2 days
- `size/L` — 3-5 days
- `size/XL` — > 1 week

### Milestone

Always assign a milestone when creating issues (e.g., `v1.0.0`).
