package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestGatewayInfraReconciler_RefusedGatewayLosesItsPlane pins that a Gateway
// the Gateway reconciler refuses as a whole has no running plane: its
// connector would stay registered on the tunnel and, on a shared token, answer
// a share of another Gateway's requests with 404s.
func TestGatewayInfraReconciler_RefusedGatewayLosesItsPlane(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		refuse func(*gatewayv1.Gateway)
	}{
		{name: "spec.tls.frontend", refuse: func(gateway *gatewayv1.Gateway) {
			gateway.Spec.TLS = &gatewayv1.GatewayTLSConfig{Frontend: &gatewayv1.FrontendTLSConfig{}}
		}},
		{name: "unsupported address type", refuse: func(gateway *gatewayv1.Gateway) {
			gateway.Spec.Addresses = []gatewayv1.GatewaySpecAddress{{Value: "192.0.2.1"}}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			reconciler := newInfraReconciler(t, infraFixtures(t)...)
			reconcileEdge(t, reconciler)

			deploymentKey := types.NamespacedName{Name: "cf-proxy-edge", Namespace: infraNamespace}
			require.NoError(t, reconciler.Get(ctx, deploymentKey, &appsv1.Deployment{}),
				"the plane must exist before the Gateway is refused")

			var gateway gatewayv1.Gateway
			require.NoError(t, reconciler.Get(ctx, types.NamespacedName{Name: "edge", Namespace: infraNamespace}, &gateway))
			tt.refuse(&gateway)
			require.NoError(t, reconciler.Update(ctx, &gateway))

			reconcileEdge(t, reconciler)

			err := reconciler.Get(ctx, deploymentKey, &appsv1.Deployment{})
			assert.True(t, apierrors.IsNotFound(err), "expected NotFound, got %v", err)
		})
	}
}
