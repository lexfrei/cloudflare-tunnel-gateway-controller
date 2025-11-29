package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
