package coregroup_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/coregroup"
)

func TestIs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		group string
		want  bool
	}{
		{group: "", want: true},
		{group: "core", want: true},
		{group: "Core", want: false},
		{group: "gateway.networking.k8s.io", want: false},
		{group: "multicluster.x-k8s.io", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.group, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, coregroup.Is(tt.group))
		})
	}
}
