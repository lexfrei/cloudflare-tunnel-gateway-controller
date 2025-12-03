// Package metrics provides Prometheus metrics instrumentation for the controller.
package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Collector provides metrics recording interface.
// This allows components to record metrics without direct prometheus dependency.
//
//nolint:interfacebloat // All methods are needed for comprehensive metrics coverage
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
	RecordHelmChartInfo(ctx context.Context, chart, version, appVersion string)

	// Ingress builder metrics
	RecordIngressBuildDuration(ctx context.Context, routeType string, duration time.Duration)
	RecordBackendRefValidation(ctx context.Context, routeType, result, reason string)
}

// prometheusCollector implements Collector using Prometheus metrics.
type prometheusCollector struct {
	// Sync metrics
	syncDuration      *prometheus.HistogramVec
	syncedRoutes      *prometheus.GaugeVec
	ingressRulesTotal prometheus.Gauge
	failedBackendRefs *prometheus.GaugeVec
	syncErrorsTotal   *prometheus.CounterVec

	// Cloudflare API metrics
	apiDuration    *prometheus.HistogramVec
	apiCallsTotal  *prometheus.CounterVec
	apiErrorsTotal *prometheus.CounterVec

	// Helm metrics
	helmDuration    *prometheus.HistogramVec
	helmOpsTotal    *prometheus.CounterVec
	helmErrorsTotal *prometheus.CounterVec
	helmChartInfo   *prometheus.GaugeVec

	// Ingress builder metrics
	ingressBuildDuration *prometheus.HistogramVec
	backendRefValidation *prometheus.CounterVec
}

// NewCollector creates a new Prometheus metrics collector and registers metrics.
func NewCollector(reg prometheus.Registerer) Collector {
	c := &prometheusCollector{}
	c.initSyncMetrics()
	c.initAPIMetrics()
	c.initHelmMetrics()
	c.initIngressMetrics()
	c.register(reg)

	return c
}

// RecordSyncDuration records the duration of a sync operation.
func (c *prometheusCollector) RecordSyncDuration(_ context.Context, status string, duration time.Duration) {
	c.syncDuration.WithLabelValues(status).Observe(duration.Seconds())
}

// RecordSyncedRoutes records the number of synced routes by type.
func (c *prometheusCollector) RecordSyncedRoutes(_ context.Context, routeType string, count int) {
	c.syncedRoutes.WithLabelValues(routeType).Set(float64(count))
}

// RecordIngressRules records the total number of ingress rules.
func (c *prometheusCollector) RecordIngressRules(_ context.Context, count int) {
	c.ingressRulesTotal.Set(float64(count))
}

// RecordFailedBackendRefs records the number of failed backend references.
func (c *prometheusCollector) RecordFailedBackendRefs(_ context.Context, routeType string, count int) {
	c.failedBackendRefs.WithLabelValues(routeType).Set(float64(count))
}

// RecordSyncError records a sync error by type.
func (c *prometheusCollector) RecordSyncError(_ context.Context, errorType string) {
	c.syncErrorsTotal.WithLabelValues(errorType).Inc()
}

// RecordAPICall records a Cloudflare API call.
func (c *prometheusCollector) RecordAPICall(
	_ context.Context,
	method, resource, status string,
	duration time.Duration,
) {
	c.apiDuration.WithLabelValues(method, resource).Observe(duration.Seconds())
	c.apiCallsTotal.WithLabelValues(method, resource, status).Inc()
}

// RecordAPIError records a Cloudflare API error.
func (c *prometheusCollector) RecordAPIError(_ context.Context, method, errorType string) {
	c.apiErrorsTotal.WithLabelValues(method, errorType).Inc()
}

// RecordHelmOperation records a Helm operation.
func (c *prometheusCollector) RecordHelmOperation(
	_ context.Context,
	operation, status string,
	duration time.Duration,
) {
	c.helmDuration.WithLabelValues(operation).Observe(duration.Seconds())
	c.helmOpsTotal.WithLabelValues(operation, status).Inc()
}

// RecordHelmError records a Helm error.
func (c *prometheusCollector) RecordHelmError(_ context.Context, operation, errorType string) {
	c.helmErrorsTotal.WithLabelValues(operation, errorType).Inc()
}

// RecordHelmChartInfo records the deployed Helm chart version info.
func (c *prometheusCollector) RecordHelmChartInfo(_ context.Context, chart, version, appVersion string) {
	c.helmChartInfo.WithLabelValues(chart, version, appVersion).Set(1)
}

// RecordIngressBuildDuration records the duration of ingress rule building.
func (c *prometheusCollector) RecordIngressBuildDuration(
	_ context.Context,
	routeType string,
	duration time.Duration,
) {
	c.ingressBuildDuration.WithLabelValues(routeType).Observe(duration.Seconds())
}

// RecordBackendRefValidation records a backend reference validation result.
func (c *prometheusCollector) RecordBackendRefValidation(_ context.Context, routeType, result, reason string) {
	c.backendRefValidation.WithLabelValues(routeType, result, reason).Inc()
}

func (c *prometheusCollector) initSyncMetrics() {
	c.syncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cftunnel_sync_duration_seconds",
			Help:    "Duration of route synchronization to Cloudflare",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		},
		[]string{"status"},
	)
	c.syncedRoutes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cftunnel_synced_routes",
			Help: "Number of routes synced by type",
		},
		[]string{"type"},
	)
	c.ingressRulesTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "cftunnel_ingress_rules",
			Help: "Total ingress rules in tunnel config",
		},
	)
	c.failedBackendRefs = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cftunnel_failed_backend_refs",
			Help: "Number of failed backend references",
		},
		[]string{"type"},
	)
	c.syncErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cftunnel_sync_errors_total",
			Help: "Total sync errors by type",
		},
		[]string{"error_type"},
	)
}

func (c *prometheusCollector) initAPIMetrics() {
	c.apiDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cftunnel_cloudflare_api_duration_seconds",
			Help:    "Duration of Cloudflare API calls",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		},
		[]string{"method", "resource"},
	)
	c.apiCallsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cftunnel_cloudflare_api_calls_total",
			Help: "Total Cloudflare API calls",
		},
		[]string{"method", "resource", "status"},
	)
	c.apiErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cftunnel_cloudflare_api_errors_total",
			Help: "Total Cloudflare API errors by type",
		},
		[]string{"method", "error_type"},
	)
}

func (c *prometheusCollector) initHelmMetrics() {
	c.helmDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cftunnel_helm_operation_duration_seconds",
			Help:    "Duration of Helm operations",
			Buckets: []float64{0.5, 1, 2.5, 5, 10, 30, 60},
		},
		[]string{"operation"},
	)
	c.helmOpsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cftunnel_helm_operations_total",
			Help: "Total Helm operations",
		},
		[]string{"operation", "status"},
	)
	c.helmErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cftunnel_helm_errors_total",
			Help: "Total Helm errors by type",
		},
		[]string{"operation", "error_type"},
	)
	c.helmChartInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cftunnel_helm_chart_info",
			Help: "Deployed Helm chart version info (always 1)",
		},
		[]string{"chart", "version", "app_version"},
	)
}

func (c *prometheusCollector) initIngressMetrics() {
	c.ingressBuildDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "cftunnel_ingress_build_duration_seconds",
			Help:    "Duration of ingress rule building",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5},
		},
		[]string{"type"},
	)
	c.backendRefValidation = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cftunnel_backend_ref_validation_total",
			Help: "Backend reference validation results",
		},
		[]string{"type", "result", "reason"},
	)
}

func (c *prometheusCollector) register(reg prometheus.Registerer) {
	reg.MustRegister(
		c.syncDuration,
		c.syncedRoutes,
		c.ingressRulesTotal,
		c.failedBackendRefs,
		c.syncErrorsTotal,
		c.apiDuration,
		c.apiCallsTotal,
		c.apiErrorsTotal,
		c.helmDuration,
		c.helmOpsTotal,
		c.helmErrorsTotal,
		c.helmChartInfo,
		c.ingressBuildDuration,
		c.backendRefValidation,
	)
}

// NoopCollector is a no-op implementation of Collector for testing.
type NoopCollector struct{}

// NewNoopCollector creates a new no-op collector.
func NewNoopCollector() *NoopCollector {
	return &NoopCollector{}
}

// RecordSyncDuration is a no-op.
func (c *NoopCollector) RecordSyncDuration(_ context.Context, _ string, _ time.Duration) {}

// RecordSyncedRoutes is a no-op.
func (c *NoopCollector) RecordSyncedRoutes(_ context.Context, _ string, _ int) {}

// RecordIngressRules is a no-op.
func (c *NoopCollector) RecordIngressRules(_ context.Context, _ int) {}

// RecordFailedBackendRefs is a no-op.
func (c *NoopCollector) RecordFailedBackendRefs(_ context.Context, _ string, _ int) {}

// RecordSyncError is a no-op.
func (c *NoopCollector) RecordSyncError(_ context.Context, _ string) {}

// RecordAPICall is a no-op.
func (c *NoopCollector) RecordAPICall(_ context.Context, _, _, _ string, _ time.Duration) {}

// RecordAPIError is a no-op.
func (c *NoopCollector) RecordAPIError(_ context.Context, _, _ string) {}

// RecordHelmOperation is a no-op.
func (c *NoopCollector) RecordHelmOperation(_ context.Context, _, _ string, _ time.Duration) {}

// RecordHelmError is a no-op.
func (c *NoopCollector) RecordHelmError(_ context.Context, _, _ string) {}

// RecordHelmChartInfo is a no-op.
func (c *NoopCollector) RecordHelmChartInfo(_ context.Context, _, _, _ string) {}

// RecordIngressBuildDuration is a no-op.
func (c *NoopCollector) RecordIngressBuildDuration(_ context.Context, _ string, _ time.Duration) {}

// RecordBackendRefValidation is a no-op.
func (c *NoopCollector) RecordBackendRefValidation(_ context.Context, _, _, _ string) {}
