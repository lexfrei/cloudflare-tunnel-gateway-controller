package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

const isolationGatewayKey = "infra/gw"

var errListListenerSets = errors.New("listing ListenerSets failed")

func isolationListener(name string, hostname gatewayv1.Hostname, protocol gatewayv1.ProtocolType) gatewayv1.Listener {
	listener := gatewayv1.Listener{
		Name: gatewayv1.SectionName(name), Port: 80, Protocol: protocol,
		AllowedRoutes: &gatewayv1.AllowedRoutes{
			Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
		},
	}
	if hostname != "" {
		listener.Hostname = &hostname
	}

	return listener
}

// isolationGateway carries the listeners of the upstream
// GatewayHTTPListenerIsolation conformance test, named after their hostnames.
func isolationGateway() *gatewayv1.Gateway {
	return allowingListenerSets(&gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				isolationListener("empty", "", gatewayv1.HTTPProtocolType),
				isolationListener("wildcard", "*.example.com", gatewayv1.HTTPProtocolType),
				isolationListener("wildcard-foo", "*.foo.example.com", gatewayv1.HTTPProtocolType),
				isolationListener("abc-foo", "abc.foo.example.com", gatewayv1.HTTPProtocolType),
			},
		},
	})
}

func isolationClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	objs = append(objs, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "team"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
	})

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// isolationRoute is a route named after its path prefix, attached to the
// given listener section of the isolation Gateway ("" attaches to every
// listener).
func isolationRoute(path string, section gatewayv1.SectionName, hostnames ...gatewayv1.Hostname) *gatewayv1.HTTPRoute {
	ref := parentRefsToGateways("gw")[0]
	if section != "" {
		ref.SectionName = &section
	}

	port := gatewayv1.PortNumber(80)

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: path, Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{ref}},
			Hostnames:       hostnames,
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{
					Type: new(gatewayv1.PathMatchPathPrefix), Value: new("/" + path),
				}}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: &port},
				}}},
			}},
		},
	}
}

// answeringRoute returns the route (by path) the proxy answers host+path
// with, or "" when it answers 404.
func answeringRoute(t *testing.T, cfg *proxy.Config, host, path string) string {
	t.Helper()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(cfg))

	result := router.Route(&http.Request{
		Method: http.MethodGet, Host: host, URL: &url.URL{Path: "/" + path}, Header: http.Header{},
	})
	if result == nil {
		return ""
	}

	return strings.TrimPrefix(result.Rule.Matches[0].Path.Value, "/")
}

func assertListenerOwnership(t *testing.T, cfg *proxy.Config, owners map[string]string, paths []string) {
	t.Helper()

	for host, owner := range owners {
		for _, path := range paths {
			want := ""
			if path == owner {
				want = path
			}

			assert.Equal(t, want, answeringRoute(t, cfg, host, path), "request %s/%s", host, path)
		}
	}
}

// TestProxySyncer_ListenerIsolation drives the config the syncer builds for
// the upstream GatewayHTTPListenerIsolation fixture through the proxy router,
// by way of the JSON the config API carries.
func TestProxySyncer_ListenerIsolation(t *testing.T) {
	t.Parallel()

	syncer := NewProxySyncer("cluster.local", "token", "", isolationClient(t, isolationGateway()), nil)

	built := syncer.buildProxyConfig(context.Background(), []*gatewayv1.HTTPRoute{
		isolationRoute("empty", "empty"),
		isolationRoute("wild", "wildcard"),
		isolationRoute("foo", "wildcard-foo"),
		isolationRoute("abc", "abc-foo"),
	}, nil, nil, nil, clientCertParents{})

	wire, err := json.Marshal(built)
	require.NoError(t, err)

	cfg, err := proxy.ParseConfig(wire)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"", "*.example.com", "*.foo.example.com", "abc.foo.example.com"},
		cfg.ListenerHostnames[isolationGatewayKey])

	assertListenerOwnership(t, cfg, map[string]string{
		"bar.com":             "empty",
		"bar.example.com":     "wild",
		"bar.foo.example.com": "foo",
		"abc.foo.example.com": "abc",
	}, []string{"empty", "wild", "foo", "abc"})
}

// TestProxySyncer_ListenerIsolationWithHostnameIntersection drives the
// variant where every route also declares hostnames.
func TestProxySyncer_ListenerIsolationWithHostnameIntersection(t *testing.T) {
	t.Parallel()

	all := []gatewayv1.Hostname{"bar.com", "*.example.com", "*.foo.example.com", "abc.foo.example.com"}
	syncer := NewProxySyncer("cluster.local", "token", "", isolationClient(t, isolationGateway()), nil)

	cfg := syncer.buildProxyConfig(context.Background(), []*gatewayv1.HTTPRoute{
		isolationRoute("empty", "empty", all...),
		isolationRoute("wild", "wildcard", all...),
		isolationRoute("foo", "wildcard-foo", all...),
		isolationRoute("abc", "abc-foo", all...),
	}, nil, nil, nil, clientCertParents{})

	assertListenerOwnership(t, cfg, map[string]string{
		"bar.com":             "empty",
		"bar.example.com":     "wild",
		"bar.foo.example.com": "foo",
		"abc.foo.example.com": "abc",
	}, []string{"empty", "wild", "foo", "abc"})
}

// TestProxySyncer_ListenerIsolationRouteOnEveryListener pins that a route
// attached without a sectionName answers every host, since every owning
// listener is one it is attached through.
func TestProxySyncer_ListenerIsolationRouteOnEveryListener(t *testing.T) {
	t.Parallel()

	syncer := NewProxySyncer("cluster.local", "token", "", isolationClient(t, isolationGateway()), nil)

	cfg := syncer.buildProxyConfig(context.Background(),
		[]*gatewayv1.HTTPRoute{isolationRoute("any", "")}, nil, nil, nil, clientCertParents{})

	for _, host := range []string{"bar.com", "bar.example.com", "bar.foo.example.com", "abc.foo.example.com"} {
		assert.Equal(t, "any", answeringRoute(t, cfg, host, "any"), host)
	}
}

// TestProxySyncer_ListenerIsolationCountsListenerSetEntries pins that a
// ListenerSet entry takes part in isolation like a listener of its parent
// Gateway: it owns its hostname against the Gateway's wildcard.
func TestProxySyncer_ListenerIsolationCountsListenerSetEntries(t *testing.T) {
	t.Parallel()

	entry := gatewayv1.Hostname("bar.example.com")
	lsRoute := isolationRoute("entry", "")
	lsKind := gatewayv1.Kind(kindListenerSet)
	lsRoute.Spec.ParentRefs[0].Kind = &lsKind
	lsRoute.Spec.ParentRefs[0].Name = "ls"

	syncer := NewProxySyncer("cluster.local", "token", "",
		isolationClient(t, isolationGateway(), listenerSetUnder("ls", "gw", &entry)), nil)

	cfg := syncer.buildProxyConfig(context.Background(), []*gatewayv1.HTTPRoute{
		isolationRoute("wild", "wildcard"),
		lsRoute,
	}, nil, nil, nil, clientCertParents{})

	assert.Contains(t, cfg.ListenerHostnames[isolationGatewayKey], "bar.example.com")
	assert.Empty(t, answeringRoute(t, cfg, "bar.example.com", "wild"), "the entry owns its hostname")
	assert.Equal(t, "entry", answeringRoute(t, cfg, "bar.example.com", "entry"))
	assert.Equal(t, "wild", answeringRoute(t, cfg, "baz.example.com", "wild"))
}

// TestProxySyncer_ListenerIsolationGRPC pins the same isolation for a
// GRPCRoute: a route on the wildcard listener does not answer a host the
// exact listener owns.
func TestProxySyncer_ListenerIsolationGRPC(t *testing.T) {
	t.Parallel()

	section := gatewayv1.SectionName("wildcard")
	ref := parentRefsToGateways("gw")[0]
	ref.SectionName = &section
	port := gatewayv1.PortNumber(80)

	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "team"},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{ref}},
			Rules: []gatewayv1.GRPCRouteRule{{
				BackendRefs: []gatewayv1.GRPCBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: &port},
				}}},
			}},
		},
	}

	syncer := NewProxySyncer("cluster.local", "token", "", isolationClient(t, isolationGateway()), nil)
	cfg := syncer.buildProxyConfig(context.Background(), nil, []*gatewayv1.GRPCRoute{route}, nil, nil, clientCertParents{})

	require.Len(t, cfg.Rules, 1)
	assert.Equal(t, map[string][]string{isolationGatewayKey: {"*.example.com"}}, cfg.Rules[0].Listeners)

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(cfg))

	request := func(host string) *proxy.RouteResult {
		return router.Route(&http.Request{
			Method: http.MethodPost, Host: host, URL: &url.URL{Path: "/svc/Method"},
			Header: http.Header{"Content-Type": {"application/grpc"}},
		})
	}

	assert.NotNil(t, request("bar.example.com"))
	assert.Nil(t, request("bar.foo.example.com"))
}

// TestProxySyncer_ListenerIsolationCountsOnlyThisPlanesGateways pins that a
// data plane isolates a route only by the Gateways it serves: a second
// Gateway the route is attached to, served by another plane, must not lend
// it a host that this plane's Gateway gives to a more specific listener.
func TestProxySyncer_ListenerIsolationCountsOnlyThisPlanesGateways(t *testing.T) {
	t.Parallel()

	other := isolationGateway()
	other.Name = "other"
	other.Spec.Listeners = other.Spec.Listeners[1:2]

	route := isolationRoute("wild", "wildcard")
	route.Spec.ParentRefs = append(route.Spec.ParentRefs, parentRefsToGateways("other")...)

	syncer := NewProxySyncer("cluster.local", "token", "", isolationClient(t, isolationGateway(), other), nil)
	cfg := syncer.buildProxyConfig(context.Background(), []*gatewayv1.HTTPRoute{route}, nil, nil, nil,
		clientCertParents{http: addGatewayKeys(nil, "team/wild", isolationGatewayKey)})

	require.Len(t, cfg.Rules, 1)
	assert.Equal(t, map[string][]string{isolationGatewayKey: {"*.example.com"}}, cfg.Rules[0].Listeners)
	assert.Empty(t, answeringRoute(t, cfg, "abc.foo.example.com", "wild"))
	assert.Equal(t, "wild", answeringRoute(t, cfg, "bar.example.com", "wild"))
}

// TestProxySyncer_ListenerIsolationIgnoresListenersAdmittingNoRoute pins that a
// listener which is not Accepted, and so admits no route, owns no host: a
// route on the wildcard keeps answering the hostname such a listener names.
func TestProxySyncer_ListenerIsolationIgnoresListenersAdmittingNoRoute(t *testing.T) {
	t.Parallel()

	wildcard := isolationListener("wildcard", "*.example.com", gatewayv1.HTTPProtocolType)
	badSelector := isolationListener("foo", "foo.example.com", gatewayv1.HTTPProtocolType)
	badSelector.AllowedRoutes.Namespaces = &gatewayv1.RouteNamespaces{
		From: new(gatewayv1.NamespacesFromSelector),
		Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "team", Operator: "BogusOperator", Values: []string{"x"}},
		}},
	}
	cases := map[string][]gatewayv1.Listener{
		"namespace selector that does not parse": {wildcard, badSelector},
		"unsupported protocol":                   {wildcard, isolationListener("tls", "foo.example.com", gatewayv1.TLSProtocolType)},
		"conflicted": {
			wildcard,
			isolationListener("foo-a", "foo.example.com", gatewayv1.HTTPProtocolType),
			isolationListener("foo-b", "foo.example.com", gatewayv1.HTTPProtocolType),
		},
	}

	for name, listeners := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gateway := isolationGateway()
			gateway.Spec.Listeners = listeners

			syncer := NewProxySyncer("cluster.local", "token", "", isolationClient(t, gateway), nil)
			cfg := syncer.buildProxyConfig(context.Background(),
				[]*gatewayv1.HTTPRoute{isolationRoute("wild", "wildcard")}, nil, nil, nil, clientCertParents{})

			assert.Equal(t, []string{"*.example.com"}, cfg.ListenerHostnames[isolationGatewayKey])
			assert.Equal(t, "wild", answeringRoute(t, cfg, "foo.example.com", "wild"))
		})
	}
}

// TestProgrammedListenerHostnames_UnreadableGatewayIsLeftOut pins the fail
// open: a Gateway that cannot be read gets no listener list, and the proxy
// then does not isolate routes by it.
func TestProgrammedListenerHostnames_UnreadableGatewayIsLeftOut(t *testing.T) {
	t.Parallel()

	cli := isolationClient(t)
	_, ok := programmedListenerHostnames(context.Background(), cli, nil, "infra/missing")
	assert.False(t, ok)

	rules := []proxy.RouteRule{{Listeners: map[string][]string{"infra/missing": {"*.example.com"}}}}
	assert.Empty(t, gatewayListenerHostnames(context.Background(), cli, nil, rules))
}

// TestProgrammedListenerHostnames_UnreadableListenerSetsLeaveGatewayOut pins
// the fail open when the ListenerSets cannot be listed: isolating by the
// Gateway's own listeners alone would hand an entry's hosts to the Gateway's
// wildcard, so the Gateway gets no listener list at all.
func TestProgrammedListenerHostnames_UnreadableListenerSetsLeaveGatewayOut(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(isolationGateway()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*gatewayv1.ListenerSetList); ok {
					return errListListenerSets
				}

				return c.List(ctx, list, opts...)
			},
		}).Build()

	_, ok := programmedListenerHostnames(context.Background(), cli, nil, isolationGatewayKey)
	assert.False(t, ok)
}
