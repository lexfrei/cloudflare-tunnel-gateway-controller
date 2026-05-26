package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_RequiresProxyEndpoints pins the v3 bootstrap invariant: the
// controller must fail loudly when --proxy-endpoints is empty, because the
// L7 proxy is the only data plane and a controller with no push targets
// would silently no-op every HTTPRoute reconcile. The early-return path
// in Run() exists specifically to prevent the silent-installation-with-
// broken-proxy failure mode -- if someone refactors initProxySyncer to
// re-introduce a nil-syncer fallback, this test fires before any cluster
// connection is attempted (we never reach ctrl.GetConfigOrDie).
func TestRun_RequiresProxyEndpoints(t *testing.T) {
	t.Parallel()

	err := Run(context.Background(), &Config{
		ControllerName: "test-controller",
		ProxyEndpoints: nil,
	})

	require.Error(t, err)
	assert.True(t,
		strings.Contains(err.Error(), "--proxy-endpoints is required"),
		"error should mention --proxy-endpoints requirement, got: %v", err)
}

// TestRun_RequiresProxyEndpoints_EmptySlice covers the slice-but-empty
// variant: viper.GetStringSlice can return a non-nil empty slice when the
// flag is provided without any value, and the guard uses len() rather than
// a nil check so both shapes must reject identically.
func TestRun_RequiresProxyEndpoints_EmptySlice(t *testing.T) {
	t.Parallel()

	err := Run(context.Background(), &Config{
		ControllerName: "test-controller",
		ProxyEndpoints: []string{},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--proxy-endpoints is required")
}

func TestGetControllerNamespace(t *testing.T) {
	// Cannot use t.Parallel() because t.Setenv() requires sequential execution.
	tests := []struct {
		name     string
		envValue string
		expected string
	}{
		{
			name:     "from environment variable",
			envValue: "test-namespace",
			expected: "test-namespace",
		},
		{
			name:     "fallback to default when env not set",
			envValue: "",
			expected: "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				t.Setenv("CONTROLLER_NAMESPACE", tt.envValue)
			}

			result := getControllerNamespace()
			assert.Equal(t, tt.expected, result)
		})
	}
}
