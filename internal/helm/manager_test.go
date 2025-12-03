package helm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v4/pkg/chart"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/metrics"
)

func TestExtractRepoFromOCI(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		chartRef string
		expected string
	}{
		{
			name:     "standard OCI reference",
			chartRef: "oci://ghcr.io/lexfrei/charts/cloudflare-tunnel",
			expected: "ghcr.io/lexfrei/charts/cloudflare-tunnel",
		},
		{
			name:     "docker hub reference",
			chartRef: "oci://docker.io/library/nginx",
			expected: "docker.io/library/nginx",
		},
		{
			name:     "reference without OCI prefix - gets truncated",
			chartRef: "ghcr.io/lexfrei/charts/cloudflare-tunnel",
			expected: "o/lexfrei/charts/cloudflare-tunnel",
		},
		{
			name:     "empty string",
			chartRef: "",
			expected: "",
		},
		{
			name:     "only OCI prefix - returns itself when length equals prefix",
			chartRef: "oci://",
			expected: "oci://",
		},
		{
			name:     "exact oci:// prefix",
			chartRef: "oci://x",
			expected: "x",
		},
		{
			name:     "short string",
			chartRef: "oci",
			expected: "oci",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := extractRepoFromOCI(tt.chartRef)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExtractChartName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		chartRef string
		expected string
	}{
		{
			name:     "OCI reference",
			chartRef: "oci://ghcr.io/lexfrei/charts/cloudflare-tunnel",
			expected: "cloudflare-tunnel",
		},
		{
			name:     "simple path",
			chartRef: "/path/to/my-chart",
			expected: "my-chart",
		},
		{
			name:     "nested path",
			chartRef: "registry.example.com/org/repo/chart-name",
			expected: "chart-name",
		},
		{
			name:     "single element",
			chartRef: "chart-name",
			expected: "chart-name",
		},
		{
			name:     "empty string",
			chartRef: "",
			expected: ".",
		},
		{
			name:     "with trailing slash - filepath.Base behavior",
			chartRef: "oci://ghcr.io/charts/",
			expected: "charts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := extractChartName(tt.chartRef)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDefaultChartRef(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "oci://ghcr.io/lexfrei/charts/cloudflare-tunnel", DefaultChartRef)
	assert.Contains(t, DefaultChartRef, "oci://")
}

func TestNewManager(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(metrics.NewNoopCollector())

	require.NoError(t, err)
	require.NotNil(t, manager)
	assert.NotNil(t, manager.settings)
	assert.NotNil(t, manager.registryClient)
	assert.Nil(t, manager.chartCache)
	assert.Empty(t, manager.chartVersion)
}

func TestManager_GetLatestVersion_RealRegistry(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	manager, err := NewManager(metrics.NewNoopCollector())
	require.NoError(t, err)

	ctx := context.Background()

	version, err := manager.GetLatestVersion(ctx, DefaultChartRef)

	require.NoError(t, err)
	assert.NotEmpty(t, version)
	assert.Regexp(t, `^\d+\.\d+\.\d+`, version)
}

func TestManager_GetLatestVersion_InvalidRegistry(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(metrics.NewNoopCollector())
	require.NoError(t, err)

	ctx := context.Background()

	_, err = manager.GetLatestVersion(ctx, "oci://invalid.registry.local/nonexistent/chart")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get tags from registry")
}

func TestManager_LoadChart_RealRegistry(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	manager, err := NewManager(metrics.NewNoopCollector())
	require.NoError(t, err)

	ctx := context.Background()

	version, err := manager.GetLatestVersion(ctx, DefaultChartRef)
	require.NoError(t, err)

	loadedChart, err := manager.LoadChart(ctx, DefaultChartRef, version)

	require.NoError(t, err)
	require.NotNil(t, loadedChart)

	accessor, err := chart.NewAccessor(loadedChart)
	require.NoError(t, err)
	assert.Equal(t, "cloudflare-tunnel", accessor.Name())
}

func TestManager_LoadChart_CacheHit(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	manager, err := NewManager(metrics.NewNoopCollector())
	require.NoError(t, err)

	ctx := context.Background()

	version, err := manager.GetLatestVersion(ctx, DefaultChartRef)
	require.NoError(t, err)

	chart1, err := manager.LoadChart(ctx, DefaultChartRef, version)
	require.NoError(t, err)

	chart2, err := manager.LoadChart(ctx, DefaultChartRef, version)
	require.NoError(t, err)

	assert.Same(t, chart1, chart2)
}

func TestManager_LoadChart_InvalidChart(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(metrics.NewNoopCollector())
	require.NoError(t, err)

	ctx := context.Background()

	_, err = manager.LoadChart(ctx, "oci://invalid.registry.local/nonexistent/chart", "1.0.0")

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to pull chart")
}

func TestManager_GetLatestVersion_TableDriven(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tests := []struct {
		name      string
		chartRef  string
		wantErr   bool
		errSubstr string
	}{
		{
			name:     "valid chart ref",
			chartRef: DefaultChartRef,
			wantErr:  false,
		},
		{
			name:      "invalid registry",
			chartRef:  "oci://invalid.registry.example/test/chart",
			wantErr:   true,
			errSubstr: "failed to get tags",
		},
		{
			name:      "empty chart ref",
			chartRef:  "",
			wantErr:   true,
			errSubstr: "failed to get tags",
		},
	}

	manager, err := NewManager(metrics.NewNoopCollector())
	require.NoError(t, err)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			version, err := manager.GetLatestVersion(ctx, tt.chartRef)

			if tt.wantErr {
				assert.Error(t, err)
				if tt.errSubstr != "" {
					assert.Contains(t, err.Error(), tt.errSubstr)
				}
			} else {
				assert.NoError(t, err)
				assert.NotEmpty(t, version)
			}
		})
	}
}

func TestManager_ConcurrentLoadChart(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	manager, err := NewManager(metrics.NewNoopCollector())
	require.NoError(t, err)

	ctx := context.Background()
	version, err := manager.GetLatestVersion(ctx, DefaultChartRef)
	require.NoError(t, err)

	const concurrency = 5
	results := make(chan chart.Charter, concurrency)
	errors := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		go func() {
			loadedChart, loadErr := manager.LoadChart(ctx, DefaultChartRef, version)
			if loadErr != nil {
				errors <- loadErr
				return
			}
			results <- loadedChart
		}()
	}

	var charts []chart.Charter
	for i := 0; i < concurrency; i++ {
		select {
		case ch := <-results:
			charts = append(charts, ch)
		case err := <-errors:
			t.Fatalf("concurrent load failed: %v", err)
		}
	}

	require.Len(t, charts, concurrency)

	for i := 1; i < len(charts); i++ {
		assert.Same(t, charts[0], charts[i], "all charts should be same cached instance")
	}
}
