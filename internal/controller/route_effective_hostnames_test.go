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

// intersectionListenersGateway mirrors the conformance
// httproute-hostname-intersection Gateway: an exact listener plus two wildcard
// listeners, all accepting routes from any namespace.
func intersectionListenersGateway(t *testing.T) *gatewayv1.Gateway {
	t.Helper()

	specific := gatewayv1.Hostname("very.specific.com")
	wildcard := gatewayv1.Hostname("*.wildcard.io")
	another := gatewayv1.Hostname("*.anotherwildcard.io")

	listener := func(name string, host *gatewayv1.Hostname) gatewayv1.Listener {
		return gatewayv1.Listener{
			Name: gatewayv1.SectionName(name), Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: host,
			AllowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
			},
		}
	}

	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				listener("listener-1", &specific),
				listener("listener-2", &wildcard),
				listener("listener-3", &another),
			},
		},
	}
}

func httpRouteTo(hostnames ...gatewayv1.Hostname) *gatewayv1.HTTPRoute {
	gwKind := gatewayv1.Kind(kindGateway)
	gwNS := gatewayv1.Namespace("infra")

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &gwKind, Name: "gw", Namespace: &gwNS},
				},
			},
			Hostnames: hostnames,
		},
	}
}

// TestWithEffectiveHostnames_NarrowsToListenerIntersection is the HTTPRoute
// side of issue #587: a route that declares hostnames must serve ONLY the
// intersection of those hostnames with the hostnames of the listeners it binds
// to, not its full declared set. Each case mirrors a conformance
// HTTPRouteHostnameIntersection route.
func TestWithEffectiveHostnames_NarrowsToListenerIntersection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		hostnames []gatewayv1.Hostname
		expected  []gatewayv1.Hostname
	}{
		{
			name:      "exact host under exact listener drops non-matching declared hosts",
			hostnames: []gatewayv1.Hostname{"non.matching.com", "*.nonmatchingwildcard.io", "very.specific.com"},
			expected:  []gatewayv1.Hostname{"very.specific.com"},
		},
		{
			name:      "specific hosts under wildcard listener keep subdomains, drop apex and non-matching",
			hostnames: []gatewayv1.Hostname{"non.matching.com", "wildcard.io", "foo.wildcard.io", "bar.wildcard.io", "foo.bar.wildcard.io"},
			expected:  []gatewayv1.Hostname{"foo.wildcard.io", "bar.wildcard.io", "foo.bar.wildcard.io"},
		},
		{
			name:      "wildcard route host over exact listener yields the listener exact host",
			hostnames: []gatewayv1.Hostname{"non.matching.com", "*.specific.com"},
			expected:  []gatewayv1.Hostname{"very.specific.com"},
		},
		{
			name:      "wildcard route host over equal wildcard listener yields the wildcard",
			hostnames: []gatewayv1.Hostname{"*.anotherwildcard.io"},
			expected:  []gatewayv1.Hostname{"*.anotherwildcard.io"},
		},
		{
			name:      "nested wildcard under a broader wildcard listener is kept",
			hostnames: []gatewayv1.Hostname{"non.matching.com", "*.sub.wildcard.io"},
			expected:  []gatewayv1.Hostname{"*.sub.wildcard.io"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cli := buildGatewayFakeClient(t, intersectionListenersGateway(t))
			out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{httpRouteTo(tt.hostnames...)}, nil)
			require.Len(t, out, 1)
			assert.Equal(t, tt.expected, out[0].Spec.Hostnames)
		})
	}
}

// TestWithEffectiveHostnames_MultiListenerUnion proves a route bound to several
// listeners serves the UNION of its per-listener intersections: hosts that
// intersect distinct listeners are all kept, and a host intersecting no
// listener is dropped.
func TestWithEffectiveHostnames_MultiListenerUnion(t *testing.T) {
	t.Parallel()

	cli := buildGatewayFakeClient(t, intersectionListenersGateway(t))
	route := httpRouteTo("very.specific.com", "foo.wildcard.io", "bar.anotherwildcard.io", "no.intersection.com")

	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.ElementsMatch(t,
		[]gatewayv1.Hostname{"very.specific.com", "foo.wildcard.io", "bar.anotherwildcard.io"},
		out[0].Spec.Hostnames,
		"route must serve the union of per-listener intersections and drop the non-intersecting host")
}

// TestWithEffectiveHostnamesGRPC_NarrowsToListenerIntersection is the GRPCRoute
// twin of TestWithEffectiveHostnames_NarrowsToListenerIntersection: the same
// intersection narrowing applies to declared gRPC route hostnames.
func TestWithEffectiveHostnamesGRPC_NarrowsToListenerIntersection(t *testing.T) {
	t.Parallel()

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
			Hostnames: []gatewayv1.Hostname{"non.matching.com", "wildcard.io", "foo.wildcard.io", "bar.wildcard.io"},
		},
	}

	cli := buildGatewayFakeClient(t, intersectionListenersGateway(t))
	out := withEffectiveHostnamesGRPC(context.Background(), cli, []*gatewayv1.GRPCRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{"foo.wildcard.io", "bar.wildcard.io"}, out[0].Spec.Hostnames)
}

// TestWithEffectiveHostnamesGRPC_MultiListenerUnion is the GRPCRoute twin of
// TestWithEffectiveHostnames_MultiListenerUnion: hosts intersecting distinct
// listeners are all kept, a host intersecting none is dropped.
func TestWithEffectiveHostnamesGRPC_MultiListenerUnion(t *testing.T) {
	t.Parallel()

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
			Hostnames: []gatewayv1.Hostname{
				"very.specific.com", "foo.wildcard.io", "bar.anotherwildcard.io", "no.intersection.com",
			},
		},
	}

	cli := buildGatewayFakeClient(t, intersectionListenersGateway(t))
	out := withEffectiveHostnamesGRPC(context.Background(), cli, []*gatewayv1.GRPCRoute{route}, nil)
	require.Len(t, out, 1)
	assert.ElementsMatch(t,
		[]gatewayv1.Hostname{"very.specific.com", "foo.wildcard.io", "bar.anotherwildcard.io"},
		out[0].Spec.Hostnames,
		"a gRPC route must serve the union of per-listener intersections and drop the non-intersecting host")
}

// TestWithEffectiveHostnames_MixedListenersKeepCatchAll pins the mixed-parent
// case: a hostname-less route accepted by BOTH a hostname-pinned listener and a
// hostname-less (catch-all) listener must stay a catch-all -- the union of
// per-listener scopes includes "all hostnames" via the catch-all listener, so
// narrowing to the pinned hostname would 404 hosts the catch-all listener is
// obliged to serve.
func TestWithEffectiveHostnames_MixedListenersKeepCatchAll(t *testing.T) {
	t.Parallel()

	pinned := gatewayv1.Hostname("a.example.com")
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name: "pinned", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &pinned,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
				{
					Name: "catch-all", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}

	cli := buildGatewayFakeClient(t, gw)
	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{httpRouteTo()}, nil)
	require.Len(t, out, 1)
	assert.Empty(t, out[0].Spec.Hostnames,
		"a hostname-less route accepted by a catch-all listener must stay a catch-all")
}

// TestWithEffectiveHostnamesGRPC_MixedListenersKeepCatchAll is the GRPCRoute
// twin of TestWithEffectiveHostnames_MixedListenersKeepCatchAll.
func TestWithEffectiveHostnamesGRPC_MixedListenersKeepCatchAll(t *testing.T) {
	t.Parallel()

	pinned := gatewayv1.Hostname("a.example.com")
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name: "pinned", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &pinned,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
				{
					Name: "catch-all", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
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
	assert.Empty(t, out[0].Spec.Hostnames,
		"a hostname-less gRPC route accepted by a catch-all listener must stay a catch-all")
}

// TestWithEffectiveHostnames_UnspecifiedListenerHostnameKeepsRouteHostnames
// pins the conformance "intersects with an unspecified hostname listener" case:
// a route with declared hostnames bound to a listener that has NO hostname
// keeps exactly its declared hostnames (the listener is a catch-all, so nothing
// narrows them).
func TestWithEffectiveHostnames_UnspecifiedListenerHostnameKeepsRouteHostnames(t *testing.T) {
	t.Parallel()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name: "listener-1", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}

	declared := []gatewayv1.Hostname{"first.com", "sub.first.com", "second.com"}
	cli := buildGatewayFakeClient(t, gw)
	out := withEffectiveHostnames(context.Background(), cli, []*gatewayv1.HTTPRoute{httpRouteTo(declared...)}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, declared, out[0].Spec.Hostnames, "a hostname-less listener must not narrow the route's declared hostnames")
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
