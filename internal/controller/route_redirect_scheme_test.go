package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// redirectRoute builds an HTTPRoute bound to gw/infra with a single
// rule-level RequestRedirect filter whose Scheme is left nil unless overridden.
func redirectRoute(scheme *string) *gatewayv1.HTTPRoute {
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
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Filters: []gatewayv1.HTTPRouteFilter{
						{
							Type: gatewayv1.HTTPRouteFilterRequestRedirect,
							RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
								Scheme:     scheme,
								StatusCode: new(302),
							},
						},
					},
				},
			},
		},
	}
}

// gatewayWithListenerProtocol builds a Gateway named gw/infra with one
// all-namespaces listener of the given protocol and no hostname, isolating the
// protocol → redirect-scheme inference from hostname inheritance.
func gatewayWithListenerProtocol(protocol gatewayv1.ProtocolType, port gatewayv1.PortNumber) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name: "l", Port: port, Protocol: protocol,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}
}

func redirectFilterScheme(route *gatewayv1.HTTPRoute) *string {
	return route.Spec.Rules[0].Filters[0].RequestRedirect.Scheme
}

// TestWithDefaultRedirectScheme_HTTPListenerDefaultsToHTTP pins the spec rule
// "when empty, the scheme of the request is used": a redirect with no explicit
// scheme, bound to an HTTP listener, must default to http — not the hardcoded
// https the proxy falls back to without this inference.
func TestWithDefaultRedirectScheme_HTTPListenerDefaultsToHTTP(t *testing.T) {
	t.Parallel()

	gw := gatewayWithListenerProtocol(gatewayv1.HTTPProtocolType, 80)
	route := redirectRoute(nil)
	cli := buildGatewayFakeClient(t, gw)

	out := withDefaultRedirectScheme(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	got := redirectFilterScheme(out[0])
	require.NotNil(t, got, "redirect scheme must be defaulted from the HTTP listener")
	assert.Equal(t, "http", *got)

	assert.Nil(t, redirectFilterScheme(route), "input route must not be mutated")
}

// TestWithDefaultRedirectScheme_HTTPSListenerDefaultsToHTTPS proves the same
// inference yields https when the bound listener terminates HTTPS.
func TestWithDefaultRedirectScheme_HTTPSListenerDefaultsToHTTPS(t *testing.T) {
	t.Parallel()

	gw := gatewayWithListenerProtocol(gatewayv1.HTTPSProtocolType, 443)
	route := redirectRoute(nil)
	cli := buildGatewayFakeClient(t, gw)

	out := withDefaultRedirectScheme(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	got := redirectFilterScheme(out[0])
	require.NotNil(t, got)
	assert.Equal(t, "https", *got)
}

// TestWithDefaultRedirectScheme_TLSListenerLeavesSchemeNil proves a TLS
// listener infers no redirect scheme: an HTTPRoute never binds to a TLS
// listener (the binding validator's default kinds for TLS exclude HTTPRoute,
// and TLS is terminated at the Cloudflare edge), so the listener never appears
// in the accepted set and the scheme is left nil for the proxy fallback.
func TestWithDefaultRedirectScheme_TLSListenerLeavesSchemeNil(t *testing.T) {
	t.Parallel()

	gw := gatewayWithListenerProtocol(gatewayv1.TLSProtocolType, 443)
	route := redirectRoute(nil)
	cli := buildGatewayFakeClient(t, gw)

	out := withDefaultRedirectScheme(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Nil(t, redirectFilterScheme(out[0]), "a TLS listener does not accept an HTTPRoute → no inferred scheme")
}

// TestWithDefaultRedirectScheme_TCPListenerLeavesSchemeNil proves a non-L7
// (TCP) listener implies no HTTP redirect scheme: the route does not bind to a
// TCP listener as an HTTPRoute, so nothing is inferred and the scheme stays nil
// for the proxy fallback. This also pins the exhaustive switch's TCP/UDP arm.
func TestWithDefaultRedirectScheme_TCPListenerLeavesSchemeNil(t *testing.T) {
	t.Parallel()

	gw := gatewayWithListenerProtocol(gatewayv1.TCPProtocolType, 9000)
	route := redirectRoute(nil)
	cli := buildGatewayFakeClient(t, gw)

	out := withDefaultRedirectScheme(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Nil(t, redirectFilterScheme(out[0]), "a TCP listener implies no L7 redirect scheme")
}

// TestWithDefaultRedirectScheme_ExplicitSchemePreserved proves an operator's
// explicit scheme is never overwritten by the inferred default.
func TestWithDefaultRedirectScheme_ExplicitSchemePreserved(t *testing.T) {
	t.Parallel()

	gw := gatewayWithListenerProtocol(gatewayv1.HTTPSProtocolType, 443)
	route := redirectRoute(new("http"))
	cli := buildGatewayFakeClient(t, gw)

	out := withDefaultRedirectScheme(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	got := redirectFilterScheme(out[0])
	require.NotNil(t, got)
	assert.Equal(t, "http", *got, "explicit scheme must win over the listener-inferred default")
}

// TestWithDefaultRedirectScheme_NoRedirectFilterPassthrough proves a route with
// no redirect filter is returned unchanged (same pointer, no deep copy).
func TestWithDefaultRedirectScheme_NoRedirectFilterPassthrough(t *testing.T) {
	t.Parallel()

	gw := gatewayWithListenerProtocol(gatewayv1.HTTPProtocolType, 80)
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			Rules: []gatewayv1.HTTPRouteRule{{}},
		},
	}
	cli := buildGatewayFakeClient(t, gw)

	out := withDefaultRedirectScheme(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Same(t, route, out[0], "routes without a redirect filter must pass through untouched")
}

// TestWithDefaultRedirectScheme_NoParentLeavesSchemeNil proves that when no
// managed parent listener resolves, the scheme is left nil so the proxy's own
// fallback applies rather than guessing.
func TestWithDefaultRedirectScheme_NoParentLeavesSchemeNil(t *testing.T) {
	t.Parallel()

	route := redirectRoute(nil)
	cli := buildGatewayFakeClient(t) // no Gateway seeded

	out := withDefaultRedirectScheme(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Nil(t, redirectFilterScheme(out[0]), "no resolvable parent → no inferred scheme")
}

// listenerSetRedirectRoute builds a redirect HTTPRoute bound to ls/infra.
func listenerSetRedirectRoute() *gatewayv1.HTTPRoute {
	lsKind := gatewayv1.Kind(kindListenerSet)
	lsNS := gatewayv1.Namespace("infra")
	route := redirectRoute(nil)
	route.Spec.ParentRefs = []gatewayv1.ParentReference{
		{Kind: &lsKind, Name: "ls", Namespace: &lsNS},
	}

	return route
}

// TestWithDefaultRedirectScheme_ListenerSetHTTPDefaultsToHTTP proves the
// ListenerSet parentRef branch infers the scheme from the entry's protocol,
// exactly like the Gateway branch. Without the parent Gateway seeded,
// nonConflictedSections is a best-effort no-op, so this isolates the
// section→protocol mapping for the accepted ListenerSet entry.
func TestWithDefaultRedirectScheme_ListenerSetHTTPDefaultsToHTTP(t *testing.T) {
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

	cli := buildGatewayFakeClient(t, ls)

	out := withDefaultRedirectScheme(context.Background(), cli, []*gatewayv1.HTTPRoute{listenerSetRedirectRoute()}, nil)
	require.Len(t, out, 1)
	got := redirectFilterScheme(out[0])
	require.NotNil(t, got, "scheme must be inferred from the accepted ListenerSet entry")
	assert.Equal(t, "http", *got)
}

// TestWithDefaultRedirectScheme_ListenerSetConflictedEntryLeavesSchemeNil
// pins the conflict-drop fix: when the route's matched ListenerSet entry is
// conflicted in the merged view (a higher-precedence entry on the same port
// claimed the same hostname), that entry is not programmed, so its protocol
// must NOT seed the redirect scheme. Without the dropConflictedSections step
// the scheme would wrongly be inferred from the conflicted entry.
func TestWithDefaultRedirectScheme_ListenerSetConflictedEntryLeavesSchemeNil(t *testing.T) {
	t.Parallel()

	host := gatewayv1.Hostname("dup.example.com")
	fromAll := gatewayv1.NamespacesFromAll
	// The Gateway listener claims the same (port, hostname) as the ListenerSet
	// entry. Gateway listeners always precede ListenerSet entries in the merged
	// view, so the Gateway wins and the ListenerSet entry is annotated with a
	// HostnameConflict — deterministically, with no dependence on resource
	// name ordering.
	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			// Opt the ListenerSet into the merged view; without this the
			// Gateway's default (From=None) excludes it and the conflict the
			// test relies on is never annotated.
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

	cli := buildGatewayFakeClient(t, gw, ls)

	out := withDefaultRedirectScheme(context.Background(), cli, []*gatewayv1.HTTPRoute{listenerSetRedirectRoute()}, nil)
	require.Len(t, out, 1)
	assert.Nil(t, redirectFilterScheme(out[0]),
		"a conflicted ListenerSet entry is not programmed → its protocol must not seed the scheme")
}

// backendRefRedirectRoute builds an HTTPRoute bound to gw/infra whose redirect
// filter lives at the backendRef level (HTTPBackendRef.Filters), not the rule
// level. The Gateway API permits RequestRedirect there (Support: Extended) and
// the proxy executes it, so scheme defaulting must reach it too.
func backendRefRedirectRoute() *gatewayv1.HTTPRoute {
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
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{
							BackendRef: gatewayv1.BackendRef{
								BackendObjectReference: gatewayv1.BackendObjectReference{
									Name: "svc", Port: new(gatewayv1.PortNumber(80)),
								},
							},
							Filters: []gatewayv1.HTTPRouteFilter{
								{
									Type: gatewayv1.HTTPRouteFilterRequestRedirect,
									RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
										StatusCode: new(302),
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func backendRefRedirectFilterScheme(route *gatewayv1.HTTPRoute) *string {
	return route.Spec.Rules[0].BackendRefs[0].Filters[0].RequestRedirect.Scheme
}

// TestWithDefaultRedirectScheme_BackendRefHTTPListenerDefaultsToHTTP proves the
// defaulting reaches a backendRef-level RequestRedirect filter, not just
// rule-level ones: a scheme-less backendRef redirect on an HTTP listener must
// default to http.
func TestWithDefaultRedirectScheme_BackendRefHTTPListenerDefaultsToHTTP(t *testing.T) {
	t.Parallel()

	gw := gatewayWithListenerProtocol(gatewayv1.HTTPProtocolType, 80)
	route := backendRefRedirectRoute()
	cli := buildGatewayFakeClient(t, gw)

	out := withDefaultRedirectScheme(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	got := backendRefRedirectFilterScheme(out[0])
	require.NotNil(t, got, "backendRef redirect scheme must be defaulted from the HTTP listener")
	assert.Equal(t, "http", *got)

	assert.Nil(t, backendRefRedirectFilterScheme(route), "input route must not be mutated")
}

// TestWithDefaultRedirectScheme_BackendRefHTTPSListenerDefaultsToHTTPS is the
// HTTPS counterpart of the backendRef-level case.
func TestWithDefaultRedirectScheme_BackendRefHTTPSListenerDefaultsToHTTPS(t *testing.T) {
	t.Parallel()

	gw := gatewayWithListenerProtocol(gatewayv1.HTTPSProtocolType, 443)
	route := backendRefRedirectRoute()
	cli := buildGatewayFakeClient(t, gw)

	out := withDefaultRedirectScheme(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	got := backendRefRedirectFilterScheme(out[0])
	require.NotNil(t, got)
	assert.Equal(t, "https", *got)
}

// TestWithDefaultRedirectScheme_HTTPSWinsTie pins the documented tie-break: a
// route accepted by both an HTTP and an HTTPS listener on the same Gateway
// defaults its scheme to the more secure https.
func TestWithDefaultRedirectScheme_HTTPSWinsTie(t *testing.T) {
	t.Parallel()

	gw := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
				{
					Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}
	route := redirectRoute(nil)
	cli := buildGatewayFakeClient(t, gw)

	out := withDefaultRedirectScheme(context.Background(), cli, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	got := redirectFilterScheme(out[0])
	require.NotNil(t, got)
	assert.Equal(t, "https", *got, "https must win when the route is bound to both an HTTP and an HTTPS listener")
}
