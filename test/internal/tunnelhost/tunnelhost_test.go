package tunnelhost

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveFrom pins the shared resolver's precedence and fail-fast contract:
// first non-empty key wins, later keys are fallbacks, and an all-empty (or
// no-key) lookup errors. There is deliberately no default — a hardcoded test
// hostname silently routed the suites at a stale tunnel. Driven by a fake
// getenv so the subtests stay parallel without mutating process env.
func TestResolveFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		keys    []string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{
			name:    "no keys errors",
			keys:    nil,
			wantErr: true,
		},
		{
			name:    "single key unset errors",
			keys:    []string{"CONFORMANCE_TUNNEL_HOSTNAME"},
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name: "single key set returns value",
			keys: []string{"CONFORMANCE_TUNNEL_HOSTNAME"},
			env:  map[string]string{"CONFORMANCE_TUNNEL_HOSTNAME": "edge.example.com"},
			want: "edge.example.com",
		},
		{
			name: "first key wins over later",
			keys: []string{"E2E_TUNNEL_HOSTNAME", "CONFORMANCE_TUNNEL_HOSTNAME"},
			env: map[string]string{
				"E2E_TUNNEL_HOSTNAME":         "primary.example.com",
				"CONFORMANCE_TUNNEL_HOSTNAME": "fallback.example.com",
			},
			want: "primary.example.com",
		},
		{
			name: "later key used when first empty",
			keys: []string{"E2E_TUNNEL_HOSTNAME", "CONFORMANCE_TUNNEL_HOSTNAME"},
			env:  map[string]string{"CONFORMANCE_TUNNEL_HOSTNAME": "fallback.example.com"},
			want: "fallback.example.com",
		},
		{
			name: "all keys empty errors",
			keys: []string{"E2E_TUNNEL_HOSTNAME", "CONFORMANCE_TUNNEL_HOSTNAME"},
			env: map[string]string{
				"E2E_TUNNEL_HOSTNAME":         "",
				"CONFORMANCE_TUNNEL_HOSTNAME": "",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			getenv := func(key string) string { return tt.env[key] }

			got, err := resolveFrom(getenv, tt.keys...)
			if tt.wantErr {
				require.Error(t, err)
				assert.Empty(t, got)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
