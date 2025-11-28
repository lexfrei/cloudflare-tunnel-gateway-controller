package helm

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
