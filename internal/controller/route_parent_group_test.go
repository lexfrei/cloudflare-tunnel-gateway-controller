package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// foreignParentGroups are parentRef groups that name something other than a
// Gateway API resource. An explicit "" is the core group (ParentReference.Group
// defaults to gateway.networking.k8s.io only when omitted), so it is foreign too.
var foreignParentGroups = []gatewayv1.Group{"example.com", ""}

// TestSyncAllRoutes_ForeignGroupParentBindsNothing pins that a parentRef
// naming a Gateway of another API group does not select the Gateway API
// Gateway of the same name. ParentReference.Group is the referent's group, so
// {group: example.com, kind: Gateway, name: shared-gw} names some other
// resource: the route must not bind, must not be accepted, and must not reach
// any data-plane partition, for either route kind.
func TestSyncAllRoutes_ForeignGroupParentBindsNothing(t *testing.T) {
	t.Parallel()

	for _, group := range foreignParentGroups {
		t.Run(fmt.Sprintf("group %q", group), func(t *testing.T) {
			t.Parallel()
			assertForeignGroupParentBindsNothing(t, group)
		})
	}
}

func assertForeignGroupParentBindsNothing(t *testing.T, foreignGroup gatewayv1.Group) {
	t.Helper()

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

	boundRoute := partitionSyncRoute("bound-http", "shared-gw", "bound-http.example.com")

	objects := append(partitionSyncObjects(t, "99999999-9999-4999-8999-999999999999", false),
		runtime.Object(httpRoute), runtime.Object(grpcRoute), runtime.Object(boundRoute))

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
	assert.NotEmpty(t, result.HTTPRouteBindings["default/bound-http"].bindingResults, "an omitted group still binds")

	accepted := IsRouteAcceptedByGateway(context.Background(), syncer.Client, routebinding.NewValidator(syncer.Client),
		skipTestControllerName, HTTPRouteWrapper{httpRoute}, nil)
	assert.False(t, accepted, "the route mappers must not treat the route as accepted either")
}

// TestGatewayAttachedRoutes_ForeignGroupParentNotCounted pins that a listener's
// attachedRoutes counts only routes that bind: a parentRef of another API
// group names another resource, so a route carrying only that ref is not
// attached to the Gateway API Gateway of the same name.
func TestGatewayAttachedRoutes_ForeignGroupParentNotCounted(t *testing.T) {
	t.Parallel()

	for _, group := range foreignParentGroups {
		t.Run(fmt.Sprintf("group %q", group), func(t *testing.T) {
			t.Parallel()
			assertForeignGroupParentNotCounted(t, group)
		})
	}
}

func assertForeignGroupParentNotCounted(t *testing.T, foreignGroup gatewayv1.Group) {
	t.Helper()

	gatewayKind := gatewayv1.Kind(kindGateway)

	fromAll := gatewayv1.NamespacesFromAll
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: "cloudflare-tunnel",
			Listeners: []gatewayv1.Listener{{
				Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
				AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll}},
			}},
		},
	}

	routeTo := func(name string, ref gatewayv1.ParentReference) *gatewayv1.HTTPRoute {
		return &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{ref}},
			},
		}
	}

	cli := setupGatewayFakeClient(gateway,
		routeTo("bound", gatewayv1.ParentReference{Name: "gw"}),
		routeTo("foreign", gatewayv1.ParentReference{Group: &foreignGroup, Kind: &gatewayKind, Name: "gw"}),
	)

	reconciler := &GatewayReconciler{Client: cli, Scheme: cli.Scheme(), ControllerName: "test-controller"}

	assert.Equal(t, map[gatewayv1.SectionName]int32{"http": 1}, reconciler.countAttachedRoutes(context.Background(), gateway),
		"only the route that binds is attached")
}

// TestRouteToGateways_ForeignGroupParentEnqueuesNothing pins the route mapper
// to the same group rule as binding: a parentRef of another API group names
// another resource, so it enqueues no Gateway.
func TestRouteToGateways_ForeignGroupParentEnqueuesNothing(t *testing.T) {
	t.Parallel()

	gatewayKind := gatewayv1.Kind(kindGateway)
	tests := []struct {
		name    string
		group   *gatewayv1.Group
		enqueue bool
	}{
		{name: "foreign group", group: new(gatewayv1.Group("example.com"))},
		{name: "explicit empty group", group: new(gatewayv1.Group(""))},
		{name: "omitted group", enqueue: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			requests := routeToGatewaysFor(t, gatewayv1.ParentReference{Group: tt.group, Kind: &gatewayKind, Name: "gw"})
			assert.Equal(t, tt.enqueue, len(requests) > 0)
		})
	}
}

func routeToGatewaysFor(t *testing.T, ref gatewayv1.ParentReference) []reconcile.Request {
	t.Helper()

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "default"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{ref}},
		},
	}

	cli := setupGatewayFakeClient(
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: "test-controller"},
		},
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cloudflare-tunnel", Listeners: httpListener()},
		},
		route,
	)

	reconciler := &GatewayReconciler{Client: cli, ControllerName: "test-controller"}

	return reconciler.routeToGateways(context.Background(), route)
}
