package config

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
)

// TestNewResolver_WithCloudflareTracing pins the option wiring: the tracing
// flag is off by default and set by WithCloudflareTracing.
func TestNewResolver_WithCloudflareTracing(t *testing.T) {
	t.Parallel()

	assert.False(t, NewResolver(nil, "default", cfmetrics.NewNoopCollector()).tracing,
		"tracing must be off by default")
	assert.True(t, NewResolver(nil, "default", cfmetrics.NewNoopCollector(), WithCloudflareTracing()).tracing,
		"WithCloudflareTracing must enable tracing")
}

// TestCloudflareRequestOptions_TracingTogglesHTTPClient pins that the traced
// HTTP client option is appended only when tracing is enabled, so the disabled
// path leaves cloudflare-go on its own default client.
func TestCloudflareRequestOptions_TracingTogglesHTTPClient(t *testing.T) {
	t.Parallel()

	assert.Len(t, cloudflareRequestOptions("token", false), 1,
		"no tracing -> only the API-token option")
	assert.Len(t, cloudflareRequestOptions("token", true), 2,
		"tracing -> API-token + traced HTTP client options")
}
