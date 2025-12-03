# Metrics Instrumentation Design

This document describes the design for comprehensive Prometheus metrics
instrumentation in the Cloudflare Tunnel Gateway Controller.

## Overview

The controller needs application-specific metrics beyond the built-in
controller-runtime metrics. This design covers metrics for:

- Route synchronization operations
- Cloudflare API interactions
- Helm operations (when managing cloudflared)
- Ingress rule generation

## Goals

1. **Observability**: Enable operators to understand controller behavior
2. **Debugging**: Provide metrics for troubleshooting issues
3. **Alerting**: Support SLO/SLA monitoring via Prometheus alerts
4. **Performance**: Track latency of critical operations

## Non-Goals

1. Business metrics (e.g., traffic volume through tunnels)
2. Cloudflared internal metrics (handled by cloudflared itself)
3. Per-request metrics (would cause cardinality explosion)

## Architecture

### Package Structure

```text
internal/metrics/
├── metrics.go        # Metrics registration and collector interface
├── sync.go           # Route sync metrics (RouteSyncer)
├── cloudflare.go     # Cloudflare API metrics
├── helm.go           # Helm operations metrics
└── ingress.go        # Ingress builder metrics
```

### Integration Pattern

Metrics will be injected into components via a collector interface:

```go
// Collector provides metrics recording interface.
// This allows components to record metrics without direct prometheus dependency.
type Collector interface {
    // Sync metrics
    RecordSyncDuration(ctx context.Context, status string, duration time.Duration)
    RecordSyncedRoutes(ctx context.Context, routeType string, count int)
    RecordIngressRules(ctx context.Context, count int)
    RecordFailedBackendRefs(ctx context.Context, routeType string, count int)
    RecordSyncError(ctx context.Context, errorType string)

    // Cloudflare API metrics
    RecordAPICall(ctx context.Context, method, resource, status string, duration time.Duration)
    RecordAPIError(ctx context.Context, method, errorType string)

    // Helm metrics
    RecordHelmOperation(ctx context.Context, operation, status string, duration time.Duration)
    RecordHelmError(ctx context.Context, operation, errorType string)
}
```

### Registration

Metrics are registered in `internal/metrics/metrics.go`:

```go
var (
    // MustRegister registers all metrics with the controller-runtime registry.
    // Called from cmd/controller during initialization.
    reg = crmetrics.Registry
)

func init() {
    reg.MustRegister(
        syncDuration,
        syncedRoutes,
        ingressRulesTotal,
        // ... other metrics
    )
}
```

## Proposed Metrics

### Route Synchronization Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cftunnel_sync_duration_seconds` | Histogram | `status` | Duration of SyncAllRoutes operation |
| `cftunnel_synced_routes` | Gauge | `type` | Number of routes synced (http/grpc) |
| `cftunnel_ingress_rules_total` | Gauge | - | Total ingress rules in tunnel config |
| `cftunnel_failed_backend_refs` | Gauge | `type` | Failed backend references |
| `cftunnel_sync_errors_total` | Counter | `error_type` | Sync errors by type |

**Bucket configuration for `cftunnel_sync_duration_seconds`:**

```go
prometheus.HistogramOpts{
    Name:    "cftunnel_sync_duration_seconds",
    Help:    "Duration of route synchronization to Cloudflare",
    Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
}
```

### Cloudflare API Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cftunnel_cloudflare_api_duration_seconds` | Histogram | `method`, `resource` | API call latency |
| `cftunnel_cloudflare_api_calls_total` | Counter | `method`, `resource`, `status` | API calls count |
| `cftunnel_cloudflare_api_errors_total` | Counter | `method`, `error_type` | API errors |

**Label values:**

- `method`: `get`, `update`
- `resource`: `tunnel_config`, `account`
- `status`: `success`, `error`
- `error_type`: `auth`, `rate_limit`, `timeout`, `server_error`, `network`

### Helm Operations Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cftunnel_helm_operation_duration_seconds` | Histogram | `operation` | Helm operation latency |
| `cftunnel_helm_operations_total` | Counter | `operation`, `status` | Helm operations count |
| `cftunnel_helm_errors_total` | Counter | `operation`, `error_type` | Helm errors |
| `cftunnel_helm_chart_info` | Gauge | `chart`, `version`, `app_version` | Deployed chart version (always 1) |

**Label values:**

- `operation`: `install`, `upgrade`, `uninstall`, `get_version`, `load_chart`
- `status`: `success`, `error`
- `chart`: chart name (e.g., `cloudflare-tunnel`)
- `version`: chart version (e.g., `0.1.0`)
- `app_version`: cloudflared version (e.g., `2024.1.0`)

### Ingress Builder Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cftunnel_ingress_build_duration_seconds` | Histogram | `type` | Rule building duration |
| `cftunnel_ingress_rules_generated` | Gauge | `type` | Rules generated per type |
| `cftunnel_backend_ref_validation_total` | Counter | `type`, `result`, `reason` | Backend ref validation results |

**Label values:**

- `type`: `http`, `grpc`
- `result`: `accepted`, `rejected`
- `reason`: `not_permitted`, `not_found`, `invalid_reference`

## Integration Points

### RouteSyncer

In `internal/controller/route_syncer.go`:

```go
type RouteSyncer struct {
    // ... existing fields
    Metrics metrics.Collector
}

func (s *RouteSyncer) SyncAllRoutes(ctx context.Context) (ctrl.Result, *SyncResult, error) {
    start := time.Now()
    defer func() {
        s.Metrics.RecordSyncDuration(ctx, status, time.Since(start))
    }()

    // After collecting routes
    s.Metrics.RecordSyncedRoutes(ctx, "http", len(httpRoutes))
    s.Metrics.RecordSyncedRoutes(ctx, "grpc", len(grpcRoutes))

    // After building rules
    s.Metrics.RecordIngressRules(ctx, len(finalRules))

    // ... rest of sync logic
}
```

### ConfigResolver API Calls

In `internal/config/resolver.go`, wrap Cloudflare API calls:

```go
func (r *Resolver) getConfigWithMetrics(
    ctx context.Context,
    client *cloudflare.Client,
    tunnelID, accountID string,
) (*zero_trust.TunnelConfiguration, error) {
    start := time.Now()

    cfg, err := client.ZeroTrust.Tunnels.Cloudflared.Configurations.Get(...)

    status := "success"
    if err != nil {
        status = "error"
        r.metrics.RecordAPIError(ctx, "get", classifyError(err))
    }
    r.metrics.RecordAPICall(ctx, "get", "tunnel_config", status, time.Since(start))

    return cfg, err
}
```

### Helm Manager

In `internal/helm/manager.go`:

```go
type Manager struct {
    // ... existing fields
    metrics metrics.Collector
}

func (m *Manager) Install(ctx context.Context, ...) (release.Releaser, error) {
    start := time.Now()
    defer func() {
        status := "success"
        if err != nil {
            status = "error"
        }
        m.metrics.RecordHelmOperation(ctx, "install", status, time.Since(start))
    }()

    // ... existing install logic
}
```

## Error Classification

API errors are classified for the `error_type` label using HTTP status codes
extracted from the Cloudflare API response:

```go
func classifyCloudflareError(err error) string {
    if err == nil {
        return ""
    }

    // Check for typed errors from cloudflare-go SDK
    var apiErr *cloudflare.APIError
    if errors.As(err, &apiErr) {
        switch {
        case apiErr.StatusCode == 401 || apiErr.StatusCode == 403:
            return "auth"
        case apiErr.StatusCode == 429:
            return "rate_limit"
        case apiErr.StatusCode >= 500 && apiErr.StatusCode < 600:
            return "server_error"
        case apiErr.StatusCode >= 400 && apiErr.StatusCode < 500:
            return "client_error"
        }
    }

    // Fallback for non-API errors
    errStr := err.Error()
    switch {
    case strings.Contains(errStr, "timeout") || strings.Contains(errStr, "deadline"):
        return "timeout"
    case strings.Contains(errStr, "connection refused") || strings.Contains(errStr, "no such host"):
        return "network"
    default:
        return "unknown"
    }
}
```

## Example PromQL Queries

### Sync Performance

```promql
# P95 sync duration
histogram_quantile(0.95,
  sum(rate(cftunnel_sync_duration_seconds_bucket[5m])) by (le)
)

# Sync error rate
sum(rate(cftunnel_sync_errors_total[5m])) by (error_type)
/
sum(rate(cftunnel_sync_duration_seconds_count[5m]))
```

### Cloudflare API Health

```promql
# API success rate
sum(rate(cftunnel_cloudflare_api_calls_total{status="success"}[5m]))
/
sum(rate(cftunnel_cloudflare_api_calls_total[5m]))

# API latency P99
histogram_quantile(0.99,
  sum(rate(cftunnel_cloudflare_api_duration_seconds_bucket[5m])) by (le, method)
)
```

### Route Status

```promql
# Total routes by type
sum(cftunnel_synced_routes) by (type)

# Failed backend refs
sum(cftunnel_failed_backend_refs) by (type)
```

## Alerting Rules

```yaml
groups:
  - name: cloudflare-tunnel-controller
    rules:
      - alert: CloudflareTunnelSyncErrors
        expr: |
          sum(rate(cftunnel_sync_errors_total[5m])) > 0.1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "High sync error rate"
          description: "Controller experiencing {{ $value | humanize }} sync errors/sec"

      - alert: CloudflareTunnelAPISlow
        expr: |
          histogram_quantile(0.99,
            sum(rate(cftunnel_cloudflare_api_duration_seconds_bucket[5m])) by (le)
          ) > 10
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Cloudflare API latency high"
          description: "P99 API latency is {{ $value | humanizeDuration }}"

      - alert: CloudflareTunnelAPIErrors
        expr: |
          sum(rate(cftunnel_cloudflare_api_errors_total[5m])) by (error_type) > 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "Cloudflare API errors"
          description: "{{ $labels.error_type }} errors: {{ $value | humanize }}/sec"
```

## Implementation Plan

### Phase 1: Core Infrastructure

1. Create `internal/metrics/` package
2. Define `Collector` interface
3. Implement prometheus metrics registration
4. Add metrics injection to `RouteSyncer`

### Phase 2: Sync Metrics

1. Instrument `SyncAllRoutes()` with duration and counters
2. Add route count metrics
3. Add ingress rules gauge
4. Add failed backend refs counter

### Phase 3: API Metrics

1. Wrap Cloudflare API calls with timing
2. Add error classification
3. Instrument `ConfigResolver` methods

### Phase 4: Helm Metrics

1. Add metrics to `Manager` struct
2. Instrument `Install`, `Upgrade`, `Uninstall`
3. Add chart version info metric

### Phase 5: Documentation & Dashboard

1. Update [metrics.md](../operations/metrics.md) with new metrics
2. Create example Grafana dashboard JSON
3. Add alerting rule examples

## Testing Strategy

### Unit Tests

- Mock `Collector` interface for testing
- Verify metrics are recorded with correct labels
- Test error classification logic

### Integration Tests

- Use `testutil` from prometheus to verify metric values
- Test metric registration doesn't conflict with controller-runtime

## Backwards Compatibility

New metrics are additive and don't affect existing functionality.
The `Collector` interface allows optional metrics (nil check).

## Security Considerations

- Metrics endpoint is internal (not exposed via Ingress)
- No sensitive data in metric labels
- Cardinality controlled via limited label values

## References

- [Prometheus Best Practices](https://prometheus.io/docs/practices/naming/)
- [controller-runtime Metrics](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/metrics)
- [Cloudflare API Docs](https://developers.cloudflare.com/api/)
