//go:build envtest

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// TestEnvtest_ServesGatewayAPIResources pins that the envtest apiserver serves
// the Gateway API kinds the controller reads, at the API versions it reads
// them in, so envtest tests can create those objects instead of falling back
// to the fake client.
func TestEnvtest_ServesGatewayAPIResources(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	for _, list := range []client.ObjectList{
		&gatewayv1.GatewayClassList{},
		&gatewayv1.GatewayList{},
		&gatewayv1.ListenerSetList{},
		&gatewayv1.HTTPRouteList{},
		&gatewayv1.GRPCRouteList{},
		&gatewayv1.BackendTLSPolicyList{},
		&gatewayv1beta1.ReferenceGrantList{},
	} {
		require.NoError(t, envK8sClient.List(ctx, list), "%T", list)
	}
}

// TestEnvtest_GatewayAPISchemaValidation pins that the loaded CRDs carry
// their schema: a Gateway without listeners fails the CRD's minItems at
// admission, which the fake client would accept.
func TestEnvtest_GatewayAPISchemaValidation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "gwapi-"}}
	require.NoError(t, envK8sClient.Create(ctx, ns))

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "no-listeners", Namespace: ns.Name},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel"},
	}

	err := envK8sClient.Create(ctx, gateway)
	require.Error(t, err)
	require.ErrorContains(t, err, "spec.listeners")
}
