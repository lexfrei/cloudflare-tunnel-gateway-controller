package render_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// TestAutoscaler_NilWithoutAutoscaling pins the opt-in: no autoscaling block,
// no HPA.
func TestAutoscaler_NilWithoutAutoscaling(t *testing.T) {
	t.Parallel()

	assert.Nil(t, render.Autoscaler(testInput("edge")))
}

// TestAutoscaler_Shape pins the HPA contract: it targets the rendered
// Deployment and scales on the proxy's in-flight gauge as a Pods-type custom
// metric with an AverageValue target — concurrency is the saturation signal
// for an I/O-bound L7 hop, not CPU.
func TestAutoscaler_Shape(t *testing.T) {
	t.Parallel()

	input := testInput("edge")
	input.Config.Spec.Autoscaling = &v1alpha1.ProxyAutoscaling{
		MaxReplicas:          10,
		TargetInflightPerPod: 50,
	}

	hpa := render.Autoscaler(input)
	require.NotNil(t, hpa)

	assert.Equal(t, "cf-proxy-edge", hpa.Name)
	assert.Equal(t, "tenant-a", hpa.Namespace)
	assert.Equal(t, "cf-proxy-edge", hpa.Spec.ScaleTargetRef.Name)
	assert.Equal(t, "Deployment", hpa.Spec.ScaleTargetRef.Kind)

	require.NotNil(t, hpa.Spec.MinReplicas)
	assert.Equal(t, int32(2), *hpa.Spec.MinReplicas, "HA floor default")
	assert.Equal(t, int32(10), hpa.Spec.MaxReplicas)

	require.Len(t, hpa.Spec.Metrics, 1)
	require.Equal(t, autoscalingv2.PodsMetricSourceType, hpa.Spec.Metrics[0].Type)
	require.NotNil(t, hpa.Spec.Metrics[0].Pods)
	assert.Equal(t, "cftunnel_proxy_requests_in_flight", hpa.Spec.Metrics[0].Pods.Metric.Name)
	require.NotNil(t, hpa.Spec.Metrics[0].Pods.Target.AverageValue)
	assert.Equal(t, int64(50), hpa.Spec.Metrics[0].Pods.Target.AverageValue.Value())
}

// TestAutoscaler_Overrides pins minReplicas and metric-name overrides.
func TestAutoscaler_Overrides(t *testing.T) {
	t.Parallel()

	one := int32(1)
	input := testInput("edge")
	input.Config.Spec.Autoscaling = &v1alpha1.ProxyAutoscaling{
		MinReplicas:          &one,
		MaxReplicas:          3,
		TargetInflightPerPod: 100,
		MetricName:           "custom_inflight",
	}

	hpa := render.Autoscaler(input)
	require.NotNil(t, hpa)
	assert.Equal(t, int32(1), *hpa.Spec.MinReplicas)
	assert.Equal(t, "custom_inflight", hpa.Spec.Metrics[0].Pods.Metric.Name)
}
