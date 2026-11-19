package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// reconcileWithPlanesDisabled reconciles pg-gateway with per-Gateway data
// planes disabled (the chart's perGatewayDataPlanes.enabled=false →
// --per-gateway-data-planes=false) and returns the updated Gateway.
func reconcileWithPlanesDisabled(t *testing.T, fakeClient client.WithWatch) gatewayv1.Gateway {
	t.Helper()

	reconciler := &GatewayReconciler{
		Client:                   fakeClient,
		Scheme:                   fakeClient.Scheme(),
		ControllerName:           "test-controller",
		ConfigResolver:           config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
		ProxyImage:               "ghcr.io/example/proxy:v1.2.3",
		PerGatewayPlanesDisabled: true,
	}

	ctx := context.Background()

	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pg-gateway", Namespace: "default"},
	})
	require.NoError(t, err)

	var updated gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "pg-gateway", Namespace: "default"}, &updated))

	return updated
}

// TestGatewayReconciler_PlanesDisabled_RefusesOptedInGateway pins the refusal
// contract of this change: with --per-gateway-data-planes=false, a Gateway
// requesting a dedicated data plane is refused with a clear condition — the
// controller no longer holds the cluster-wide RBAC those planes need, so
// silently not rendering one would leave the operator with a healthy-looking
// Gateway whose routes never work.
func TestGatewayReconciler_PlanesDisabled_RefusesOptedInGateway(t *testing.T) {
	t.Parallel()

	fakeClient := setupGatewayFakeClient(perGatewayStatusFixtures(t)...)
	updated := reconcileWithPlanesDisabled(t, fakeClient)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, reasonPerGatewayPlanesDisabled, accepted.Reason)
	assert.Contains(t, accepted.Message, "per-Gateway data planes are disabled",
		"the condition must say WHY the Gateway is refused")
	assert.Contains(t, accepted.Message, "perGatewayDataPlanes.enabled",
		"the condition must name the chart value that changes the verdict")

	programmed := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionProgrammed))
	require.NotNil(t, programmed)
	assert.Equal(t, metav1.ConditionFalse, programmed.Status)
	assert.Equal(t, string(gatewayv1.GatewayReasonNoResources), programmed.Reason,
		"a disabled feature is missing infrastructure, not a spec mistake")

	assert.LessOrEqual(t, len(refusedConditionPrefix+perGatewayPlanesDisabledMessage), maxConditionMessageLength,
		"the full message must survive truncateMessage, or the remedy gets cut off")
}

// TestGatewayReconciler_PlanesDisabled_RefusesBeforeResolvingSecrets pins the
// ordering that makes the refusal work under the scoped Secret cache: the
// per-Gateway token Secret lives in the Gateway's namespace, which the
// restricted cache cannot read, so the refusal must be decided WITHOUT
// resolving it. The fixtures here deliberately omit the token Secret — if the
// reconcile tried to read it first, it would error or report a resolve
// failure instead of the refusal.
func TestGatewayReconciler_PlanesDisabled_RefusesBeforeResolvingSecrets(t *testing.T) {
	t.Parallel()

	fixtures := perGatewayStatusFixtures(t)
	objs := make([]client.Object, 0, len(fixtures))

	for _, obj := range fixtures {
		if obj.GetName() == "pg-token" {
			continue
		}

		objs = append(objs, obj)
	}

	fakeClient := setupGatewayFakeClient(objs...)
	updated := reconcileWithPlanesDisabled(t, fakeClient)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, reasonPerGatewayPlanesDisabled, accepted.Reason,
		"the refusal must not depend on reading the Gateway's token Secret")
}

// TestGatewayReconciler_PlanesDisabled_SharedGatewayUnaffected pins the other
// half of the toggle: a Gateway on the shared data plane reconciles exactly as
// before — the toggle removes only the per-Gateway machinery.
func TestGatewayReconciler_PlanesDisabled_SharedGatewayUnaffected(t *testing.T) {
	t.Parallel()

	fixtures := perGatewayStatusFixtures(t)
	objs := make([]client.Object, 0, len(fixtures))

	for _, obj := range fixtures {
		if gateway, ok := obj.(*gatewayv1.Gateway); ok {
			gateway = gateway.DeepCopy()
			gateway.Spec.Infrastructure = nil // shared plane
			obj = gateway
		}

		objs = append(objs, obj)
	}

	fakeClient := setupGatewayFakeClient(objs...)
	updated := reconcileWithPlanesDisabled(t, fakeClient)

	accepted := findCondition(updated.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status,
		"a shared-plane Gateway must keep working with per-Gateway planes disabled")
}
