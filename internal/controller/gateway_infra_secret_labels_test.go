package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestGatewayInfraReconciler_GeneratedSecretsCarryInfrastructureMetadata pins
// that the Secrets created for a per-Gateway data plane carry
// spec.infrastructure labels and annotations and the well-known Gateway
// labels, as the rendered Deployment does.
func TestGatewayInfraReconciler_GeneratedSecretsCarryInfrastructureMetadata(t *testing.T) {
	t.Parallel()

	reconciler, _ := newTLSInfraReconciler(t)

	var gateway gatewayv1.Gateway
	require.NoError(t, reconciler.Get(context.Background(),
		types.NamespacedName{Name: "edge", Namespace: infraNamespace}, &gateway))

	gateway.Spec.Infrastructure.Labels = map[gatewayv1.LabelKey]gatewayv1.LabelValue{"team": "payments"}
	gateway.Spec.Infrastructure.Annotations = map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue{"owner": "ops"}
	require.NoError(t, reconciler.Update(context.Background(), &gateway))

	reconcileEdgeResult(t, reconciler)

	for _, key := range []types.NamespacedName{
		{Name: "cf-proxy-edge-auth", Namespace: infraNamespace},
		edgeLeafKey("0"),
	} {
		var secret corev1.Secret
		require.NoError(t, reconciler.Get(context.Background(), key, &secret))

		assert.Equal(t, "payments", secret.Labels["team"], key.Name)
		assert.Equal(t, "edge", secret.Labels[gatewayv1.GatewayNameLabelKey], key.Name)
		assert.Equal(t, "cloudflare-tunnel", secret.Labels[gatewayv1.GatewayClassNameLabelKey], key.Name)
		assert.Equal(t, "ops", secret.Annotations["owner"], key.Name)
	}
}
