package tunnel

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCatchAllIngress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		proxyURL string
		wantErr  bool
	}{
		{
			name:     "valid HTTP URL",
			proxyURL: "http://localhost:8080",
			wantErr:  false,
		},
		{
			name:     "valid HTTPS URL",
			proxyURL: "https://backend.svc.cluster.local:443",
			wantErr:  false,
		},
		{
			name:     "empty URL produces error",
			proxyURL: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := buildCatchAllIngress(tt.proxyURL)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.NotEmpty(t, result.Rules, "ingress rules should not be empty")
		})
	}
}

func TestBuildRootCAPool(t *testing.T) {
	t.Parallel()

	pool, err := buildRootCAPool()

	require.NoError(t, err)
	assert.NotNil(t, pool, "root CA pool should not be nil")
}

func TestNewZerologLogger(t *testing.T) {
	t.Parallel()

	logger := newZerologLogger()

	assert.NotNil(t, logger, "zerolog logger should not be nil")
}
