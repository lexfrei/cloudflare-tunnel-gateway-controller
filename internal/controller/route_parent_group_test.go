package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// TestSyncAllRoutes_ForeignGroupParentBindsNothing pins that a parentRef
// naming a Gateway of another API group does not select the Gateway API
// Gateway of the same name. ParentReference.Group is the referent's group, so
// {group: example.com, kind: Gateway, name: shared-gw} names some other
// resource: the route must not bind, must not be accepted, and must not reach
// any data-plane partition, for either route kind.
func TestSyncAllRoutes_ForeignGroupParentBindsNothing(t *testing.T) {
	t.Parallel()

	foreignGroup := gatewayv1.Group("example.com")
	gatewayKind := gatewayv1.Kind(kindGateway)
	foreignRef := gatewayv1.ParentReference{Group: &foreignGroup, Kind: &gatewayKind, Name: "shared-gw"}

	httpRoute := partitionSyncRoute("foreign-http", "shared-gw", "foreign-http.example.com")
	httpRoute.Spec.ParentRefs = []gatewayv1.ParentReference{foreignRef}

	grpcRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "foreign-grpc", Namespace: "default"},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{foreignRef}},
			Hostnames:       []gatewayv1.Hostname{"foreign-grpc.example.com"},
		},
	}

	objects := append(partitionSyncObjects(t, "99999999-9999-4999-8999-999999999999", false),
		runtime.Object(httpRoute), runtime.Object(grpcRoute))

	syncer := partitionSyncSyncerFor(t, newRecordingTunnelAPI(t), objects)

	_, result, err := syncer.SyncAllRoutes(context.Background())
	require.NoError(t, err)
	require.NotNil(t, result)

	for _, partition := range result.Partitions {
		for i := range partition.HTTPRoutes {
			assert.NotEqual(t, "foreign-http", partition.HTTPRoutes[i].Name, "partition %s", partition.Key)
		}

		for i := range partition.GRPCRoutes {
			assert.NotEqual(t, "foreign-grpc", partition.GRPCRoutes[i].Name, "partition %s", partition.Key)
		}
	}

	assert.Empty(t, result.HTTPRouteBindings["default/foreign-http"].bindingResults, "a foreign-group ref binds nothing")
	assert.Empty(t, result.GRPCRouteBindings["default/foreign-grpc"].bindingResults, "a foreign-group ref binds nothing")

	accepted := IsRouteAcceptedByGateway(context.Background(), syncer.Client, routebinding.NewValidator(syncer.Client),
		skipTestControllerName, HTTPRouteWrapper{httpRoute}, nil)
	assert.False(t, accepted, "the route mappers must not treat the route as accepted either")
}
