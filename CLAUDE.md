# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Feature delivery workflow

Every feature/fix follows the gates below. No shortcuts: each gate must close before the next opens.

1. **Branch.** Create `feat/...`, `fix/...`, etc. from a freshly pulled `master`. Never work on `master` directly.
2. **TDD implementation.** Red → Green → Refactor, strictly. Write a failing test FIRST, run it, watch it fail, then write the minimum code to pass. Repeat per behaviour. No implementation lands without a test that fails without it and passes with it. This applies even when the implementation feels obvious — "I'll write the tests after" silently drops edge cases.
3. **Local CI gates.** Run every check from `Pull Request Guidelines → Local CI Checks` for the files you touched; everything must pass before the next step.
4. **Double pre-merge review LGTM.** Run the project's pre-merge review pass against the branch (provided by the plugin enabled in `.claude/settings.json`; check that file for the active plugin). Iterate until it returns LGTM twice in a row. Any NOT LGTM resets the counter to zero — even cosmetic feedback that gets addressed must be followed by two consecutive LGTMs on top of the fixes.
5. **Open PR as draft.** `gh pr create --draft` with the body filled per `.github/pull_request_template.md`. Wait for CI green; address bot feedback if any.
6. **Mark ready + request review.** Once CI is green and any maintainer-side verification (e.g. running upstream Gateway API conformance against a real Cloudflare Tunnel) is complete, the PR moves out of draft and is merged with `--squash --delete-branch`.

Step 6's real-cluster verification is maintainer-only — it requires a Cloudflare account, a registered tunnel, and a Kubernetes cluster the maintainer can reach. Contributors finishing a branch should leave the PR in draft and ping the maintainer; the verification + merge is owned by them.

## Project Overview

Kubernetes controller implementing Gateway API for Cloudflare Tunnel. Watches Gateway and HTTPRoute resources, automatically configures Cloudflare Tunnel ingress rules via API. Supports hot reload without cloudflared restart. Ships a single L7 proxy data plane (embeds cloudflared transport in-process via the vendored fork's `OverrideProxy` hook).

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

# Build proxy binary
go build -o bin/proxy ./cmd/proxy

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

- **GatewayReconciler** (`internal/controller/gateway_controller.go`): Watches Gateway resources whose GatewayClass has a matching `spec.controllerName`. Resolves GatewayClassConfig for tunnel credentials. Updates Gateway status with tunnel CNAME address. Status-only since v3 — the in-process L7 proxy embeds cloudflared transport, so the controller no longer deploys a separate cloudflared instance.

- **HTTPRouteReconciler** (`internal/controller/httproute_controller.go`): Watches HTTPRoute resources referencing managed Gateways. Performs full sync of all relevant routes to Cloudflare Tunnel configuration on any change. Updates HTTPRoute status. Pushes config to L7 proxy replicas via ProxySyncer.

- **GRPCRouteReconciler** (`internal/controller/grpcroute_controller.go`): Watches GRPCRoute resources. Shares RouteSyncer with HTTPRouteReconciler for unified Cloudflare Tunnel sync, and pushes the merged HTTP+gRPC config to the L7 proxy via ProxySyncer — the in-process proxy serves gRPC at runtime (HTTP/2 POSTs to `/{service}/{method}`), so gRPC is not tunnel-only.

- **ProxySyncer** (`internal/controller/proxy_syncer.go`): Converts HTTPRoutes into proxy config and pushes to proxy endpoints via HTTP API. Resolves headless service DNS for endpoint discovery. Validates cross-namespace backends via ReferenceGrant.

### Custom Resource Definition

- **GatewayClassConfig** (`api/v1alpha1/`): Cluster-scoped CRD for configuring Cloudflare credentials and tunnel ID. Referenced by GatewayClass via `parametersRef`. Spec carries only `cloudflareCredentialsSecretRef`, optional `accountId`, and `tunnelID`. Proxy-side configuration (replicas, tunnel token, probes, access log, websocket timeouts) lives in Helm chart values, not in the CRD.

### Supporting Packages

- **internal/config/resolver.go**: Resolves GatewayClassConfig from GatewayClass parametersRef, reads credentials from Secrets, auto-detects account ID via Cloudflare API.

- **internal/ingress/builder.go**: Converts HTTPRoute specs to Cloudflare tunnel ingress rules. Handles hostnames, path matching (prefix/exact), backend service resolution.

- **internal/dns/detect.go**: Auto-detects Kubernetes cluster domain from `/etc/resolv.conf` search domains.

### V2 Proxy Data Plane

- **cmd/proxy/**: Standalone binary running cloudflared tunnel transport with an L7 reverse proxy. Supports tunnel mode (in-process via `OriginProxy`) and standalone mode (HTTP server for development).

- **internal/proxy/**: L7 reverse proxy implementation:
  - `router.go` — Thread-safe HTTP router with atomic config swap, compiled matchers, and weighted backend selection.
  - `handler.go` — Request handler with filter pipeline, per-route timeouts, and transport pool management.
  - `converter.go` — Converts Gateway API HTTPRoute resources to proxy config.
  - `filter.go` — Filter implementations: header modifier, redirect, URL rewrite, request mirror.
  - `config.go` — Proxy config types (rules, matches, filters, backends, timeouts).
  - `api.go` — Config API endpoints (PUT/GET /config, healthz, readyz) with Bearer token auth.
  - `pusher.go` — HTTP client for pushing config to proxy replicas concurrently.

- **internal/tunnel/**: Bootstraps cloudflared tunnel with in-process proxy override. Parses tunnel tokens, builds tunnel configuration, and integrates with the vendored cloudflared `OverrideProxy` hook. Exposes `GatewayOriginProxy` adapter for routing requests to the L7 proxy.

### Key Dependencies

- `sigs.k8s.io/controller-runtime` - Kubernetes controller framework
- `sigs.k8s.io/gateway-api` - Gateway API types
- `github.com/cloudflare/cloudflare-go/v7` - Cloudflare API client
- `github.com/cockroachdb/errors` - Error wrapping

### Cloudflared Fork

The project uses a fork of cloudflared: `github.com/lexfrei/cloudflared` (via `replace` directive in `go.mod`).

**Why:** The v2 in-process proxy needs to inject a custom `OriginProxy` into cloudflared's `Orchestrator`. Upstream cloudflared doesn't expose this capability, so the fork adds an `OverrideProxy` field to `Orchestrator` and modifies `GetOriginProxy()` to return it when set.

**Key architectural consequence:** Because the proxy hooks into cloudflared at the `OriginProxy` layer, ALL tunnel traffic flows through our in-process L7 proxy and bypasses cloudflared's native ingress rules. The Cloudflare-side tunnel API config (Cloudflare ingress rules) only serves DNS / edge routing purposes — actual L7 routing, hostname matching, path matching, filters etc. are all done by the in-cluster proxy. This means features like wildcard routes, regex path matching, and CORS work end-to-end regardless of what the Cloudflare Tunnel API itself supports.

**Fork maintenance:**

- Fork repo: `github.com/lexfrei/cloudflared`, branch `master`
- Base: upstream cloudflare/cloudflared at a pinned commit
- Patch: single commit adding `OverrideProxy` field to `orchestration/orchestrator.go`
- **NEVER patch vendor directly** — always update the fork and re-vendor
- When upgrading cloudflared version: rebase fork's master onto new upstream tag/commit, re-apply patch, update `replace` directive and pseudo-version in `go.mod`, then `go mod tidy && go mod vendor`
- **Canary tests post-rebase**: `TestVendoredCloudflaredUpgradeConstantsPinned` (`internal/tunnel/origin_vendor_drift_test.go`) fails loudly if the WebSocket signalling header / token constants drift. If it fires, audit `internal/tunnel/origin.go` `GatewayOriginProxy.ProxyHTTP` and any sibling behavioural tests (notably `TestGatewayOriginProxy_ProxyHTTP_WebSocketReinjectsHeaders`) before re-pinning the constants.

### Configuration

Configuration is split between the GatewayClassConfig CRD (controller-side) and the Helm chart values (proxy data plane):

GatewayClassConfig spec (referenced by GatewayClass parametersRef):

- `cloudflareCredentialsSecretRef` - Secret with `api-token` key (required)
- `tunnelID` - Cloudflare Tunnel UUID (required)
- `accountId` - Auto-detected if API token has single account access

Helm chart proxy values:

- `proxy.tunnelTokenSecretRef` - Secret with `tunnel-token` key (required); supplied directly to the proxy pods, not via the CRD
- `proxy.replicas`, `proxy.image.*`, `proxy.resources`, `proxy.healthProbes` - proxy deployment knobs
- `proxy.accessLog.*`, `proxy.websocket.*`, `proxy.authTokenSecretRef` - proxy runtime tuning

## Project Structure

```text
api/v1alpha1/            # GatewayClassConfig CRD types
cmd/controller/          # Controller entrypoint and CLI (cobra/viper)
cmd/proxy/               # L7 proxy binary entrypoint (standalone + tunnel modes)
internal/
  config/                # GatewayClassConfig resolver and credential handling
  controller/            # Kubernetes controllers (Gateway, HTTPRoute, GRPCRoute, ProxySyncer)
  dns/                   # Cluster domain auto-detection
  ingress/               # HTTPRoute → Cloudflare ingress rule conversion
  logging/               # Structured logging helpers (OpenTelemetry trace handler)
  cfmetrics/             # Cloudflare metrics collection
  proxy/                 # L7 reverse proxy (router, matcher, filter, config API, converter)
  referencegrant/        # ReferenceGrant validation for cross-namespace backends
  routebinding/          # Route-to-Gateway binding validation
  tunnel/                # cloudflared tunnel bootstrap and GatewayOriginProxy adapter
charts/                  # Helm chart with helm-unittest tests
deploy/                  # Raw Kubernetes manifests for manual deployment
test/e2e/                # E2E tests (custom, against live tunnel + proxy)
test/conformance/        # Official Gateway API conformance suite integration
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
├── guides/                  # L7 proxy, external-DNS, cross-namespace
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

## Build environment

- **Go version**: tracked in `go.mod` (currently Go 1.26.x). Newer builtins like `new(expr)` are used freely — there is no fallback to `ptr.To` helpers.
- **gopls quirk**: `gopls` versions older than the project's Go release sometimes flag `new(expr)` as `requires go1.26`. The real compiler accepts it; ignore that specific gopls noise.

## Design principles

- **Spec compliance is the default.** If the implementation deviates from the Gateway API spec, the deviation MUST be justified (with a code comment and, where user-visible, a `docs/gateway-api/limitations.md` entry). Otherwise refactor to match the spec. Initial design decisions that turn out to disagree with the spec get fixed, not defended — for example, the controller's route → Gateway binding was originally keyed on GatewayClass name; it was refactored to use `controllerName` because that's what the spec defines as the binding mechanism.
- **Validate the tunnel transport, not just `httptest`.** Any proxy feature that touches the request or response flow must be designed against the actual production path: `cloudflared.connection.http2RespWriter` (vendored fork), invoked through `internal/tunnel/origin.go` `GatewayOriginProxy.ProxyHTTP`, NOT `httptest.NewServer`. The two writers have materially different contracts — most importantly, the cloudflared HTTP/2 writer rejects `Hijack` when `statusWritten == false` and translates `WriteHeader(101)` to 200 on the wire. Past failures shipped past two rounds of pre-merge review because the test suite exercised only the HTTP/1.1 writer and missed the production hazard. Required workflow for any proxy feature that reads / writes / hijacks the response: (1) trace the request through `OverrideProxy` → `connection.ResponseWriter` semantics in the design notes; (2) cover the path with an integration test using `fakeCloudflaredRespWriter` (`internal/proxy/handler_tunnelfake_test.go`) in addition to any `httptest.NewServer`-based test; (3) re-vendor the cloudflared fork as part of any change that depends on its contract, and update both the production code and the fake. See `docs/development/proxy-architecture.md` `Tunnel-mode response writer semantics`.

## Linting Configuration

golangci-lint v2 config in `.golangci.yaml`:

- `funlen` limit: 60 lines/statements
- `gocyclo/cyclop` complexity: 15
- All linters enabled by default with specific exclusions
- Test files have relaxed rules for funlen, dupl, complexity

**`nolint` policy:** `nolint` directives are last resort. If the linter is flagging something fixable (long function, duplication, unused argument, error wrapping), fix the root cause — extract a helper, hoist a constant, refactor. Only legitimately-unfixable cases earn a `nolint`, and each one needs a comment explaining why the linter is wrong for this call site. Drive-by `nolint` adds get removed in review.

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

Labels follow the Kubernetes-style namespaced scheme. The authoritative source is `.github/labels.yml`, synced into the repo by `.github/workflows/labels.yaml` (EndBug/label-sync@v2). Edit the YAML and open a PR — the GitHub UI is effectively read-only because the next sync would overwrite manual edits.

When creating issues, apply at minimum one `kind/*`, one `area/*`, one `priority/*`, and one `triage/*` (or `lifecycle/*` if already active).

PRs are labelled automatically by `.github/workflows/pr-labels.yaml`: `kind/*` and `area/*` are derived from the Conventional Commit title (`feat(proxy): …` → `kind/feature` + `area/proxy`), and `size/*` is derived from the line-change count of non-vendored / non-generated files. The mapping lives in `.github/scripts/pr-labels.js` (unit-tested via stdlib `node --test`); update both there and `.github/labels.yml` when the taxonomy changes. The autolabeler is **additive** — it never removes a label, including labels it added itself in a previous run. Consequence: if you rename a PR from `feat(proxy): …` → `fix(controller): …`, both `area/proxy` and `area/controller` end up applied. Manual overrides (`kind/breaking-change`, `priority/*`) also stick. Remove a stale label via the GitHub UI when you rename a PR scope.

Trigger detail: `pull_request_target` fires on `opened` / `synchronize` / `reopened` / `edited`. `edited` covers title, body, and base-ref changes (not just the title); the labelling run is idempotent so this is cheap noise, not a bug. Renovate / Dependabot PRs also pass through and pick up `kind/cleanup` + `area/dependencies` + `size/*` on top of whatever the bot already applied (Renovate's own `area/dependencies` matches what the labeller would have derived, so the result is the same label, not a duplicate).

CI coverage for the autolabeler is asymmetric:

- `scripts.yaml`'s `pr-labels` job runs under `pull_request` with `paths:` filtered to `.github/scripts/pr-labels.{js,test.js}` + the workflow file. It checks out the **PR branch** and runs `node --test`, so a PR that edits the mapping module is validated against its own copy.
- `pr-labels.yaml`'s `apply` job runs under `pull_request_target` on every PR (no `paths:` filter). It checks out **master** for the secure-token reasons `pull_request_target` exists, so the labelling itself uses the deployed mapping module and is unaffected by JS changes in the PR being labelled.

Neither job is wired into branch-protection's required-status-checks today, so a maintainer can technically merge with them red — review them before merging a PR that touches the labeller code.

### kind/ — issue or PR type (required)

- `kind/bug` — Something isn't working
- `kind/feature` — New feature or request
- `kind/documentation` — Documentation improvements
- `kind/cleanup` — Tech debt, refactor, code or process cleanup
- `kind/regression` — Regression from a prior release
- `kind/flake` — Flaky test
- `kind/failing-test` — Consistently or frequently failing test
- `kind/api-change` — Adds, removes, or otherwise changes an API
- `kind/breaking-change` — Breaking API or behaviour change
- `kind/support` — Support question

### area/ — subsystem (required; extensible)

- `area/controller` — Kubernetes controller (`internal/controller`, GatewayReconciler, route binding)
- `area/proxy` — In-process L7 reverse proxy (`internal/proxy`, filters, transport)
- `area/tunnel` — cloudflared tunnel bridge (`internal/tunnel`, GatewayOriginProxy, vendored fork)
- `area/api` — GatewayClassConfig CRD and API types (`api/v1alpha1`)
- `area/helm` — Helm chart
- `area/ci` — CI workflows and release automation
- `area/docs` — docs site (`mkdocs`, `docs/`, README.md)
- `area/testing` — Test infrastructure (`test/`, conformance, e2e, integration)
- `area/dependencies` — Vendored deps, `go.mod`, Renovate updates
- `area/uncategorized` — Fallback when no concrete `area/*` fits; pick a real area during triage or expand the taxonomy

Add a new `area/*` when there are 3+ open issues on the topic.

Standalone labels not enumerated above (`epic`, `community`, `help wanted`, `good first issue`, `upstream-issue`, `automated`, `lgtm`, `ok-to-test`, `go`, `python`, `Container Available`) live in `.github/labels.yml` — that file is the authoritative full catalog.

### priority/ — urgency (required)

- `priority/critical-urgent` — Must be top priority right now
- `priority/important-soon` — Currently being staffed, ideally in time for the next release
- `priority/important-longterm` — Important long-term, may need multiple releases
- `priority/backlog` — General backlog priority

### triage/ — review state (required for new issues)

- `triage/needs-triage` — Needs maintainer triage
- `triage/accepted` — Ready to be actively worked on
- `triage/needs-information` — More information needed
- `triage/not-reproducible` — Cannot be reproduced as described
- `triage/duplicate` — Duplicate of another issue
- `triage/unresolved` — Cannot or will not be resolved

### lifecycle/ — once work starts

- `lifecycle/active` — Actively being worked on by a contributor
- `lifecycle/frozen` — Should not auto-close due to staleness
- `lifecycle/stale` — Stale due to no activity
- `lifecycle/rotten` — Aged beyond stale; will auto-close

### do-not-merge/ — PR merge blockers (Prow convention)

- `do-not-merge/work-in-progress` — PR is a work in progress
- `do-not-merge/hold` — Someone issued `/hold`; also used for "blocked by dependency"

### size/ — PR size (applied automatically by `.github/workflows/pr-labels.yaml`)

- `size/XS` — 0-9 lines
- `size/S` — 10-29 lines
- `size/M` — 30-99 lines
- `size/L` — 100-499 lines
- `size/XL` — 500-999 lines
- `size/XXL` — 1000+ lines

### security/ — security finding severity and status

`security`, `security/critical`, `security/high`, `security/medium`, `security/low`, `security/triage-needed`, `security/confirmed`, `security/false-positive`, `security/accepted-risk`, `security/in-progress`, `security/fixed`.

### Milestone

Always assign a milestone when creating issues. The current active milestone is `v3.0.0`; anything bound to an earlier release line goes in the corresponding `vX.Y.Z` milestone if one exists.

## Conformance Testing

### Overview

Official Gateway API conformance suite (`sigs.k8s.io/gateway-api/conformance` v1.5.0) runs against a kind cluster with a real Cloudflare Tunnel.

### Cloudflare Edge Constraints

**Host header validation**: Cloudflare edge returns 403 for any Host header with a domain not registered on the account. Conformance tests use `example.com`, `example.net`, `rewrite.example` etc. — all rejected by edge.

**Solution**: `X-Original-Host` header pattern:

- `TunnelRoundTripper` (test-only) always sets `Host: <edge-hostname>` so Cloudflare passes the request through
- Original test Host is sent via `X-Original-Host` header
- Proxy's `extractHost()` checks `X-Original-Host` first, falls back to `Host`
- This is NOT a production pattern — only needed because conformance tests use unregistered domains

**Other approaches investigated and rejected**:

- WARP/Zero Trust: edge still validates Host for public hostname routing
- `cloudflared access` proxy: still goes through edge, same Host validation
- Private Network (IP/CIDR routing): bypasses Host validation but requires WARP client on CI — impractical
- Direct tunnel connection: not possible, cloudflared is outbound-only

**gRPC tests**: Conformance suite's gRPC client (`grpc.NewClient`) dials directly to Gateway address (`*.cfargotunnel.com` AAAA → Cloudflare ULA fd10::/8, not routable). Custom gRPC dialer cannot be injected. These tests are skipped.

### Setup and Execution

```bash
# Full setup: fresh kind cluster + deploy + run tests
./hack/conformance-setup.sh --test

# Setup only (reuse for iterative testing)
./hack/conformance-setup.sh

# Setup + run the custom e2e suite (smoke-level; lighter than --test)
./hack/conformance-setup.sh --test-e2e

# Verify a PR's CI artifact: skip the local build, deploy the PR's published
# ttl.sh chart+images (the chart already pins the ttl.sh image refs). Add --test
# to also run the suite. ttl.sh artifacts expire 24h after the PR's CI ran.
./hack/conformance-setup.sh --use-ci-images <PR-number>

# Run all conformance tests on existing cluster (CONFORMANCE_TUNNEL_HOSTNAME is required)
CONFORMANCE_TUNNEL_HOSTNAME=<your-tunnel-hostname> go test -v -tags conformance -count=1 -timeout=60m -parallel 10 ./test/conformance/...

# Run specific failing test
CONFORMANCE_TUNNEL_HOSTNAME=<your-tunnel-hostname> go test -v -tags conformance -count=1 -timeout=10m ./test/conformance/... -run "HTTPRouteMatchingAcrossRoutes"

# Rebuild and redeploy images without recreating cluster
docker build --tag controller:dev --file Containerfile .
docker build --tag proxy:dev --file Containerfile.proxy .
kind load docker-image controller:dev --name <cluster-name>
kind load docker-image proxy:dev --name <cluster-name>
kubectl --context kind-<cluster-name> rollout restart deployment --namespace cloudflare-tunnel-system \
  cftunnel-cloudflare-tunnel-gateway-controller \
  cftunnel-cloudflare-tunnel-gateway-controller-proxy
```

### Key Files

- `test/conformance/conformance_test.go` — Test configuration, skip lists, feature sets
- `test/conformance/roundtripper.go` — Custom HTTP round-tripper for Cloudflare edge
- `hack/conformance-setup.sh` — Cluster setup script (kind + helm deploy)

### Environment Variables

- `CONFORMANCE_TUNNEL_HOSTNAME` — Edge hostname routing to the test tunnel (required; no default — see `.env.example`)
- `CONFORMANCE_GATEWAY_CLASS` — GatewayClass name (default: `cloudflare-tunnel`)
- `CONFORMANCE_REPORT_OUTPUT` — Path for YAML conformance report
