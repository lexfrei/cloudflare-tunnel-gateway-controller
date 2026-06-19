package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMetricsEnabled_DefaultTrueMatrix pins the default-ON semantics of
// PROXY_METRICS_ENABLED: only an explicit falsy value disables the /metrics
// endpoint. This is the inverse of the isTruthyEnv convention used by the
// other toggles, deliberately — metrics are harmless and cheap, and an
// operator should get them without setting anything.
func TestMetricsEnabled_DefaultTrueMatrix(t *testing.T) {
	tests := []struct {
		name  string
		value string
		set   bool
		want  bool
	}{
		{name: "unset means enabled", set: false, want: true},
		{name: "empty means enabled", set: true, value: "", want: true},
		{name: "true stays enabled", set: true, value: "true", want: true},
		{name: "1 stays enabled", set: true, value: "1", want: true},
		{name: "false disables", set: true, value: "false", want: false},
		{name: "FALSE disables case-insensitively", set: true, value: "FALSE", want: false},
		{name: "0 disables", set: true, value: "0", want: false},
		{name: "garbage stays enabled", set: true, value: "banana", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv("PROXY_METRICS_ENABLED", tt.value)
			}

			assert.Equal(t, tt.want, metricsEnabled())
		})
	}
}

// TestBuildProxyMetrics_DisabledReturnsNil pins the wiring contract both run
// modes rely on: disabled → no handler option, no endpoint handler.
func TestBuildProxyMetrics_DisabledReturnsNil(t *testing.T) {
	t.Setenv("PROXY_METRICS_ENABLED", "false")

	opt, handler := buildProxyMetrics()
	assert.Nil(t, opt)
	assert.Nil(t, handler)
}

// TestBuildProxyMetrics_EnabledReturnsBoth pins the enabled path: an option
// for the handler and an exposition handler for the config API.
func TestBuildProxyMetrics_EnabledReturnsBoth(t *testing.T) {
	t.Setenv("PROXY_METRICS_ENABLED", "true")

	opt, handler := buildProxyMetrics()
	require.NotNil(t, opt)
	require.NotNil(t, handler)
}
