package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// holdInTermination deletes the Gateway while a finalizer keeps the object in
// the apiserver, which is how any finalizer, or a foreground-cascading delete,
// leaves a Gateway: DeletionTimestamp set, object still listed.
func holdInTermination(t *testing.T, fakeClient client.Client, namespace, name string) {
	t.Helper()

	ctx := context.Background()

	var gateway gatewayv1.Gateway
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &gateway))

	gateway.Finalizers = append(gateway.Finalizers, "example.com/hold")
	require.NoError(t, fakeClient.Update(ctx, &gateway))
	require.NoError(t, fakeClient.Delete(ctx, &gateway))

	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &gateway))
	require.False(t, gateway.DeletionTimestamp.IsZero(), "the fixture must leave %s/%s terminating", namespace, name)
}

// TestManagedInfraGateways_KeepsTerminatingGateways pins the listing every plane
// rule reads. A terminating Gateway's plane is owned by it and outlives the
// start of its deletion, so dropping it here would release its tunnel to
// another namespace and its slot to a sibling while its connector still serves,
// and would route its routes to the shared plane, since a Gateway the
// partitioner never hears of falls through to the shared partition.
func TestManagedInfraGateways_KeepsTerminatingGateways(t *testing.T) {
	t.Parallel()

	fakeClient := quotaFixtures(t, nil)
	holdInTermination(t, fakeClient, "tenant", "gw-old")

	gateways, err := managedInfraGateways(context.Background(), fakeClient, "test-controller")
	require.NoError(t, err)

	keys := make([]string, 0, len(gateways))
	for _, gateway := range gateways {
		keys = append(keys, gateway.Namespace+"/"+gateway.Name)
	}

	assert.Contains(t, keys, "tenant/gw-old", "a terminating Gateway must stay in scope until it is gone")
}

// TestGatewayReconciler_TerminatingGatewayHoldsItsSlot pins the cap half: the
// terminating Gateway's pods keep running until owner GC collects them, so the
// slot is not free until the object is gone.
func TestGatewayReconciler_TerminatingGatewayHoldsItsSlot(t *testing.T) {
	t.Parallel()

	fakeClient := quotaFixtures(t, new(int32(2)))
	holdInTermination(t, fakeClient, "tenant", "gw-old")

	refused := quotaAcceptedCondition(t, fakeClient, "tenant", "gw-new")
	assert.Equal(t, metav1.ConditionFalse, refused.Status,
		"a sibling still being deleted holds its slot")
	assert.Equal(t, reasonDataPlaneQuotaExceeded, refused.Reason)
}

// TestGatewayReconciler_TerminatingGatewayHoldsItsTunnel pins the tunnel half.
// The terminating holder's connector stays registered on the tunnel until its
// plane is collected, so a claimant from another namespace accepted in that
// window would be served on the holder's tunnel alongside it.
func TestGatewayReconciler_TerminatingGatewayHoldsItsTunnel(t *testing.T) {
	t.Parallel()

	const tenantTunnel = "550e8400-e29b-41d4-a716-446655440000"

	fakeClient := quotaFixtures(t, nil)
	ctx := context.Background()

	// Leave gw-old as the tunnel's only holder in its namespace, so the verdict
	// below turns on whether it still counts.
	for _, name := range []string{"gw-mid", "gw-new"} {
		require.NoError(t, fakeClient.Delete(ctx, &gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant"},
		}))
	}

	holdInTermination(t, fakeClient, "tenant", "gw-old")

	var token corev1.Secret
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "gw-solo-token", Namespace: "neighbour"}, &token))

	token.Data["tunnel-token"] = []byte(infraTunnelTokenFor(t, tenantTunnel))
	require.NoError(t, fakeClient.Update(ctx, &token))

	refused := quotaAcceptedCondition(t, fakeClient, "neighbour", "gw-solo")
	assert.Equal(t, metav1.ConditionFalse, refused.Status,
		"another namespace must not take a tunnel whose holder is still being deleted")
	assert.Contains(t, refused.Message, tenantTunnel)
}
