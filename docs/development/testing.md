# Testing

This guide covers testing standards and practices for the Cloudflare Tunnel Gateway Controller.

## Running Tests

### Unit Tests

```bash
# Run all tests
go test -v ./...

# Run with race detector
go test -race ./...

# Run specific package
go test -v -race ./internal/controller/...

# Run specific test
go test -v -race ./internal/controller/... -run TestHTTPRouteReconciler
```

### Coverage

```bash
# Generate coverage report
go test -coverprofile=coverage.out ./...

# View coverage in browser
go tool cover -html=coverage.out

# View coverage in terminal
go tool cover -func=coverage.out
```

### Helm Chart Tests

CI is pinned to Helm v4.2.0. The `helm-unittest` plugin install below uses Helm 4 syntax (`--verify=false`); the test run and chart install themselves work on Helm 3+ as well.

```bash
# Install helm-unittest plugin (matches CI).
# helm-unittest installs from a .git source, which Helm 4's installer
# cannot verify — signature verification only applies to .tgz archives.
# Helm 4 defaults plugin install to --verify=true, so a .git source
# hard-errors unless you opt out. Plugin verification (and the --verify
# flag) did not exist before Helm 4.
helm plugin install https://github.com/helm-unittest/helm-unittest.git --verify=false

# Run chart tests
helm unittest charts/cloudflare-tunnel-gateway-controller

# Lint chart
helm lint charts/cloudflare-tunnel-gateway-controller

# Template locally (for debugging)
helm template test charts/cloudflare-tunnel-gateway-controller \
  --values charts/cloudflare-tunnel-gateway-controller/examples/basic-values.yaml
```

## Test Patterns

### Table-Driven Tests

Use table-driven tests with named test cases:

```go
func TestFeature(t *testing.T) {
    t.Parallel()

    tests := []struct {
        name     string
        input    InputType
        expected OutputType
        wantErr  bool
    }{
        {
            name:     "valid input",
            input:    InputType{...},
            expected: OutputType{...},
            wantErr:  false,
        },
        {
            name:     "invalid input",
            input:    InputType{...},
            expected: OutputType{},
            wantErr:  true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel()

            result, err := DoSomething(tt.input)

            if tt.wantErr {
                require.Error(t, err)
                return
            }

            require.NoError(t, err)
            assert.Equal(t, tt.expected, result)
        })
    }
}
```

### Parallel Execution

Always use `t.Parallel()` at test and subtest level:

```go
func TestSomething(t *testing.T) {
    t.Parallel()  // Mark test as parallel

    t.Run("subtest", func(t *testing.T) {
        t.Parallel()  // Mark subtest as parallel
        // ...
    })
}
```

### Fake Client Setup

Use controller-runtime fake client for unit tests:

```go
func TestController(t *testing.T) {
    // Create scheme with all required types
    scheme := runtime.NewScheme()
    _ = clientgoscheme.AddToScheme(scheme)
    _ = gatewayv1.Install(scheme)

    // Create fake client with initial objects
    client := fake.NewClientBuilder().
        WithScheme(scheme).
        WithObjects(
            &gatewayv1.Gateway{...},
            &gatewayv1.HTTPRoute{...},
        ).
        Build()

    // Create reconciler with fake client
    reconciler := &HTTPRouteReconciler{
        Client: client,
        Scheme: scheme,
    }

    // Test reconciliation
    result, err := reconciler.Reconcile(ctx, ctrl.Request{...})
    require.NoError(t, err)
}
```

## Test Libraries

| Library | Usage |
|---------|-------|
| `github.com/stretchr/testify/assert` | Soft assertions (test continues) |
| `github.com/stretchr/testify/require` | Hard assertions (test stops) |
| `sigs.k8s.io/controller-runtime/pkg/client/fake` | Fake Kubernetes client |
| `sigs.k8s.io/controller-runtime/pkg/envtest` | Integration tests |

### Assert vs Require

```go
// Use require for setup and critical checks (stops test on failure)
require.NoError(t, err, "setup should succeed")

// Use assert for multiple checks (test continues)
assert.Equal(t, expected.Name, actual.Name)
assert.Equal(t, expected.Port, actual.Port)
```

## Test Organization

### File Naming

| Pattern | Description |
|---------|-------------|
| `*_test.go` | Test files (standard Go convention) |
| `*_internal_test.go` | Tests for unexported functions (same package) |

### Test Helpers

Extract common setup into helper functions:

```go
func setupFakeClient(t *testing.T, objs ...client.Object) client.Client {
    t.Helper()

    scheme := runtime.NewScheme()
    require.NoError(t, clientgoscheme.AddToScheme(scheme))
    require.NoError(t, gatewayv1.Install(scheme))

    return fake.NewClientBuilder().
        WithScheme(scheme).
        WithObjects(objs...).
        Build()
}
```

## What to Test

### Unit Tests

- Business logic functions
- Input validation
- Error handling paths
- Edge cases (empty inputs, nil values)

### Integration Tests

- Controller reconciliation loops
- Kubernetes API interactions
- Cloudflare API interactions (mocked)

### Not Tested

- Generated code (CRD types, mocks)
- Third-party library internals

## Mocking

### External Services

Mock external services (Cloudflare API) in unit tests:

```go
type mockCloudflareClient struct {
    tunnelConfig *cloudflare.TunnelConfiguration
    err          error
}

func (m *mockCloudflareClient) UpdateTunnelConfiguration(
    ctx context.Context,
    config cloudflare.TunnelConfiguration,
) error {
    m.tunnelConfig = &config
    return m.err
}
```

### Time-Dependent Tests

Use injectable time for deterministic tests:

```go
type Clock interface {
    Now() time.Time
}

// In production
type RealClock struct{}
func (RealClock) Now() time.Time { return time.Now() }

// In tests
type FakeClock struct {
    CurrentTime time.Time
}
func (c FakeClock) Now() time.Time { return c.CurrentTime }
```

## CI Integration

Tests run automatically in CI:

```yaml
# .github/workflows/pr.yaml
- name: Run tests
  run: go test -v -race -tags=envtest -coverprofile=coverage.out -covermode=atomic ./...

- name: Upload coverage
  uses: codecov/codecov-action@v6
  with:
    files: coverage.out
```

## Gateway API Conformance Tests

Conformance tests validate that the controller implements the Gateway API specification correctly. These tests require a real Kubernetes cluster with a working Cloudflare Tunnel.

### Prerequisites

- Kubernetes cluster (kind, k3s, or real cluster)
- Controller installed and running
- Cloudflare Tunnel configured and working
- GatewayClass `cloudflare-tunnel` created

### Running E2E Tests

E2E tests run against a live kind cluster with Cloudflare Tunnel and L7 proxy deployed. `E2E_TUNNEL_HOSTNAME` is required — the suite fails fast without it. `hack/conformance-setup.sh` threads it automatically from `.env` or the exported environment (`CF_TUNNEL_HOSTNAME`); set it explicitly when running `go test` by hand.

```bash
# Run all E2E tests
E2E_TUNNEL_HOSTNAME=<your-tunnel-hostname> \
  go test -v -race -tags e2e -count=1 -timeout=15m ./test/e2e/...

# Run a single test
E2E_TUNNEL_HOSTNAME=<your-tunnel-hostname> \
  go test -v -race -tags e2e -count=1 -timeout=15m ./test/e2e/... \
  -run TestHTTPRouteConformance/Core/HTTPRouteSimpleSameNamespace
```

### E2E Environment Variables

| Variable | Fallback | Default | Description |
| --- | --- | --- | --- |
| `E2E_TUNNEL_HOSTNAME` | `CONFORMANCE_TUNNEL_HOSTNAME` | none (required) | Edge hostname routing to the test tunnel; see `.env.example` |
| `E2E_KUBE_CONTEXT` | `CONFORMANCE_KUBE_CONTEXT` | `kind-v2-test` | kubectl context |
| `E2E_NAMESPACE` | `CONFORMANCE_NAMESPACE` | `cloudflare-tunnel-system` | Controller namespace |
| `E2E_TEST_NAMESPACE` | `CONFORMANCE_TEST_NAMESPACE` | `e2e-test` | Test resources namespace |
| `E2E_GATEWAY_NAME` | `CONFORMANCE_GATEWAY_NAME` | `e2e-gateway` | Gateway resource name |
| `E2E_SKIP_CLEANUP_ON_FAILURE` | (none) | unset | When non-empty, retains test resources (HTTPRoutes, Services) after a failed test for post-mortem `kubectl` inspection. CI leaves it unset so resources never accumulate. **Caveat: pair this with `-run TestName/SubtestName` to isolate the failing case.** The cleanup helpers wipe the entire test namespace, so in a full-suite run a passing sibling that comes after the failing subtest will delete its retained state -- only the last failing subtest after the final passing sibling actually survives. Retention is also scoped to a single `go test` run; the initial `wipeAllRoutesInNamespace` at the start of `TestHTTPRouteConformance` wipes leftover state from a previous invocation, so `kubectl`-inspect happens between runs, not across them. |

### E2E Test Coverage (25 tests)

Tests cover both Cloudflare Tunnel and L7 proxy features:

- **Core (4):** SimpleSameNamespace, PathPrefixMatching, ExactPathMatching, MatchingAcrossRoutes
- **Extended (19):** HeaderMatching, MethodMatching, QueryParamMatching, Weight, RequestHeaderModifier, ResponseHeaderModifier, RequestRedirect, RegexPathMatching, RegexHeaderMatching, RegexQueryParamMatching, PathMatchOrder, URLRewritePath, URLRewriteHost, RequestMirror, RedirectPort, RedirectPath, RedirectSchemeProbe, CombinedMatching, MultipleMatchesOR
- **Gateway (2):** AcceptedCondition, ObservedGenerationBump

### Official Gateway API Conformance Suite

The project integrates the official `sigs.k8s.io/gateway-api/conformance` suite with a custom `TunnelRoundTripper` that routes requests through Cloudflare edge.

`CONFORMANCE_TUNNEL_HOSTNAME` is required — the suite fails fast without it. `hack/conformance-setup.sh` threads it automatically from `.env` or the exported environment (`CF_TUNNEL_HOSTNAME`); set it explicitly when running `go test` by hand.

```bash
# Run conformance tests (requires deployed controller + tunnel)
CONFORMANCE_TUNNEL_HOSTNAME=<your-tunnel-hostname> \
  go test -v -tags conformance -count=1 -timeout=30m ./test/conformance/...

# Generate conformance report
CONFORMANCE_TUNNEL_HOSTNAME=<your-tunnel-hostname> \
CONFORMANCE_REPORT_OUTPUT=./conformance-report.yaml \
  go test -v -tags conformance -count=1 -timeout=30m ./test/conformance/...
```

| Variable | Default | Description |
| --- | --- | --- |
| `CONFORMANCE_TUNNEL_HOSTNAME` | none (required) | Edge hostname routing to the test tunnel; see `.env.example` |
| `CONFORMANCE_GATEWAY_CLASS` | `cloudflare-tunnel` | GatewayClass name |
| `CONFORMANCE_REPORT_OUTPUT` | (none) | Path for YAML conformance report |
| `CONTROLLER_VERSION` | `dev` | Version for report metadata |

Profiles: `GATEWAY-HTTP`, `GATEWAY-GRPC`.

### Known conformance flakes

A few conformance subtests are statistical by nature and occasionally need the suite's built-in retry budget to pass over the real Cloudflare Tunnel. These are sampling-variance non-regressions, not implementation failures — a retry of the named subtest passes. They are listed here so a failed *individual* attempt in the logs is not mistaken for a regression.

- **`HTTPRouteRequestPercentageMirror`** — the subtest distributes traffic and asserts the observed mirror rate lands in an 85–115% band over a 500-request sample. Over the tunnel the sampled rate occasionally lands just outside (78% and 116% observed on individual attempts) before passing within the suite's retry budget. The tolerance, sample size, and retry count are hardcoded in the upstream suite (`vendor/sigs.k8s.io/gateway-api/conformance/tests/httproute-request-percentage-mirror.go`), so they cannot be widened locally without forking official conformance — the flake is documented rather than tuned. Reported upstream as [kubernetes-sigs/gateway-api#4933](https://github.com/kubernetes-sigs/gateway-api/issues/4933): the relative ±15% band is only ~1.7σ on the 20% subcase, so even a perfectly conforming implementation flakes ~8–9% per distribution-check by binomial sampling alone.
- **`HTTPRouteRequestMirror`** — the `RequestMirror` filter dispatches a fire-and-forget mirror asynchronously, so the mirrored request occasionally arrives shortly after the test's poll window closes. The subtest passes on retry.

The same sampling-variance reasoning is applied inline for weighted backend selection in the custom e2e suite (`test/e2e/e2e_test.go`), where a deliberately wide proportion bound avoids reintroducing the flake from the opposite tail.

## Live-Tunnel Coverage Matrix

What actually gets exercised against a real Cloudflare Tunnel, and by which suite. "Conformance" is the official Gateway API suite (`test/conformance`, both profiles); "e2e" is the custom suite (`test/e2e`). Features marked unit-only are the deliberate residue — each carries a reason.

| Feature | Live coverage | Notes |
| --- | --- | --- |
| HTTPRoute core + extended matching (path/header/query/method, regex, ordering) | conformance + e2e | e2e re-checks through the real edge hostname (no `X-Original-Host` rewrite) |
| Filters: header modifiers, redirects (301/302/303/307/308, port/scheme/path), rewrites, mirrors (multiple, percentage) | conformance + e2e | percentage-mirror flake is suite-side, documented upstream |
| CORS filter | conformance | `SupportHTTPRouteCORS` |
| Timeouts (request / backendRequest) | conformance | explicit-`0s` disable semantics pinned by unit tests |
| Weighted traffic splitting | conformance + e2e | |
| Service types: ClusterIP, headless, ExternalName | conformance (`HTTPRouteServiceTypes`) | headless targetPort resolution covered |
| BackendTLSPolicy (CA ConfigMap, SNI hostname, DNS + URI SANs) — HTTP path | conformance | `SupportBackendTLSPolicy` + `SANValidation` |
| BackendTLSPolicy — gRPC path | e2e (`TestGRPCRouteOverTLSBackend`) | official gRPC client cannot dial through the tunnel |
| Gateway client certificate (backend mTLS) | conformance | multi-parent edge case unit-only (documented in limitations) |
| GRPCRoute matching + header modifiers through the tunnel transport | e2e (`TestGRPCRouteEndToEnd`) | official suite's gRPC dialer cannot reach `*.cfargotunnel.com`; suite-side tests run where possible |
| WebSocket upgrade through the tunnel (+ response filters) | conformance + e2e | `ws` cleartext; `wss` (TLS WebSocket backend) is unit-only, see below |
| `appProtocol` semantics: `kubernetes.io/h2c` | conformance | |
| `appProtocol` TLS hint without BackendTLSPolicy (fail-closed 502) | e2e (`TestBackendAppProtocolTLSWithoutPolicyFailsClosed`) | spec SHOULD: never silently dial cleartext |
| ExternalBackend CRD (direct-dial URL, base path) | e2e (`TestExternalBackendEndToEnd`) | proxy dials the URL directly, no Service resolution |
| ListenerSet (attach, conditions, AttachedRoutes, routing) | conformance + e2e | |
| ReferenceGrant (cross-namespace backends) | conformance | |
| Gateway / route status conditions, observedGeneration | conformance + e2e | |

Unit-only by design (each needs infrastructure a kind cluster does not have, or is not data-plane behaviour):

- **ServiceImport (MCS)** — kind has no `multicluster.x-k8s.io` API or `clusterset.local` DNS; resolution logic is unit-tested (`serviceimport_builder_test.go`). Revisit if a multi-cluster test rig ever exists.
- **`wss` backends (TLS WebSocket)** — needs a TLS-terminating WebSocket echo; the transport decision (policy → TLS, ALPN) is shared with the tested HTTPS path, so the marginal live value is low.
- **GatewayClass finalizer, status writers, sync skip internals** — control-plane behaviour with no data-plane signal; envtest/unit cover them, and a wrongly-skipped push would fail the routing e2e immediately.

When adding a user-visible feature, add its row here and decide the live-coverage story explicitly — an empty cell is a decision, not an oversight.

## Best Practices

1. **Fast tests**: Unit tests should run in milliseconds
2. **Isolated tests**: No shared state between tests
3. **Deterministic tests**: Same input = same output
4. **Readable tests**: Test name describes behavior
5. **Minimal mocking**: Only mock what's necessary
6. **Error testing**: Test error paths, not just happy paths
