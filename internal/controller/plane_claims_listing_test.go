package controller

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

// gatewayListCounter wraps a client and counts cluster-wide Gateway lists.
func gatewayListCounter(base client.WithWatch) (client.WithWatch, *atomic.Int32) {
	var lists atomic.Int32

	counting := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*gatewayv1.GatewayList); ok {
				lists.Add(1)
			}

			return cl.List(ctx, list, opts...)
		},
	})

	return counting, &lists
}

// TestPlaneRefusals_ListManagedGatewaysOnce pins that deciding a Gateway's
// tunnel claim and its data-plane quota reads the managed Gateways once, not
// once per rule: both claim sets are derived from the same listing.
func TestPlaneRefusals_ListManagedGatewaysOnce(t *testing.T) {
	t.Parallel()

	capacity := new(int32(2))

	t.Run("gateway reconciler", func(t *testing.T) {
		t.Parallel()

		counting, lists := gatewayListCounter(quotaFixtures(t, capacity))
		reconciler := &GatewayReconciler{
			Client:         counting,
			Scheme:         counting.Scheme(),
			ControllerName: "test-controller",
			ConfigResolver: config.NewResolver(counting, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
		}

		var gateway gatewayv1.Gateway
		require.NoError(t, counting.Get(context.Background(), client.ObjectKey{Namespace: "tenant", Name: "gw-old"}, &gateway))

		_, handled, err := reconciler.refuseDedicatedPlane(context.Background(), &gateway, true)
		require.NoError(t, err)
		require.False(t, handled, "the oldest Gateway is within the cap and owns its tunnel")
		assert.Equal(t, int32(1), lists.Load())
	})

	t.Run("infra reconciler", func(t *testing.T) {
		t.Parallel()

		counting, lists := gatewayListCounter(quotaFixtures(t, capacity))
		reconciler := &GatewayInfraReconciler{
			Client:         counting,
			ControllerName: "test-controller",
			ConfigResolver: config.NewResolver(counting, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
		}

		var gateway gatewayv1.Gateway
		require.NoError(t, counting.Get(context.Background(), client.ObjectKey{Namespace: "tenant", Name: "gw-new"}, &gateway))

		refused, err := reconciler.dedicatedPlaneRefused(context.Background(), &gateway)
		require.NoError(t, err)
		require.True(t, refused, "the newest of three Gateways is over a cap of two")
		assert.Equal(t, int32(1), lists.Load())
	})

	t.Run("route syncer", func(t *testing.T) {
		t.Parallel()

		counting, lists := gatewayListCounter(quotaFixtures(t, capacity))
		syncer := &RouteSyncer{
			Client:         counting,
			ControllerName: "test-controller",
			ConfigResolver: config.NewResolver(counting, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
		}

		ctx := context.Background()

		infra, err := syncer.resolveInfraGateways(ctx)
		require.NoError(t, err)

		resolved := &config.ResolvedConfig{
			TunnelID:                  "12345678-1234-1234-1234-123456789abc",
			MaxDataPlanesPerNamespace: capacity,
		}
		require.NoError(t, syncer.applyPlaneRefusals(ctx, infra, resolved))

		_, refused := infra.quotaRefusal("tenant/gw-new")
		require.True(t, refused, "the newest of three Gateways is over a cap of two")
		assert.Equal(t, int32(1), lists.Load(),
			"the refusals must reuse the listing the partitioner was built from")
	})
}
