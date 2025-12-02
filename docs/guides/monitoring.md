# Monitoring

This guide covers setting up monitoring for the Cloudflare Tunnel Gateway
Controller using Prometheus and Grafana.

## Overview

The controller exposes Prometheus metrics for monitoring reconciliation
performance, errors, and resource usage.

## Endpoints

| Endpoint | Port | Description |
|----------|------|-------------|
| `/metrics` | 8080 | Prometheus metrics |
| `/healthz` | 8081 | Liveness probe |
| `/readyz` | 8081 | Readiness probe |

## Quick Setup with Helm

Enable ServiceMonitor in Helm values:

```yaml
serviceMonitor:
  enabled: true
  interval: 30s
  labels:
    release: prometheus  # Match your Prometheus selector
```

## Manual ServiceMonitor

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

## Available Metrics

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

### Go Runtime Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `go_goroutines` | Gauge | Number of goroutines |
| `go_gc_duration_seconds` | Summary | GC pause duration |
| `go_memstats_alloc_bytes` | Gauge | Allocated memory |
| `process_cpu_seconds_total` | Counter | CPU time used |
| `process_resident_memory_bytes` | Gauge | Resident memory |

## Useful PromQL Queries

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

        # Slow reconciliation
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

        # Queue backlog
        - alert: CloudflareTunnelControllerQueueBacklog
          expr: workqueue_depth{name=~".*gateway.*|.*httproute.*"} > 100
          for: 5m
          labels:
            severity: warning
          annotations:
            summary: "Workqueue backlog"
            description: "Queue {{ $labels.name }} has {{ $value }} items pending"

        # Controller down
        - alert: CloudflareTunnelControllerDown
          expr: up{job="cloudflare-tunnel-gateway-controller"} == 0
          for: 2m
          labels:
            severity: critical
          annotations:
            summary: "Controller is down"
            description: "Cloudflare Tunnel Gateway Controller is not responding"

        # High memory
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

Import this dashboard or use as reference:

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

### Readiness Probe

```bash
curl http://localhost:8081/readyz
# Returns: ok
```

## Troubleshooting

### No Metrics Available

1. Check controller is running:

```bash
kubectl get pods --namespace cloudflare-tunnel-system
```

2. Check metrics endpoint:

```bash
kubectl port-forward --namespace cloudflare-tunnel-system \
  svc/cloudflare-tunnel-gateway-controller 8080:8080
curl http://localhost:8080/metrics
```

### ServiceMonitor Not Working

1. Check Prometheus is discovering the ServiceMonitor:

```bash
kubectl get servicemonitor --all-namespaces
```

2. Check Prometheus targets in UI at `/targets`

3. Verify labels match Prometheus selector
