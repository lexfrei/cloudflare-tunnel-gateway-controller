# Metrics and Monitoring

The controller exposes Prometheus metrics for monitoring and alerting.

## Endpoints

| Endpoint | Port | Description |
|----------|------|-------------|
| `/metrics` | 8080 | Prometheus metrics |
| `/healthz` | 8081 | Liveness probe |
| `/readyz` | 8081 | Readiness probe |

## Available Metrics

The controller exposes standard controller-runtime metrics plus Go runtime metrics.

### Controller Metrics

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
  - job_name: 'cloudflare-tunnel-gateway-controller'
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

## Useful Queries

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

# Items waiting in queue
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

### PrometheusRule (Prometheus Operator)

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: cloudflare-tunnel-gateway-controller
  namespace: cloudflare-tunnel-system
spec:
  groups:
    - name: cloudflare-tunnel-gateway-controller
      rules:
        # High error rate
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

        # Reconciliation taking too long
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

        # Queue backing up
        - alert: CloudflareTunnelControllerQueueBacklog
          expr: workqueue_depth{name=~".*gateway.*|.*httproute.*"} > 100
          for: 5m
          labels:
            severity: warning
          annotations:
            summary: "Workqueue backlog"
            description: "Queue {{ $labels.name }} has {{ $value }} items pending"

        # Controller not running
        - alert: CloudflareTunnelControllerDown
          expr: up{job="cloudflare-tunnel-gateway-controller"} == 0
          for: 2m
          labels:
            severity: critical
          annotations:
            summary: "Controller is down"
            description: "Cloudflare Tunnel Gateway Controller is not responding"

        # High memory usage
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

Import this dashboard JSON or use as reference:

```json
{
  "title": "Cloudflare Tunnel Gateway Controller",
  "panels": [
    {
      "title": "Reconciliations/sec",
      "type": "timeseries",
      "targets": [
        {
          "expr": "sum(rate(controller_runtime_reconcile_total[5m])) by (controller)",
          "legendFormat": "{{ controller }}"
        }
      ]
    },
    {
      "title": "Error Rate %",
      "type": "timeseries",
      "targets": [
        {
          "expr": "sum(rate(controller_runtime_reconcile_errors_total[5m])) by (controller) / sum(rate(controller_runtime_reconcile_total[5m])) by (controller) * 100",
          "legendFormat": "{{ controller }}"
        }
      ]
    },
    {
      "title": "Reconciliation Latency",
      "type": "timeseries",
      "targets": [
        {
          "expr": "histogram_quantile(0.50, sum(rate(controller_runtime_reconcile_time_seconds_bucket[5m])) by (le, controller))",
          "legendFormat": "p50 {{ controller }}"
        },
        {
          "expr": "histogram_quantile(0.95, sum(rate(controller_runtime_reconcile_time_seconds_bucket[5m])) by (le, controller))",
          "legendFormat": "p95 {{ controller }}"
        }
      ]
    },
    {
      "title": "Queue Depth",
      "type": "timeseries",
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
      "targets": [
        {
          "expr": "process_resident_memory_bytes{job=\"cloudflare-tunnel-gateway-controller\"}"
        }
      ],
      "fieldConfig": {
        "defaults": {
          "unit": "bytes"
        }
      }
    },
    {
      "title": "Goroutines",
      "type": "stat",
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

The liveness probe indicates the controller process is running.

### Readiness Probe

```bash
curl http://localhost:8081/readyz
# Returns: ok
```

The readiness probe indicates the controller is ready to process requests.

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
