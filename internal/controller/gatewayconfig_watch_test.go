package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
)

// TestGatewayConfigToGateways pins the GatewayConfig → Gateway mapper: a
// GatewayConfig event enqueues the same-namespace Gateway that references it
// via infrastructure.parametersRef, and nothing else — so an edit that does
// not change the rendered Deployment still refreshes the Gateway's status.
func TestGatewayConfigToGateways(t *testing.T) {
	t.Parallel()

	referencing := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "team-a"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: &gatewayv1.LocalParametersReference{
					Group: "cf.k8s.lex.la", Kind: "GatewayConfig", Name: "edge-config",
				},
			},
		},
	}
	otherName := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "team-a"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Infrastructure: &gatewayv1.GatewayInfrastructure{
				ParametersRef: &gatewayv1.LocalParametersReference{
					Group: "cf.k8s.lex.la", Kind: "GatewayConfig", Name: "other-config",
				},
			},
		},
	}
	shared := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team-a"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel"},
	}

	fakeClient := setupGatewayFakeClient(referencing, otherName, shared)
	reconciler := &GatewayReconciler{Client: fakeClient, Scheme: fakeClient.Scheme()}

	gwConfig := &v1alpha1.GatewayConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "edge-config", Namespace: "team-a"},
	}

	requests := reconciler.gatewayConfigToGateways(context.Background(), gwConfig)

	assert.Len(t, requests, 1, "only the Gateway whose parametersRef names this GatewayConfig is enqueued")
	if len(requests) == 1 {
		assert.Equal(t, "edge", requests[0].Name)
		assert.Equal(t, "team-a", requests[0].Namespace)
	}
}
