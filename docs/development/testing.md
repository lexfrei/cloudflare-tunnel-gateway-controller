# Testing

This guide covers testing standards and practices for the Cloudflare Tunnel
Gateway Controller.

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

```bash
# Install helm-unittest plugin
helm plugin install https://github.com/helm-unittest/helm-unittest

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
  run: go test -v -race -coverprofile=coverage.out ./...

- name: Upload coverage
  uses: codecov/codecov-action@v4
  with:
    files: coverage.out
```

## Best Practices

1. **Fast tests**: Unit tests should run in milliseconds
2. **Isolated tests**: No shared state between tests
3. **Deterministic tests**: Same input = same output
4. **Readable tests**: Test name describes behavior
5. **Minimal mocking**: Only mock what's necessary
6. **Error testing**: Test error paths, not just happy paths
