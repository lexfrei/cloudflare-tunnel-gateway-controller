package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectorInterface(t *testing.T) {
	t.Parallel()

	// Verify that prometheusCollector implements Collector interface
	var _ Collector = (*prometheusCollector)(nil)
	var _ Collector = (*NoopCollector)(nil)
}

func TestNewCollector(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg)

	require.NotNil(t, collector)
	assert.IsType(t, &prometheusCollector{}, collector)
}

func TestNoopCollector(t *testing.T) {
	t.Parallel()

	collector := NewNoopCollector()
	require.NotNil(t, collector)

	ctx := context.Background()

	// All methods should not panic
	assert.NotPanics(t, func() {
		collector.RecordSyncDuration(ctx, "success", time.Second)
		collector.RecordSyncedRoutes(ctx, "http", 5)
		collector.RecordIngressRules(ctx, 10)
		collector.RecordFailedBackendRefs(ctx, "http", 2)
		collector.RecordSyncError(ctx, "timeout")
		collector.RecordAPICall(ctx, "get", "tunnel_config", "success", time.Second)
		collector.RecordAPIError(ctx, "get", "auth")
		collector.RecordHelmOperation(ctx, "install", "success", time.Second)
		collector.RecordHelmError(ctx, "install", "timeout")
		collector.RecordHelmChartInfo(ctx, "cloudflare-tunnel", "0.1.0", "2024.1.0")
		collector.RecordIngressBuildDuration(ctx, "http", time.Millisecond*100)
		collector.RecordBackendRefValidation(ctx, "http", "accepted", "")
	})
}

func TestMetricsRegistration(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	// Trigger all metrics to be collected at least once
	collector.RecordSyncDuration(ctx, "success", time.Second)
	collector.RecordSyncedRoutes(ctx, "http", 1)
	collector.RecordIngressRules(ctx, 1)
	collector.RecordFailedBackendRefs(ctx, "http", 0)
	collector.RecordSyncError(ctx, "test")
	collector.RecordAPICall(ctx, "get", "tunnel_config", "success", time.Second)
	collector.RecordAPIError(ctx, "get", "test")
	collector.RecordHelmOperation(ctx, "install", "success", time.Second)
	collector.RecordHelmError(ctx, "install", "test")
	collector.RecordHelmChartInfo(ctx, "test", "1.0.0", "1.0.0")
	collector.RecordIngressBuildDuration(ctx, "http", time.Millisecond)
	collector.RecordBackendRefValidation(ctx, "http", "accepted", "")

	// Verify metrics are registered
	metricFamilies, err := reg.Gather()
	require.NoError(t, err)

	expectedMetrics := []string{
		"cftunnel_sync_duration_seconds",
		"cftunnel_synced_routes",
		"cftunnel_ingress_rules",
		"cftunnel_failed_backend_refs",
		"cftunnel_sync_errors_total",
		"cftunnel_cloudflare_api_duration_seconds",
		"cftunnel_cloudflare_api_calls_total",
		"cftunnel_cloudflare_api_errors_total",
		"cftunnel_helm_operation_duration_seconds",
		"cftunnel_helm_operations_total",
		"cftunnel_helm_errors_total",
		"cftunnel_helm_chart_info",
		"cftunnel_ingress_build_duration_seconds",
		"cftunnel_backend_ref_validation_total",
	}

	registeredMetrics := make(map[string]bool)
	for _, mf := range metricFamilies {
		registeredMetrics[mf.GetName()] = true
	}

	for _, expected := range expectedMetrics {
		assert.True(t, registeredMetrics[expected], "metric %s should be registered", expected)
	}
}

func TestRecordSyncDuration(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	collector.RecordSyncDuration(ctx, "success", time.Second)

	// Check that histogram was observed
	count := testutil.CollectAndCount(collector.syncDuration)
	assert.Equal(t, 1, count)
}

func TestRecordSyncedRoutes(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	collector.RecordSyncedRoutes(ctx, "http", 5)
	collector.RecordSyncedRoutes(ctx, "grpc", 3)

	httpCount := testutil.ToFloat64(collector.syncedRoutes.WithLabelValues("http"))
	grpcCount := testutil.ToFloat64(collector.syncedRoutes.WithLabelValues("grpc"))

	assert.Equal(t, float64(5), httpCount)
	assert.Equal(t, float64(3), grpcCount)
}

func TestRecordIngressRules(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	collector.RecordIngressRules(ctx, 10)

	count := testutil.ToFloat64(collector.ingressRulesTotal)
	assert.Equal(t, float64(10), count)
}

func TestRecordFailedBackendRefs(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	collector.RecordFailedBackendRefs(ctx, "http", 2)

	count := testutil.ToFloat64(collector.failedBackendRefs.WithLabelValues("http"))
	assert.Equal(t, float64(2), count)
}

func TestRecordSyncError(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	collector.RecordSyncError(ctx, "timeout")
	collector.RecordSyncError(ctx, "timeout")
	collector.RecordSyncError(ctx, "network")

	timeoutCount := testutil.ToFloat64(collector.syncErrorsTotal.WithLabelValues("timeout"))
	networkCount := testutil.ToFloat64(collector.syncErrorsTotal.WithLabelValues("network"))

	assert.Equal(t, float64(2), timeoutCount)
	assert.Equal(t, float64(1), networkCount)
}

func TestRecordAPICall(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	collector.RecordAPICall(ctx, "get", "tunnel_config", "success", time.Second)

	// Check histogram and counter
	durationCount := testutil.CollectAndCount(collector.apiDuration)
	callsCount := testutil.ToFloat64(collector.apiCallsTotal.WithLabelValues("get", "tunnel_config", "success"))

	assert.Equal(t, 1, durationCount)
	assert.Equal(t, float64(1), callsCount)
}

func TestRecordAPIError(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	collector.RecordAPIError(ctx, "get", "auth")

	count := testutil.ToFloat64(collector.apiErrorsTotal.WithLabelValues("get", "auth"))
	assert.Equal(t, float64(1), count)
}

func TestRecordHelmOperation(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	collector.RecordHelmOperation(ctx, "install", "success", time.Second)

	// Check histogram and counter
	durationCount := testutil.CollectAndCount(collector.helmDuration)
	opsCount := testutil.ToFloat64(collector.helmOpsTotal.WithLabelValues("install", "success"))

	assert.Equal(t, 1, durationCount)
	assert.Equal(t, float64(1), opsCount)
}

func TestRecordHelmError(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	collector.RecordHelmError(ctx, "install", "timeout")

	count := testutil.ToFloat64(collector.helmErrorsTotal.WithLabelValues("install", "timeout"))
	assert.Equal(t, float64(1), count)
}

func TestRecordHelmChartInfo(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	collector.RecordHelmChartInfo(ctx, "cloudflare-tunnel", "0.1.0", "2024.1.0")

	count := testutil.ToFloat64(collector.helmChartInfo.WithLabelValues("cloudflare-tunnel", "0.1.0", "2024.1.0"))
	assert.Equal(t, float64(1), count)
}

func TestRecordIngressBuildDuration(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	collector.RecordIngressBuildDuration(ctx, "http", time.Millisecond*100)

	// Check histogram was observed
	count := testutil.CollectAndCount(collector.ingressBuildDuration)
	assert.Equal(t, 1, count)
}

func TestRecordBackendRefValidation(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	collector.RecordBackendRefValidation(ctx, "http", "accepted", "")
	collector.RecordBackendRefValidation(ctx, "http", "rejected", "not_found")

	acceptedCount := testutil.ToFloat64(collector.backendRefValidation.WithLabelValues("http", "accepted", ""))
	rejectedCount := testutil.ToFloat64(collector.backendRefValidation.WithLabelValues("http", "rejected", "not_found"))

	assert.Equal(t, float64(1), acceptedCount)
	assert.Equal(t, float64(1), rejectedCount)
}

func TestHistogramBuckets(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	collector := NewCollector(reg).(*prometheusCollector)
	ctx := context.Background()

	// Record sync duration of 1 second
	collector.RecordSyncDuration(ctx, "success", time.Second)

	// Verify histogram was collected (bucket verification via lint)
	count := testutil.CollectAndCount(collector.syncDuration)
	assert.Equal(t, 1, count)
}
