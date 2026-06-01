package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

func TestResolveRouteParentBinding_GatewayParentManaged(t *testing.T) {
	t.Parallel()

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			Listeners: []gatewayv1.Listener{
				{
					Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, gc, gw)
	validator := routebinding.NewValidator(cli)

	gwKind := gatewayv1.Kind(kindGateway)
	gwNS := gatewayv1.Namespace("infra")
	ref := gatewayv1.ParentReference{Kind: &gwKind, Name: "gw", Namespace: &gwNS}
	routeInfo := &routebinding.RouteInfo{
		Name: "r", Namespace: "team-a",
		Hostnames: nil, Kind: routebinding.KindHTTPRoute,
	}

	binding, err := resolveRouteParentBinding(context.Background(), cli, validator, testListenerSetController, ref, "team-a", routeInfo, nil)
	require.NoError(t, err)
	assert.True(t, binding.ManagedByThisController)
	assert.True(t, binding.Result.Accepted)
}

func TestResolveRouteParentBinding_GatewayManagedByForeignController(t *testing.T) {
	t.Parallel()

	foreignClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: "other-class"},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: "other.example.com/other"},
	}
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(foreignClass.Name)},
	}

	cli := buildGatewayFakeClient(t, foreignClass, gw)
	validator := routebinding.NewValidator(cli)

	gwKind := gatewayv1.Kind(kindGateway)
	gwNS := gatewayv1.Namespace("infra")
	ref := gatewayv1.ParentReference{Kind: &gwKind, Name: "gw", Namespace: &gwNS}
	routeInfo := &routebinding.RouteInfo{Name: "r", Namespace: "team-a", Kind: routebinding.KindHTTPRoute}

	binding, err := resolveRouteParentBinding(context.Background(), cli, validator, testListenerSetController, ref, "team-a", routeInfo, nil)
	require.NoError(t, err)
	assert.False(t, binding.ManagedByThisController, "ref to a Gateway whose class is owned by a foreign controller must not register as ours")
}

func TestResolveRouteParentBinding_ListenerSetNotAllowedByGateway(t *testing.T) {
	t.Parallel()

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(gc.Name)},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, gc, gw, ls)
	validator := routebinding.NewValidator(cli)

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	ref := gatewayv1.ParentReference{Kind: &lsKind, Name: "ls", Namespace: &lsNS}
	routeInfo := &routebinding.RouteInfo{Name: "r", Namespace: "team-a", Kind: routebinding.KindHTTPRoute}

	binding, err := resolveRouteParentBinding(context.Background(), cli, validator, testListenerSetController, ref, "team-a", routeInfo, nil)
	require.NoError(t, err)
	assert.True(t, binding.ManagedByThisController, "Gateway IS managed, even though it rejects the ListenerSet — so the route still belongs to us")
	assert.False(t, binding.Result.Accepted)
	assert.Equal(t, gatewayv1.RouteReasonNoMatchingParent, binding.Result.Reason)
}

func TestResolveRouteParentBinding_ListenerSetParentGatewayMissing(t *testing.T) {
	t.Parallel()

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "missing-gw"},
		},
	}

	cli := buildGatewayFakeClient(t, ls)
	validator := routebinding.NewValidator(cli)

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	ref := gatewayv1.ParentReference{Kind: &lsKind, Name: "ls", Namespace: &lsNS}
	routeInfo := &routebinding.RouteInfo{Name: "r", Namespace: "team-a", Kind: routebinding.KindHTTPRoute}

	binding, err := resolveRouteParentBinding(context.Background(), cli, validator, testListenerSetController, ref, "team-a", routeInfo, nil)
	require.NoError(t, err)
	assert.False(t, binding.ManagedByThisController, "missing parent Gateway leaves the ref unresolvable")
}

func TestResolveRouteParentBinding_CrossNamespaceListenerSet(t *testing.T) {
	t.Parallel()

	gc := managedGatewayClass()
	fromAll := gatewayv1.NamespacesFromAll
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "platform"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &fromAll},
			},
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}
	platformNS := gatewayv1.Namespace("platform")
	// Give the ListenerSet entry its own hostname so it doesn't share the
	// (port=80, hostname="") tuple with the Gateway listener and trigger a
	// HostnameConflict. The Gateway listener has no hostname (defaults to
	// wildcard "*") and the LS entry has a specific host — distinct slots.
	tenantHost := gatewayv1.Hostname("tenant.example.com")
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "tenant"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw", Namespace: &platformNS},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "extra", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
					Hostname: &tenantHost,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, gc, gw, ls)
	validator := routebinding.NewValidator(cli)

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("tenant")
	ref := gatewayv1.ParentReference{Kind: &lsKind, Name: "ls", Namespace: &lsNS}
	routeInfo := &routebinding.RouteInfo{Name: "r", Namespace: "team-a", Kind: routebinding.KindHTTPRoute}

	binding, err := resolveRouteParentBinding(context.Background(), cli, validator, testListenerSetController, ref, "team-a", routeInfo, nil)
	require.NoError(t, err)
	assert.True(t, binding.ManagedByThisController)
	assert.True(t, binding.Result.Accepted, "route attaches via the cross-namespace ListenerSet's entry")
}

// TestResolveRouteParentBinding_ConflictedListenerSetEntryRejected guards
// the spec contract: a route attached to a ListenerSet entry that conflicts
// with a higher-precedence Gateway listener (same port + same hostname)
// MUST NOT be accepted. The merged-view conflict mark drives the rejection.
func TestResolveRouteParentBinding_ConflictedListenerSetEntryRejected(t *testing.T) {
	t.Parallel()

	gc := managedGatewayClass()
	fromSame := gatewayv1.NamespacesFromSame
	conflictHost := gatewayv1.Hostname("conflict.example.com")
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(gc.Name),
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &fromSame},
			},
			Listeners: []gatewayv1.Listener{
				{Name: "gw-l1", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &conflictHost},
			},
		},
	}
	// Same (port, hostname) → conflict; Gateway wins, LS entry is marked.
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "conflicted", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
					Hostname: &conflictHost,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, gc, gw, ls)
	validator := routebinding.NewValidator(cli)

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	ref := gatewayv1.ParentReference{Kind: &lsKind, Name: "ls", Namespace: &lsNS}
	routeInfo := &routebinding.RouteInfo{Name: "r", Namespace: "team-a", Kind: routebinding.KindHTTPRoute}

	binding, err := resolveRouteParentBinding(context.Background(), cli, validator, testListenerSetController, ref, "team-a", routeInfo, nil)
	require.NoError(t, err)
	assert.True(t, binding.ManagedByThisController)
	assert.False(t, binding.Result.Accepted, "binding to a conflicted LS entry must be rejected")
	assert.Equal(t, gatewayv1.RouteReasonNoMatchingParent, binding.Result.Reason)
}

// TestParentRefSelectsManagedGateway_ForeignGroupRejected guards the status
// writer's group filter: a parentRef with Kind=ListenerSet but a Group other
// than gateway.networking.k8s.io must NOT register as a managed parent even
// when a same-named Gateway-API ListenerSet exists in the cluster. Without
// the group filter, the route's status.parents would gain an unintended
// entry attributed to a third-party CRD ref.
func TestParentRefSelectsManagedGateway_ForeignGroupRejected(t *testing.T) {
	t.Parallel()

	gc := managedGatewayClass()
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: gatewayv1.ObjectName(gc.Name)},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
		},
	}

	cli := buildGatewayFakeClient(t, gc, gw, ls)
	classNames := map[string]bool{gc.Name: true}

	foreignGroup := gatewayv1.Group("other.example.com")
	kind := gatewayv1.Kind(kindListenerSet)
	ns := gatewayv1.Namespace("infra")
	ref := gatewayv1.ParentReference{Group: &foreignGroup, Kind: &kind, Name: "ls", Namespace: &ns}

	assert.False(t,
		parentRefSelectsManagedGateway(context.Background(), cli, ref, "team-a", classNames),
		"foreign-group ListenerSet ref must not register as a managed parent",
	)
}

func namespacesFromAllPtr() *gatewayv1.FromNamespaces {
	v := gatewayv1.NamespacesFromAll
	return &v
}
