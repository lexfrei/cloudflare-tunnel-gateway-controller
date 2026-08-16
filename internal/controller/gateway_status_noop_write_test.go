package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// The fake client bumps resourceVersion on every Status().Update, so an
// unchanged resourceVersion across two reconciles proves the second write was
// skipped rather than rewritten with identical content.
func reconcileGatewayTwice(t *testing.T, reconciler *GatewayReconciler, key types.NamespacedName) (string, string) {
	t.Helper()

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: key}

	_, err := reconciler.Reconcile(ctx, req)
	require.NoError(t, err)

	var first gatewayv1.Gateway
	require.NoError(t, reconciler.Get(ctx, key, &first))

	_, err = reconciler.Reconcile(ctx, req)
	require.NoError(t, err)

	var second gatewayv1.Gateway
	require.NoError(t, reconciler.Get(ctx, key, &second))

	return first.ResourceVersion, second.ResourceVersion
}

func TestGatewayReconciler_SecondReconcileWithNoChangeSkipsStatusWrite(t *testing.T) {
	t.Parallel()

	gateway := gatewayWithSeededListenerStatus(nil)
	gatewayClass, classConfig, secret := listenerTransitionFixtures()
	fakeClient := setupGatewayFakeClient(gateway, classConfig, secret, gatewayClass)

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
	}

	first, second := reconcileGatewayTwice(t, reconciler, types.NamespacedName{Name: "gw", Namespace: "default"})
	assert.Equal(t, first, second, "an unchanged Gateway status must not be written again")
}

func TestGatewayReconciler_ConfigError_SecondReconcileWithNoChangeSkipsStatusWrite(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gateway", Namespace: "default", Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: "test-controller",
			ParametersRef: &gatewayv1.ParametersReference{
				Group: config.ParametersRefGroup,
				Kind:  config.ParametersRefKind,
				Name:  "nonexistent-config",
			},
		},
	}
	fakeClient := setupGatewayFakeClient(gateway, gatewayClass)

	reconciler := &GatewayReconciler{
		Client:         fakeClient,
		Scheme:         fakeClient.Scheme(),
		ControllerName: "test-controller",
		ConfigResolver: config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector()),
	}

	first, second := reconcileGatewayTwice(t, reconciler, types.NamespacedName{Name: "test-gateway", Namespace: "default"})
	assert.Equal(t, first, second, "an unchanged config-error status must not be written again")
}
