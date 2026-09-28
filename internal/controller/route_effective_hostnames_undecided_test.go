package controller

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// failingGetClient serves every object normally except reads of the same type
// as failOn, which fail with a transient error.
func failingGetClient(t *testing.T, failOn client.Object, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				switch failOn.(type) {
				case *gatewayv1.Gateway:
					if _, ok := obj.(*gatewayv1.Gateway); ok {
						return errSimulatedCacheMiss
					}
				case *gatewayv1.ListenerSet:
					if _, ok := obj.(*gatewayv1.ListenerSet); ok {
						return errSimulatedCacheMiss
					}
				}

				return cli.Get(ctx, key, obj, opts...)
			},
		}).Build()
}

// invalidSelectorGateway is a Gateway of ours whose only listener admits
// routes by a label selector that does not parse, so binding validation
// against a route whose hostnames intersect the listener's returns an error
// instead of a verdict.
func invalidSelectorGateway(name string, hostname *gatewayv1.Hostname) *gatewayv1.Gateway {
	gateway := gatewayUnderClass(name, "our-class", hostname)
	gateway.Spec.Listeners[0].AllowedRoutes = &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{
			From: new(gatewayv1.NamespacesFromSelector),
			Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "team", Operator: "NotAnOperator"},
			}},
		},
	}

	return gateway
}

type undecidedParentCase struct {
	name  string
	cli   func(t *testing.T) client.Client
	route func(hostnames ...gatewayv1.Hostname) *gatewayv1.HTTPRoute
}

// undecidedParentCases are the ways a parentRef can fail to say whether it
// lends the route a hostname: the Gateway or the ListenerSet cannot be read,
// binding validation against either errors, the ListenerSet's parent Gateway
// cannot be read, or that Gateway's allowedListeners cannot be evaluated.
func undecidedParentCases() []undecidedParentCase {
	ourHost := gatewayv1.Hostname("ours.example.com")
	entryHost := gatewayv1.Hostname("ls.example.com")

	toGateway := func(hostnames ...gatewayv1.Hostname) *gatewayv1.HTTPRoute {
		route := httpRouteTo(hostnames...)
		route.Spec.ParentRefs = parentRefsToGateways("ours")

		return route
	}

	toListenerSet := func(hostnames ...gatewayv1.Hostname) *gatewayv1.HTTPRoute {
		route := routeToListenerSet("ls")
		route.Spec.Hostnames = hostnames

		return route
	}

	listenerSetObjects := func() []client.Object {
		return []client.Object{
			gatewayClassFor("our-class", skipTestControllerName),
			allowingListenerSets(gatewayUnderClass("ours", "our-class", nil)),
			listenerSetUnder("ls", "ours", &entryHost),
		}
	}

	return []undecidedParentCase{
		{
			name: "gateway read fails",
			cli: func(t *testing.T) client.Client {
				t.Helper()

				return failingGetClient(t, &gatewayv1.Gateway{},
					gatewayClassFor("our-class", skipTestControllerName),
					gatewayUnderClass("ours", "our-class", &ourHost))
			},
			route: toGateway,
		},
		{
			name: "binding validation errors",
			cli: func(t *testing.T) client.Client {
				t.Helper()

				return buildGatewayFakeClient(t,
					gatewayClassFor("our-class", skipTestControllerName),
					invalidSelectorGateway("ours", nil))
			},
			route: toGateway,
		},
		{
			name: "listenerset read fails",
			cli: func(t *testing.T) client.Client {
				t.Helper()

				return failingGetClient(t, &gatewayv1.ListenerSet{}, listenerSetObjects()...)
			},
			route: toListenerSet,
		},
		{
			name: "listenerset binding validation errors",
			cli: func(t *testing.T) client.Client {
				t.Helper()

				listenerSet := listenerSetUnder("ls", "ours", nil)
				listenerSet.Spec.Listeners[0].AllowedRoutes = invalidSelectorGateway("unused", nil).Spec.Listeners[0].AllowedRoutes

				return buildGatewayFakeClient(t,
					gatewayClassFor("our-class", skipTestControllerName),
					allowingListenerSets(gatewayUnderClass("ours", "our-class", nil)),
					listenerSet)
			},
			route: toListenerSet,
		},
		{
			name: "listenerset acceptance evaluation errors",
			cli: func(t *testing.T) client.Client {
				t.Helper()

				gateway := gatewayUnderClass("ours", "our-class", nil)
				gateway.Spec.AllowedListeners = &gatewayv1.AllowedListeners{
					Namespaces: &gatewayv1.ListenerNamespaces{
						From: new(gatewayv1.NamespacesFromSelector),
						Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
							{Key: "team", Operator: "NotAnOperator"},
						}},
					},
				}

				return buildGatewayFakeClient(t,
					gatewayClassFor("our-class", skipTestControllerName),
					gateway,
					listenerSetUnder("ls", "ours", &entryHost))
			},
			route: toListenerSet,
		},
		{
			name: "listenerset parent gateway read fails",
			cli: func(t *testing.T) client.Client {
				t.Helper()

				return failingGetClient(t, &gatewayv1.Gateway{}, listenerSetObjects()...)
			},
			route: toListenerSet,
		},
	}
}

// TestWithEffectiveHostnames_UndecidedParentLeavesRouteOut pins what happens
// when the only parent of a route cannot be evaluated. Returning the route as
// written would serve a hostname-less route as a catch-all answering every
// Host, and a route with hostnames on names no listener of ours was shown to
// cover. Neither is known to be right, so the route is left out.
func TestWithEffectiveHostnames_UndecidedParentLeavesRouteOut(t *testing.T) {
	t.Parallel()

	for _, tt := range undecidedParentCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for _, hostnames := range [][]gatewayv1.Hostname{nil, {"app.example.com"}} {
				out, diags := withEffectiveHostnames(context.Background(), tt.cli(t), skipTestControllerName,
					[]*gatewayv1.HTTPRoute{tt.route(hostnames...)}, nil)
				assert.Empty(t, out, "a route whose only parent cannot be evaluated must not be served (hostnames %v)", hostnames)
				assertParentNotEvaluated(t, diags)
			}
		})
	}
}

// TestWithEffectiveHostnamesGRPC_UndecidedParentLeavesRouteOut is the GRPCRoute
// twin for the Gateway read.
func TestWithEffectiveHostnamesGRPC_UndecidedParentLeavesRouteOut(t *testing.T) {
	t.Parallel()

	ourHost := gatewayv1.Hostname("ours.example.com")
	cli := failingGetClient(t, &gatewayv1.Gateway{},
		gatewayClassFor("our-class", skipTestControllerName),
		gatewayUnderClass("ours", "our-class", &ourHost))

	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToGateways("ours")},
		},
	}

	out, diags := withEffectiveHostnamesGRPC(context.Background(), cli, skipTestControllerName, []*gatewayv1.GRPCRoute{route}, nil)
	assert.Empty(t, out, "a gRPC route whose only parent cannot be evaluated must not be served")
	assertParentNotEvaluated(t, diags)
}

// assertParentNotEvaluated checks that the one route left out is reported on
// its own status, so the drop is visible beyond the controller log.
func assertParentNotEvaluated(t *testing.T, diags []proxy.RouteDiagnostic) {
	t.Helper()

	require.Len(t, diags, 1, "the route left out must be reported once")
	assert.Equal(t, "team", diags[0].Namespace)
	assert.Equal(t, "r", diags[0].Name)
	assert.Equal(t, proxy.DiagnosticProxyConfigPush, diags[0].Target)
	assert.Equal(t, routeReasonParentNotEvaluated, diags[0].Reason)
	assert.NotEmpty(t, diags[0].Message)
}

// TestWithEffectiveHostnames_UndecidedParentBesideAnAcceptingOne pins that one
// undecided parent does not take the route down when another parent lends it a
// hostname: the route serves what is known to be accepted.
func TestWithEffectiveHostnames_UndecidedParentBesideAnAcceptingOne(t *testing.T) {
	t.Parallel()

	ourHost := gatewayv1.Hostname("ours.example.com")
	otherHost := gatewayv1.Hostname("other.example.com")

	cli := buildGatewayFakeClient(t,
		gatewayClassFor("our-class", skipTestControllerName),
		gatewayUnderClass("ours", "our-class", &ourHost),
		invalidSelectorGateway("broken", &otherHost),
	)

	route := httpRouteTo()
	route.Spec.ParentRefs = parentRefsToGateways("ours", "broken")

	out, diags := withEffectiveHostnames(context.Background(), cli, skipTestControllerName, []*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, []gatewayv1.Hostname{ourHost}, out[0].Spec.Hostnames)
	assert.Empty(t, diags, "a route that is still served is not reported as left out")
}

// TestWithEffectiveHostnames_StableWhenGatewayMissing is the Gateway twin of
// TestWithEffectiveHostnames_StableWhenParentMissing: a Gateway that does not
// exist is an answer, not a failure to get one, so the route is returned as
// written rather than left out.
func TestWithEffectiveHostnames_StableWhenGatewayMissing(t *testing.T) {
	t.Parallel()

	route := httpRouteTo()
	route.Spec.ParentRefs = parentRefsToGateways("missing")

	out, _ := withEffectiveHostnames(context.Background(), buildGatewayFakeClient(t), skipTestControllerName,
		[]*gatewayv1.HTTPRoute{route}, nil)
	require.Len(t, out, 1)
	assert.Empty(t, out[0].Spec.Hostnames, "a missing Gateway must not synthesise hostnames")
}

// TestSyncPartition_ReportsRouteLeftOutOverItsParent pins that the diagnostic
// for a route left out reaches the sync's result for both route kinds, which
// is what carries it to the route's status.
func TestSyncPartition_ReportsRouteLeftOutOverItsParent(t *testing.T) {
	t.Parallel()

	replica := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(replica.Close)

	ourHost := gatewayv1.Hostname("ours.example.com")
	cli := failingGetClient(t, &gatewayv1.Gateway{}, gatewayUnderClass("ours", "our-class", &ourHost))
	syncer := NewProxySyncer("cluster.local", "", "", cli, slog.Default())

	httpRoute := httpRouteTo()
	httpRoute.Spec.ParentRefs = parentRefsToGateways("ours")
	httpRoute.Name = "http-r"

	grpcRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "grpc-r", Namespace: "team"},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToGateways("ours")},
		},
	}

	diags, err := syncer.SyncPartition(context.Background(), 0, sharedPartitionKey, "",
		[]string{replica.URL + "/config"}, []*gatewayv1.HTTPRoute{httpRoute}, []*gatewayv1.GRPCRoute{grpcRoute}, nil, nil)
	require.NoError(t, err)

	var leftOut []string

	for _, diag := range diags {
		if diag.Reason == routeReasonParentNotEvaluated {
			leftOut = append(leftOut, diag.Name)
		}
	}

	assert.ElementsMatch(t, []string{"http-r", "grpc-r"}, leftOut)
}
