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

	out := withEffectiveHostnames(context.Background(), cli, "", []*gatewayv1.HTTPRoute{route}, nil)
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

	out := withEffectiveHostnamesGRPC(context.Background(), cli, "", []*gatewayv1.GRPCRoute{route}, nil)
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

	out := withEffectiveHostnames(context.Background(), cli, "", []*gatewayv1.HTTPRoute{route}, nil)
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

	out := withEffectiveHostnames(context.Background(), cli, "", []*gatewayv1.HTTPRoute{route}, nil)
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
			out := withEffectiveHostnames(context.Background(), cli, "", []*gatewayv1.HTTPRoute{httpRouteTo(tt.hostnames...)}, nil)
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

	out := withEffectiveHostnames(context.Background(), cli, "", []*gatewayv1.HTTPRoute{route}, nil)
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
	out := withEffectiveHostnamesGRPC(context.Background(), cli, "", []*gatewayv1.GRPCRoute{route}, nil)
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
	out := withEffectiveHostnamesGRPC(context.Background(), cli, "", []*gatewayv1.GRPCRoute{route}, nil)
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
	out := withEffectiveHostnames(context.Background(), cli, "", []*gatewayv1.HTTPRoute{httpRouteTo()}, nil)
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
	out := withEffectiveHostnamesGRPC(context.Background(), cli, "", []*gatewayv1.GRPCRoute{route}, nil)
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
	out := withEffectiveHostnames(context.Background(), cli, "", []*gatewayv1.HTTPRoute{httpRouteTo(declared...)}, nil)
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

	out := withEffectiveHostnames(context.Background(), cli, "", []*gatewayv1.HTTPRoute{route}, nil)
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

	out := withEffectiveHostnames(context.Background(), cli, "", []*gatewayv1.HTTPRoute{route}, nil)
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

	out := withEffectiveHostnames(context.Background(), cli, "", []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Empty(t, out[0].Spec.Hostnames,
		"a conflicted ListenerSet entry is not programmed → its hostname must not be inherited")
}

// foreignControllerName is the controllerName of a GatewayClass belonging to
// some other Gateway API implementation sharing the cluster with us.
const foreignControllerName = "example.com/other-controller"

// gatewayUnderClass builds a Gateway with one listener carrying the given
// hostname (nil for a hostname-less listener), open to routes from every
// namespace.
func gatewayUnderClass(name, className string, hostname *gatewayv1.Hostname) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(className),
			Listeners: []gatewayv1.Listener{
				{
					Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostname,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}
}

// parentRefsToGateways builds the parentRef list selecting the named Gateways
// in the infra namespace.
func parentRefsToGateways(names ...string) []gatewayv1.ParentReference {
	gwKind := gatewayv1.Kind(kindGateway)
	gwNS := gatewayv1.Namespace("infra")

	refs := make([]gatewayv1.ParentReference, 0, len(names))
	for _, name := range names {
		refs = append(refs, gatewayv1.ParentReference{
			Kind: &gwKind, Name: gatewayv1.ObjectName(name), Namespace: &gwNS,
		})
	}

	return refs
}

// TestWithEffectiveHostnames_IgnoresForeignGateway covers a route attached to
// our Gateway AND to one belonging to another implementation -- the ordinary
// shape of a migration, and of any cluster running two Gateway API
// controllers. The route-acceptance pass filters parentRefs by the GatewayClass
// controllerName; this pass re-resolves the same refs, so it must apply the
// same filter. Otherwise the foreign listener's hostname joins the union and
// our data plane answers for a hostname no listener of ours carries.
func TestWithEffectiveHostnames_IgnoresForeignGateway(t *testing.T) {
	t.Parallel()

	ourHost := gatewayv1.Hostname("ours.example.com")
	foreignHost := gatewayv1.Hostname("theirs.example.com")

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToGateways("ours", "theirs")},
		},
	}

	cli := buildGatewayFakeClient(t,
		gatewayClassFor("our-class", skipTestControllerName),
		gatewayClassFor("their-class", foreignControllerName),
		gatewayUnderClass("ours", "our-class", &ourHost),
		gatewayUnderClass("theirs", "their-class", &foreignHost),
	)

	out := withEffectiveHostnames(context.Background(), cli, skipTestControllerName, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{ourHost}, out[0].Spec.Hostnames,
		"only listeners of Gateways this controller manages may contribute hostnames")
}

// TestWithEffectiveHostnames_ForeignCatchAllListenerDoesNotWiden is the sharp
// end of the same gap. A hostname-less listener on a foreign Gateway marks the
// route as a catch-all, and a catch-all rule in our proxy answers EVERY Host it
// receives -- including hostnames belonging to other tenants' routes. The route
// must stay pinned to the hostname of the listener of ours that accepted it.
func TestWithEffectiveHostnames_ForeignCatchAllListenerDoesNotWiden(t *testing.T) {
	t.Parallel()

	ourHost := gatewayv1.Hostname("ours.example.com")

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToGateways("ours", "theirs")},
		},
	}

	cli := buildGatewayFakeClient(t,
		gatewayClassFor("our-class", skipTestControllerName),
		gatewayClassFor("their-class", foreignControllerName),
		gatewayUnderClass("ours", "our-class", &ourHost),
		gatewayUnderClass("theirs", "their-class", nil),
	)

	out := withEffectiveHostnames(context.Background(), cli, skipTestControllerName, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{ourHost}, out[0].Spec.Hostnames,
		"a foreign hostname-less listener must not turn the route into a catch-all on our data plane")
}

// TestWithEffectiveHostnamesGRPC_IgnoresForeignGateway is the GRPCRoute twin:
// both paths run the same parentRef resolution, so both inherit the same gap.
func TestWithEffectiveHostnamesGRPC_IgnoresForeignGateway(t *testing.T) {
	t.Parallel()

	ourHost := gatewayv1.Hostname("ours.example.com")
	foreignHost := gatewayv1.Hostname("theirs.example.com")

	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToGateways("ours", "theirs")},
		},
	}

	cli := buildGatewayFakeClient(t,
		gatewayClassFor("our-class", skipTestControllerName),
		gatewayClassFor("their-class", foreignControllerName),
		gatewayUnderClass("ours", "our-class", &ourHost),
		gatewayUnderClass("theirs", "their-class", &foreignHost),
	)

	out := withEffectiveHostnamesGRPC(context.Background(), cli, skipTestControllerName, []*gatewayv1.GRPCRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{ourHost}, out[0].Spec.Hostnames,
		"only listeners of Gateways this controller manages may contribute hostnames")
}

// TestWithEffectiveHostnames_IgnoresForeignListenerSet covers the other half of
// the parentRef surface. A ListenerSet names no GatewayClass of its own, so
// whether it is ours is a property of its parent Gateway — and a ListenerSet
// attached to another implementation's Gateway describes listeners we do not
// serve. Without the parent-Gateway check its entries would contribute
// hostnames exactly as a foreign Gateway's own listeners did.
func TestWithEffectiveHostnames_IgnoresForeignListenerSet(t *testing.T) {
	t.Parallel()

	ourHost := gatewayv1.Hostname("ours.example.com")
	foreignHost := gatewayv1.Hostname("theirs.example.com")

	listenerSet := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "theirs"},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: &foreignHost,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}

	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	refs := append(parentRefsToGateways("ours"),
		gatewayv1.ParentReference{Kind: &lsKind, Name: "ls", Namespace: &lsNS})

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: refs},
		},
	}

	cli := buildGatewayFakeClient(t,
		gatewayClassFor("our-class", skipTestControllerName),
		gatewayClassFor("their-class", foreignControllerName),
		gatewayUnderClass("ours", "our-class", &ourHost),
		gatewayUnderClass("theirs", "their-class", nil),
		listenerSet,
	)

	out := withEffectiveHostnames(context.Background(), cli, skipTestControllerName, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{ourHost}, out[0].Spec.Hostnames,
		"a ListenerSet under a foreign parent Gateway contributes nothing")
}

// TestWithEffectiveHostnames_OurHostnamelessListenerStaysCatchAll is the
// inverse of the foreign-listener cases, and it guards the feature those
// cases could have broken. A hostname-less listener of OURS means the route
// genuinely does serve every Host through it, so the catch-all sentinel must
// still be emitted and the route must still come back untouched. A class
// filter that folded this case would close the defect by breaking the
// feature, and every other test in this file points the other way.
func TestWithEffectiveHostnames_OurHostnamelessListenerStaysCatchAll(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToGateways("ours")},
		},
	}

	cli := buildGatewayFakeClient(t,
		gatewayClassFor("our-class", skipTestControllerName),
		gatewayUnderClass("ours", "our-class", nil),
	)

	out := withEffectiveHostnames(context.Background(), cli, skipTestControllerName, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Empty(t, out[0].Spec.Hostnames,
		"a hostname-less listener of ours is a real catch-all and must stay one")
	assert.Same(t, route, out[0], "an untouched route is returned as-is, not cloned")
}

// TestWithEffectiveHostnames_OurGatewayStillNarrows is the second inverse: a
// route parented only to a Gateway of ours resolves exactly as it did before
// the class filter existed. It pins that the filter narrowed which parents may
// contribute, not the resolution itself.
func TestWithEffectiveHostnames_OurGatewayStillNarrows(t *testing.T) {
	t.Parallel()

	ourHost := gatewayv1.Hostname("ours.example.com")
	otherHost := gatewayv1.Hostname("elsewhere.example.com")

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToGateways("ours")},
			Hostnames:       []gatewayv1.Hostname{ourHost, otherHost},
		},
	}

	cli := buildGatewayFakeClient(t,
		gatewayClassFor("our-class", skipTestControllerName),
		gatewayUnderClass("ours", "our-class", &ourHost),
	)

	out := withEffectiveHostnames(context.Background(), cli, skipTestControllerName, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{ourHost}, out[0].Spec.Hostnames,
		"a declared hostname our listener does not cover is still dropped")
}

// listenerSetUnder builds a ListenerSet in infra whose parentRef names the
// given Gateway, carrying one all-namespaces listener with that hostname.
func listenerSetUnder(name, parentGateway string, hostname *gatewayv1.Hostname) *gatewayv1.ListenerSet {
	return &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "infra"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(parentGateway)},
			Listeners: []gatewayv1.ListenerEntry{
				{
					Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostname,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}
}

// routeToListenerSet builds a hostname-less route parented only to the named
// ListenerSet in infra.
func routeToListenerSet(name string) *gatewayv1.HTTPRoute {
	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{
					{Kind: &lsKind, Name: gatewayv1.ObjectName(name), Namespace: &lsNS},
				},
			},
		},
	}
}

// TestWithEffectiveHostnames_OurListenerSetStillContributes is the inverse the
// ListenerSet branch was missing. The foreign cases prove a ListenerSet under
// someone else's Gateway contributes nothing; without this one, a filter that
// rejected EVERY ListenerSet — or resolved the parent in the wrong namespace —
// would pass the whole suite, because every other ListenerSet test passes an
// empty controllerName and returns before the parent is ever resolved.
func TestWithEffectiveHostnames_OurListenerSetStillContributes(t *testing.T) {
	t.Parallel()

	entryHost := gatewayv1.Hostname("ls.example.com")

	cli := buildGatewayFakeClient(t,
		gatewayClassFor("our-class", skipTestControllerName),
		gatewayUnderClass("ours", "our-class", nil),
		listenerSetUnder("ls", "ours", &entryHost),
	)

	out := withEffectiveHostnames(context.Background(), cli, skipTestControllerName,
		[]*gatewayv1.HTTPRoute{routeToListenerSet("ls")}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{entryHost}, out[0].Spec.Hostnames,
		"a ListenerSet under a Gateway of ours still lends its entry hostname")
}

// TestWithEffectiveHostnames_ListenerSetWithMissingParentContributesNothing
// pins a deliberate behaviour change rather than a preserved behaviour. Before
// the class filter, a ListenerSet whose parentRef named a Gateway that does not
// exist still contributed its hostnames, because the conflict filter treats an
// unresolvable parent as best-effort and returns the sections unchanged. A
// ListenerSet with no parent is programmed by nobody, and the route-acceptance
// pass already drops such a ref, so the two passes now agree.
func TestWithEffectiveHostnames_ListenerSetWithMissingParentContributesNothing(t *testing.T) {
	t.Parallel()

	entryHost := gatewayv1.Hostname("orphan.example.com")

	cli := buildGatewayFakeClient(t,
		gatewayClassFor("our-class", skipTestControllerName),
		listenerSetUnder("ls", "ghost", &entryHost),
	)

	out := withEffectiveHostnames(context.Background(), cli, skipTestControllerName,
		[]*gatewayv1.HTTPRoute{routeToListenerSet("ls")}, nil)
	require.Len(t, out, 1)
	assert.Empty(t, out[0].Spec.Hostnames,
		"a ListenerSet whose parent Gateway does not exist is nobody's, so it lends nothing")
}

// TestWithEffectiveHostnames_UnreadableGatewayClassLeavesCatchAll pins the
// hazard limitations.md now describes to users, rather than the three links
// that compose it. Each link is covered on its own: the guard answers false on
// a missing GatewayClass, a false answer contributes nothing, and a route that
// inherits nothing is returned as written. Only together do they mean that an
// unreadable class leaves a hostname-less route answering every Host, and that
// sentence is in the docs, so a change to any one link must fail here rather
// than make the page quietly wrong.
//
// This pins current behaviour rather than endorsing it. gatewayIsManaged
// already separates a missing class from an unreadable one by returning
// (bool, error); this guard does not. Issue #820 tracks the fix, and when it
// lands this test should assert the new answer instead of the catch-all.
func TestWithEffectiveHostnames_UnreadableGatewayClassLeavesCatchAll(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToGateways("ours")},
		},
	}

	ourHost := gatewayv1.Hostname("ours.example.com")

	// The Gateway exists and its listener would contribute a hostname; only
	// the GatewayClass it names is absent.
	cli := buildGatewayFakeClient(t, gatewayUnderClass("ours", "vanished-class", &ourHost))

	out := withEffectiveHostnames(context.Background(), cli, skipTestControllerName,
		[]*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Empty(t, out[0].Spec.Hostnames,
		"an unreadable GatewayClass contributes nothing, which leaves a hostname-less route serving every Host")
	assert.Same(t, route, out[0], "the route is returned as written, which is what makes it a catch-all")
}
