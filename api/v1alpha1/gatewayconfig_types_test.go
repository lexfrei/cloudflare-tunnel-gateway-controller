package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// TestLocalSecretReference_KeyOr pins the per-context key defaulting the
// per-Gateway resolver relies on: the tunnel token defaults to "tunnel-token"
// (matching the chart's proxy.tunnelTokenSecretRef convention) and the auth
// token to "auth-token".
func TestLocalSecretReference_KeyOr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ref      v1alpha1.LocalSecretReference
		fallback string
		want     string
	}{
		{
			name:     "empty key uses fallback",
			ref:      v1alpha1.LocalSecretReference{Name: "token"},
			fallback: "tunnel-token",
			want:     "tunnel-token",
		},
		{
			name:     "explicit key wins",
			ref:      v1alpha1.LocalSecretReference{Name: "token", Key: "custom"},
			fallback: "tunnel-token",
			want:     "custom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, tt.ref.KeyOr(tt.fallback))
		})
	}
}

// TestProxyAutoscaling_EffectiveMinReplicas pins the render-time default: an
// unset minReplicas means 2 (the proxy's HA default — a single connector pod
// is a tunnel availability hazard).
func TestProxyAutoscaling_EffectiveMinReplicas(t *testing.T) {
	t.Parallel()

	unset := v1alpha1.ProxyAutoscaling{MaxReplicas: 5, TargetInflightPerPod: 50}
	assert.Equal(t, int32(2), unset.EffectiveMinReplicas())

	one := int32(1)
	explicit := v1alpha1.ProxyAutoscaling{MinReplicas: &one, MaxReplicas: 5, TargetInflightPerPod: 50}
	assert.Equal(t, int32(1), explicit.EffectiveMinReplicas())
}

// TestProxyAutoscaling_EffectiveMetricName pins the default metric the HPA
// scales on: the proxy's in-flight gauge from the data-plane metrics.
func TestProxyAutoscaling_EffectiveMetricName(t *testing.T) {
	t.Parallel()

	unset := v1alpha1.ProxyAutoscaling{MaxReplicas: 5, TargetInflightPerPod: 50}
	assert.Equal(t, "cftunnel_proxy_requests_in_flight", unset.EffectiveMetricName())

	custom := v1alpha1.ProxyAutoscaling{MaxReplicas: 5, TargetInflightPerPod: 50, MetricName: "my_metric"}
	assert.Equal(t, "my_metric", custom.EffectiveMetricName())
}
