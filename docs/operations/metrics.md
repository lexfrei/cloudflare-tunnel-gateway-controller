# Metrics & Alerting

The controller exposes Prometheus metrics for monitoring and alerting.

## Endpoints

| Endpoint | Port | Description |
|----------|------|-------------|
| `/metrics` | 8080 | Prometheus metrics |
| `/healthz` | 8081 | Liveness probe |
| `/readyz` | 8081 | Readiness probe |

## Available Metrics

### Route Synchronization Metrics

These metrics track the core synchronization of routes to Cloudflare Tunnel.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cftunnel_sync_duration_seconds` | Histogram | `status` | Duration of route sync operations |
| `cftunnel_synced_routes` | Gauge | `type` | Number of routes synced (http/grpc) |
| `cftunnel_ingress_rules` | Gauge | - | Total ingress rules in tunnel config |
| `cftunnel_failed_backend_refs` | Gauge | `type` | Failed backend references by route type |
| `cftunnel_sync_errors_total` | Counter | `error_type` | Sync errors by type |

### Cloudflare API Metrics

Track Cloudflare API interactions for performance and reliability monitoring.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cftunnel_cloudflare_api_duration_seconds` | Histogram | `method`, `resource` | API call latency |
| `cftunnel_cloudflare_api_calls_total` | Counter | `method`, `resource`, `status` | API calls count |
| `cftunnel_cloudflare_api_errors_total` | Counter | `method`, `error_type` | API errors by type |

**Label values:**

- `method`: `get`, `list`, `update`
- `resource`: `tunnel_config`, `account`
- `status`: `success`, `error`
- `error_type`: `auth`, `rate_limit`, `timeout`, `server_error`, `network`

### Helm Operations Metrics

When cloudflared is managed by the controller via Helm.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cftunnel_helm_operation_duration_seconds` | Histogram | `operation` | Helm operation latency |
| `cftunnel_helm_operations_total` | Counter | `operation`, `status` | Helm operations count |
| `cftunnel_helm_errors_total` | Counter | `operation`, `error_type` | Helm errors |
| `cftunnel_helm_chart_info` | Gauge | `chart`, `version`, `app_version` | Deployed chart version info |

**Label values:**

- `operation`: `install`, `upgrade`, `uninstall`, `get_version`, `load_chart`
- `status`: `success`, `error`

### Ingress Builder Metrics

Track the conversion of Gateway API routes to Cloudflare ingress rules.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cftunnel_ingress_build_duration_seconds` | Histogram | `type` | Rule building duration |
| `cftunnel_backend_ref_validation_total` | Counter | `type`, `result`, `reason` | Backend ref validation results |

### Controller Runtime Metrics

Built-in metrics from controller-runtime.

| Metric | Type | Description |
|--------|------|-------------|
| `controller_runtime_reconcile_total` | Counter | Total reconciliations per controller |
| `controller_runtime_reconcile_errors_total` | Counter | Total reconciliation errors |
| `controller_runtime_reconcile_time_seconds` | Histogram | Reconciliation duration |
| `controller_runtime_max_concurrent_reconciles` | Gauge | Max concurrent reconciles |
| `controller_runtime_active_workers` | Gauge | Current active workers |

### Workqueue Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `workqueue_adds_total` | Counter | Items added to queue |
| `workqueue_depth` | Gauge | Current queue depth |
| `workqueue_queue_duration_seconds` | Histogram | Time in queue |
| `workqueue_work_duration_seconds` | Histogram | Processing time |
| `workqueue_retries_total` | Counter | Item retries |
| `workqueue_longest_running_processor_seconds` | Gauge | Longest running item |

### Go Runtime Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `go_goroutines` | Gauge | Number of goroutines |
| `go_gc_duration_seconds` | Summary | GC pause duration |
| `go_memstats_alloc_bytes` | Gauge | Allocated memory |
| `go_memstats_heap_inuse_bytes` | Gauge | Heap in use |

### Process Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `process_cpu_seconds_total` | Counter | CPU time used |
| `process_resident_memory_bytes` | Gauge | Resident memory |
| `process_open_fds` | Gauge | Open file descriptors |

## Prometheus Configuration

### ServiceMonitor (Prometheus Operator)

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: cloudflare-tunnel-gateway-controller
  namespace: cloudflare-tunnel-system
  labels:
    app.kubernetes.io/name: cloudflare-tunnel-gateway-controller
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: cloudflare-tunnel-gateway-controller
  endpoints:
    - port: metrics
      interval: 30s
      path: /metrics
```

### Scrape Config (Prometheus)

```yaml
scrape_configs:
  - job_name: cloudflare-tunnel-gateway-controller
    kubernetes_sd_configs:
      - role: endpoints
        namespaces:
          names:
            - cloudflare-tunnel-system
    relabel_configs:
      - source_labels: [__meta_kubernetes_service_name]
        action: keep
        regex: cloudflare-tunnel-gateway-controller
      - source_labels: [__meta_kubernetes_endpoint_port_name]
        action: keep
        regex: metrics
```

## PromQL Queries

### Route Sync Performance

```promql
# Sync duration P95
histogram_quantile(0.95,
  sum(rate(cftunnel_sync_duration_seconds_bucket[5m])) by (le)
)

# Sync error rate
sum(rate(cftunnel_sync_errors_total[5m])) by (error_type)

# Routes synced by type
sum(cftunnel_synced_routes) by (type)

# Total ingress rules
cftunnel_ingress_rules

# Failed backend references
sum(cftunnel_failed_backend_refs) by (type)
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

# API errors by type
sum(rate(cftunnel_cloudflare_api_errors_total[5m])) by (method, error_type)
```

### Helm Operations

```promql
# Helm operation success rate
sum(rate(cftunnel_helm_operations_total{status="success"}[5m])) by (operation)
/
sum(rate(cftunnel_helm_operations_total[5m])) by (operation)

# Helm operation latency P95
histogram_quantile(0.95,
  sum(rate(cftunnel_helm_operation_duration_seconds_bucket[5m])) by (le, operation)
)
```

### Reconciliation Rate

```promql
# Reconciliations per second by controller
sum(rate(controller_runtime_reconcile_total[5m])) by (controller)

# Error rate
sum(rate(controller_runtime_reconcile_errors_total[5m])) by (controller)

# Error percentage
sum(rate(controller_runtime_reconcile_errors_total[5m])) by (controller)
/
sum(rate(controller_runtime_reconcile_total[5m])) by (controller)
* 100
```

### Reconciliation Latency

```promql
# P50 latency
histogram_quantile(0.50,
  sum(rate(controller_runtime_reconcile_time_seconds_bucket[5m])) by (le, controller)
)

# P95 latency
histogram_quantile(0.95,
  sum(rate(controller_runtime_reconcile_time_seconds_bucket[5m])) by (le, controller)
)

# P99 latency
histogram_quantile(0.99,
  sum(rate(controller_runtime_reconcile_time_seconds_bucket[5m])) by (le, controller)
)
```

### Queue Health

```promql
# Queue depth (should be low)
workqueue_depth{name=~".*gateway.*|.*httproute.*"}

# Average time in queue
sum(workqueue_queue_duration_seconds_sum) by (name)
/
sum(workqueue_queue_duration_seconds_count) by (name)

# Processing time
sum(workqueue_work_duration_seconds_sum) by (name)
/
sum(workqueue_work_duration_seconds_count) by (name)
```

### Resource Usage

```promql
# Memory usage
process_resident_memory_bytes{job="cloudflare-tunnel-gateway-controller"}

# CPU usage
rate(process_cpu_seconds_total{job="cloudflare-tunnel-gateway-controller"}[5m])

# Goroutines
go_goroutines{job="cloudflare-tunnel-gateway-controller"}
```

## Alerting Rules

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: cloudflare-tunnel-gateway-controller
  namespace: cloudflare-tunnel-system
spec:
  groups:
    - name: cloudflare-tunnel-sync
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

        - alert: CloudflareTunnelSyncSlow
          expr: |
            histogram_quantile(0.95,
              sum(rate(cftunnel_sync_duration_seconds_bucket[5m])) by (le)
            ) > 10
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "Slow route synchronization"
            description: "P95 sync duration is {{ $value | humanizeDuration }}"

        - alert: CloudflareTunnelFailedBackendRefs
          expr: |
            sum(cftunnel_failed_backend_refs) > 0
          for: 15m
          labels:
            severity: warning
          annotations:
            summary: "Failed backend references"
            description: "{{ $value }} backend references are failing validation"

    - name: cloudflare-tunnel-api
      rules:
        - alert: CloudflareTunnelAPIErrors
          expr: |
            sum(rate(cftunnel_cloudflare_api_errors_total[5m])) by (error_type) > 0
          for: 5m
          labels:
            severity: critical
          annotations:
            summary: "Cloudflare API errors"
            description: "{{ $labels.error_type }} errors: {{ $value | humanize }}/sec"

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

        - alert: CloudflareTunnelAPIRateLimited
          expr: |
            sum(rate(cftunnel_cloudflare_api_errors_total{error_type="rate_limit"}[5m])) > 0
          for: 2m
          labels:
            severity: critical
          annotations:
            summary: "Cloudflare API rate limited"
            description: "Controller is being rate limited by Cloudflare API"

    - name: cloudflare-tunnel-controller
      rules:
        - alert: CloudflareTunnelControllerHighErrorRate
          expr: |
            sum(rate(controller_runtime_reconcile_errors_total[5m])) by (controller)
            /
            sum(rate(controller_runtime_reconcile_total[5m])) by (controller)
            > 0.1
          for: 5m
          labels:
            severity: warning
          annotations:
            summary: "High reconciliation error rate"
            description: "Controller {{ $labels.controller }} has error rate {{ $value | humanizePercentage }}"

        - alert: CloudflareTunnelControllerSlowReconciliation
          expr: |
            histogram_quantile(0.99,
              sum(rate(controller_runtime_reconcile_time_seconds_bucket[5m])) by (le, controller)
            ) > 30
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "Slow reconciliation"
            description: "Controller {{ $labels.controller }} P99 latency is {{ $value | humanizeDuration }}"

        - alert: CloudflareTunnelControllerQueueBacklog
          expr: workqueue_depth{name=~".*gateway.*|.*httproute.*"} > 100
          for: 5m
          labels:
            severity: warning
          annotations:
            summary: "Workqueue backlog"
            description: "Queue {{ $labels.name }} has {{ $value }} items pending"

        - alert: CloudflareTunnelControllerDown
          expr: up{job="cloudflare-tunnel-gateway-controller"} == 0
          for: 2m
          labels:
            severity: critical
          annotations:
            summary: "Controller is down"
            description: "Cloudflare Tunnel Gateway Controller is not responding"

        - alert: CloudflareTunnelControllerHighMemory
          expr: |
            process_resident_memory_bytes{job="cloudflare-tunnel-gateway-controller"}
            > 512 * 1024 * 1024
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "High memory usage"
            description: "Controller using {{ $value | humanize1024 }}B memory"
```

## Grafana Dashboard

Example dashboard panels:

```json
{
  "title": "Cloudflare Tunnel Gateway Controller",
  "panels": [
    {
      "title": "Routes Synced",
      "type": "stat",
      "gridPos": { "x": 0, "y": 0, "w": 6, "h": 4 },
      "targets": [
        {
          "expr": "sum(cftunnel_synced_routes) by (type)",
          "legendFormat": "{{ type }}"
        }
      ]
    },
    {
      "title": "Ingress Rules",
      "type": "stat",
      "gridPos": { "x": 6, "y": 0, "w": 6, "h": 4 },
      "targets": [
        {
          "expr": "cftunnel_ingress_rules"
        }
      ]
    },
    {
      "title": "Failed Backend Refs",
      "type": "stat",
      "gridPos": { "x": 12, "y": 0, "w": 6, "h": 4 },
      "targets": [
        {
          "expr": "sum(cftunnel_failed_backend_refs)",
          "legendFormat": "failed"
        }
      ],
      "fieldConfig": {
        "defaults": {
          "thresholds": {
            "steps": [
              { "value": 0, "color": "green" },
              { "value": 1, "color": "red" }
            ]
          }
        }
      }
    },
    {
      "title": "Sync Duration (P95)",
      "type": "timeseries",
      "gridPos": { "x": 0, "y": 4, "w": 12, "h": 8 },
      "targets": [
        {
          "expr": "histogram_quantile(0.95, sum(rate(cftunnel_sync_duration_seconds_bucket[5m])) by (le))",
          "legendFormat": "p95"
        },
        {
          "expr": "histogram_quantile(0.50, sum(rate(cftunnel_sync_duration_seconds_bucket[5m])) by (le))",
          "legendFormat": "p50"
        }
      ],
      "fieldConfig": {
        "defaults": { "unit": "s" }
      }
    },
    {
      "title": "Cloudflare API Latency",
      "type": "timeseries",
      "gridPos": { "x": 12, "y": 4, "w": 12, "h": 8 },
      "targets": [
        {
          "expr": "histogram_quantile(0.99, sum(rate(cftunnel_cloudflare_api_duration_seconds_bucket[5m])) by (le, method))",
          "legendFormat": "p99 {{ method }}"
        }
      ],
      "fieldConfig": {
        "defaults": { "unit": "s" }
      }
    },
    {
      "title": "API Calls/sec",
      "type": "timeseries",
      "gridPos": { "x": 0, "y": 12, "w": 12, "h": 8 },
      "targets": [
        {
          "expr": "sum(rate(cftunnel_cloudflare_api_calls_total[5m])) by (method, status)",
          "legendFormat": "{{ method }} ({{ status }})"
        }
      ]
    },
    {
      "title": "Sync Errors/sec",
      "type": "timeseries",
      "gridPos": { "x": 12, "y": 12, "w": 12, "h": 8 },
      "targets": [
        {
          "expr": "sum(rate(cftunnel_sync_errors_total[5m])) by (error_type)",
          "legendFormat": "{{ error_type }}"
        }
      ]
    },
    {
      "title": "Reconciliations/sec",
      "type": "timeseries",
      "gridPos": { "x": 0, "y": 20, "w": 12, "h": 8 },
      "targets": [
        {
          "expr": "sum(rate(controller_runtime_reconcile_total[5m])) by (controller)",
          "legendFormat": "{{ controller }}"
        }
      ]
    },
    {
      "title": "Reconciliation Latency",
      "type": "timeseries",
      "gridPos": { "x": 12, "y": 20, "w": 12, "h": 8 },
      "targets": [
        {
          "expr": "histogram_quantile(0.95, sum(rate(controller_runtime_reconcile_time_seconds_bucket[5m])) by (le, controller))",
          "legendFormat": "p95 {{ controller }}"
        }
      ],
      "fieldConfig": {
        "defaults": { "unit": "s" }
      }
    },
    {
      "title": "Queue Depth",
      "type": "timeseries",
      "gridPos": { "x": 0, "y": 28, "w": 12, "h": 8 },
      "targets": [
        {
          "expr": "workqueue_depth",
          "legendFormat": "{{ name }}"
        }
      ]
    },
    {
      "title": "Memory Usage",
      "type": "stat",
      "gridPos": { "x": 12, "y": 28, "w": 6, "h": 4 },
      "targets": [
        {
          "expr": "process_resident_memory_bytes{job=\"cloudflare-tunnel-gateway-controller\"}"
        }
      ],
      "fieldConfig": {
        "defaults": { "unit": "bytes" }
      }
    },
    {
      "title": "Goroutines",
      "type": "stat",
      "gridPos": { "x": 18, "y": 28, "w": 6, "h": 4 },
      "targets": [
        {
          "expr": "go_goroutines{job=\"cloudflare-tunnel-gateway-controller\"}"
        }
      ]
    }
  ]
}
```

## Health Checks

### Liveness Probe

```bash
curl http://localhost:8081/healthz
# Returns: ok
```

### Readiness Probe

```bash
curl http://localhost:8081/readyz
# Returns: ok
```

### Kubernetes Configuration

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 8081
  initialDelaySeconds: 15
  periodSeconds: 20

readinessProbe:
  httpGet:
    path: /readyz
    port: 8081
  initialDelaySeconds: 5
  periodSeconds: 10
```
