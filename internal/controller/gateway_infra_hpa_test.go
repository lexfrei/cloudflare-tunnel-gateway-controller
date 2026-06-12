package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/types"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// TestGatewayInfraReconciler_AutoscalerLifecycle pins the HPA lifecycle: an
// autoscaling block renders an owned HPA; removing the block deletes it.
func TestGatewayInfraReconciler_AutoscalerLifecycle(t *testing.T) {
	t.Parallel()

	objects := infraFixtures(t)
	for _, obj := range objects {
		if gwConfig, ok := obj.(*v1alpha1.GatewayConfig); ok {
			gwConfig.Spec.Autoscaling = &v1alpha1.ProxyAutoscaling{MaxReplicas: 10, TargetInflightPerPod: 50}
		}
	}

	reconciler := newInfraReconciler(t, objects...)
	reconcileEdge(t, reconciler)

	ctx := context.Background()
	key := types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}

	var hpa autoscalingv2.HorizontalPodAutoscaler
	require.NoError(t, reconciler.Get(ctx, key, &hpa))
	assert.Equal(t, int32(10), hpa.Spec.MaxReplicas)
	require.Len(t, hpa.OwnerReferences, 1)
	assert.Equal(t, "edge", hpa.OwnerReferences[0].Name)

	// Remove the autoscaling block — the HPA must be cleaned up.
	var gwConfig v1alpha1.GatewayConfig
	require.NoError(t, reconciler.Get(ctx, types.NamespacedName{Name: "edge-config", Namespace: infraNamespace}, &gwConfig))
	gwConfig.Spec.Autoscaling = nil
	require.NoError(t, reconciler.Update(ctx, &gwConfig))

	reconcileEdge(t, reconciler)

	err := reconciler.Get(ctx, key, &hpa)
	assert.Error(t, err, "the HPA must be deleted once autoscaling is unset")
}
