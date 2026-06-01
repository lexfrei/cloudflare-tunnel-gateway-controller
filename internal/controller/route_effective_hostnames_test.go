package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestWithEffectiveHostnames_InheritsFromGatewayListener(t *testing.T) {
	t.Parallel()

	gatewayHost := gatewayv1.Hostname("gw-listener.example.com")
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &gatewayHost,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}

	gwKind := gatewayv1.Kind(kindGateway)
	gwNS := gatewayv1.Namespace("infra")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &gwKind, Name: "gw", Namespace: &gwNS},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, gw)

	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{gatewayHost}, out[0].Spec.Hostnames)
}

// TestWithEffectiveHostnamesGRPC_InheritsFromGatewayListener proves a GRPCRoute
// with empty spec.hostnames inherits its parent listener's hostname, exactly
// like the HTTPRoute path. Without it the gRPC rule would carry no hostnames
// and the proxy router would treat it as a catch-all matching every Host —
// answering gRPC for hostnames owned by other routes.
func TestWithEffectiveHostnamesGRPC_InheritsFromGatewayListener(t *testing.T) {
	t.Parallel()

	gatewayHost := gatewayv1.Hostname("grpc-listener.example.com")
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &gatewayHost,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}

	gwKind := gatewayv1.Kind(kindGateway)
	gwNS := gatewayv1.Namespace("infra")
	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &gwKind, Name: "gw", Namespace: &gwNS},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, gw)

	out := withEffectiveHostnamesGRPC(context.Background(), cli, []*gatewayv1.GRPCRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{gatewayHost}, out[0].Spec.Hostnames)
}

func TestWithEffectiveHostnames_InheritsFromListenerSetEntry(t *testing.T) {
	t.Parallel()

	entryHost := gatewayv1.Hostname("ls.example.com")
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "only", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &entryHost,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: "ls", Namespace: &lsNS},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, ls)

	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{entryHost}, out[0].Spec.Hostnames)
}

func TestWithEffectiveHostnames_SectionNameNarrowsListenerSetEntries(t *testing.T) {
	t.Parallel()

	a := gatewayv1.Hostname("a.example.com")
	b := gatewayv1.Hostname("b.example.com")
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "first", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &a,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
				{
					Name: "second", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &b,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	section := gatewayv1.SectionName("second")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: "ls", Namespace: &lsNS, SectionName: &section},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, ls)

	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{b}, out[0].Spec.Hostnames, "only the sectionName-matched entry's hostname should be inherited")
}

func TestWithEffectiveHostnames_NoOpWhenRouteAlreadyDeclaresHostnames(t *testing.T) {
	t.Parallel()

	declared := gatewayv1.Hostname("override.example.com")
	listenerHost := gatewayv1.Hostname("ls.example.com")
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{Name: "only", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &listenerHost},
			},
		},
	}

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: "ls", Namespace: &lsNS},
				},
			},
			Hostnames: []gatewayv1.Hostname{declared},
		},
	}

	cli := buildGatewayFakeClient(t, ls)

	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{declared}, out[0].Spec.Hostnames, "explicit route hostnames must not be overwritten")
}

// TestWithEffectiveHostnames_OnlyInheritsFromAcceptingListeners pins the
// conformance contract that the ListenerSetAllowedRoutesNamespaces test
// caught: a route bound to a multi-listener ListenerSet with no sectionName
// inherits hostnames ONLY from the listeners whose allowedRoutes.namespaces
// actually permits the route. A listener that rejects the route's namespace
// must NOT lend its hostname, or the route would answer on a host it has no
// business serving.
func TestWithEffectiveHostnames_OnlyInheritsFromAcceptingListeners(t *testing.T) {
	t.Parallel()

	allHost := gatewayv1.Hostname("all.example.com")
	sameHost := gatewayv1.Hostname("same.example.com")
	fromSame := gatewayv1.NamespacesFromSame

	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "all", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &allHost,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
				{
					Name: "same", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &sameHost,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: &fromSame},
					},
				},
			},
		},
	}

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	// Route in a DIFFERENT namespace than the ListenerSet: the `all` listener
	// accepts it, the `same` listener rejects it.
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: "ls", Namespace: &lsNS},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, ls)

	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{allHost}, out[0].Spec.Hostnames,
		"route must inherit only the accepting listener's hostname, not the same-namespace-only listener's")
}

func TestWithEffectiveHostnames_StableWhenParentMissing(t *testing.T) {
	t.Parallel()

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: "missing", Namespace: &lsNS},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t)

	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Empty(t, out[0].Spec.Hostnames, "missing parent must not synthesise hostnames")
}

// buildGatewayFakeClient registers the gateway-api v1 scheme and seeds the
// fake client with the given objects.
func buildGatewayFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objs {
		builder = builder.WithObjects(obj)
	}

	return builder.Build()
}

// TestWithEffectiveHostnames_ListenerSetConflictedEntryNotInherited pins the
// hostname side of the shared nonConflictedSections drop: when the route's
// matched ListenerSet entry is conflicted in the merged view (a
// higher-precedence Gateway listener on the same port claims the same
// hostname), that entry is not programmed, so the route must NOT inherit its
// hostname. Without the conflict drop the route would wrongly serve the
// conflicted listener's hostname. The redirect-scheme path has a sibling test;
// both callers of nonConflictedSections need independent coverage.
func TestWithEffectiveHostnames_ListenerSetConflictedEntryNotInherited(t *testing.T) {
	t.Parallel()

	host := gatewayv1.Hostname("dup.example.com")
	fromAll := gatewayv1.NamespacesFromAll
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			AllowedListeners: &gatewayv1.AllowedListeners{
				Namespaces: &gatewayv1.ListenerNamespaces{From: &fromAll},
			},
			Listeners: []gatewayv1.Listener{
				{
					Name: "g", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &host,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "only", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &host,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: "ls", Namespace: &lsNS},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, gw, ls)

	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Empty(t, out[0].Spec.Hostnames,
		"a conflicted ListenerSet entry is not programmed → its hostname must not be inherited")
}
