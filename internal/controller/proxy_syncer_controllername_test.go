package controller

// The class filter on parentRef resolution is only as good as the wiring that
// carries this syncer's controllerName to it. Every other test of those passes
// calls them directly and supplies the name itself, so a refactor that dropped
// the argument between the field and a call site would leave them green. These
// go through NewProxySyncer, and the fixture is built so that both call sites
// in buildProxyConfig — HTTP and gRPC hostnames, each also deciding the
// listeners a rule is isolated by — give a different answer when the name does
// not arrive.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

const (
	wiringOurHost     = "ours.example.com"
	wiringForeignHost = "theirs.example.com"
)

// wiringGateway builds a Gateway in infra under the named class, with one
// all-namespaces listener carrying the given hostname and protocol.
func wiringGateway(name, className, hostname string, protocol gatewayv1.ProtocolType, port gatewayv1.PortNumber) *gatewayv1.Gateway {
	host := gatewayv1.Hostname(hostname)

	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(className),
			Listeners: []gatewayv1.Listener{
				{
					Name: "l", Port: port, Protocol: protocol, Hostname: &host,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
					},
				},
			},
		},
	}
}

// wiringRoute is hostname-less, so it inherits whatever listeners it binds to.
// One foreign listener has a foreign hostname, which the route's hostnames
// show, and the other has ours on another port, which only the listeners the
// rule is isolated by show.
func wiringRoute() *gatewayv1.HTTPRoute {
	port := gatewayv1.PortNumber(80)

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToGateways("ours", "theirs", "theirs-tls")},
			Rules: []gatewayv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: &port},
							Weight:                 new(int32(1)),
						}},
					},
				},
			},
		},
	}
}

// wiringGRPCRoute is the gRPC twin, covering the third call site.
func wiringGRPCRoute() *gatewayv1.GRPCRoute {
	port := gatewayv1.PortNumber(80)

	return &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "team"},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToGateways("ours", "theirs", "theirs-tls")},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: &port},
							Weight:                 new(int32(1)),
						}},
					},
				},
			},
		},
	}
}

// wiringClient seeds our Gateway and a foreign one, differing in hostname AND
// in listener protocol.
func wiringClient(t *testing.T) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		gatewayClassFor("our-class", skipTestControllerName),
		gatewayClassFor("their-class", foreignControllerName),
		wiringGateway("ours", "our-class", wiringOurHost, gatewayv1.HTTPProtocolType, 80),
		wiringGateway("theirs", "their-class", wiringForeignHost, gatewayv1.HTTPProtocolType, 80),
		// A foreign listener carrying OUR hostname leaves the hostnames
		// unchanged, so only the rule's listeners tell it apart.
		wiringGateway("theirs-tls", "their-class", wiringOurHost, gatewayv1.HTTPSProtocolType, 443),
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "team"},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		},
	).Build()
}

var (
	wiringOurListeners   = []proxy.Listener{{Hostname: wiringOurHost, Port: 80}}
	wiringOursOnly       = map[string][]proxy.Listener{"infra/ours": wiringOurListeners}
	wiringEveryListeners = map[string][]proxy.Listener{
		"infra/ours":       wiringOurListeners,
		"infra/theirs":     {{Hostname: wiringForeignHost, Port: 80}},
		"infra/theirs-tls": {{Hostname: wiringOurHost, Port: 443}},
	}
)

// TestProxySyncer_ControllerNameReachesEveryPass builds the syncer the way the
// manager does and reads the config it produces, so the field, both call
// sites and the filter are covered as one path.
func TestProxySyncer_ControllerNameReachesEveryPass(t *testing.T) {
	t.Parallel()

	syncer := NewProxySyncer("cluster.local", "token", skipTestControllerName, wiringClient(t), nil)

	cfg := syncer.buildProxyConfig(context.Background(),
		[]*gatewayv1.HTTPRoute{wiringRoute()}, []*gatewayv1.GRPCRoute{wiringGRPCRoute()}, nil, nil, clientCertParents{})

	require.NotNil(t, cfg)
	require.Len(t, cfg.Rules, 2, "one HTTP rule and one gRPC rule")

	assert.Equal(t, []string{wiringOurHost}, cfg.Rules[0].Hostnames,
		"the hostname pass must receive the syncer's controllerName")
	assert.Equal(t, wiringOursOnly, cfg.Rules[0].Listeners,
		"no foreign listener may isolate the rule, the one on our hostname included")
	assert.Equal(t, []string{wiringOurHost}, cfg.Rules[1].Hostnames,
		"and so must the gRPC hostname pass")
	assert.Equal(t, wiringOursOnly, cfg.Rules[1].Listeners)
}

// TestProxySyncer_EmptyControllerNameAcceptsAnyGateway pins the other half of
// the wiring contract. An empty name is the documented test convention, and it
// must still mean "accept any Gateway" once the value travels through the
// syncer rather than being handed to each pass directly.
func TestProxySyncer_EmptyControllerNameAcceptsAnyGateway(t *testing.T) {
	t.Parallel()

	syncer := NewProxySyncer("cluster.local", "token", "", wiringClient(t), nil)

	cfg := syncer.buildProxyConfig(context.Background(),
		[]*gatewayv1.HTTPRoute{wiringRoute()}, []*gatewayv1.GRPCRoute{wiringGRPCRoute()}, nil, nil, clientCertParents{})

	require.NotNil(t, cfg)
	require.Len(t, cfg.Rules, 2)

	assert.ElementsMatch(t, []string{wiringOurHost, wiringForeignHost}, cfg.Rules[0].Hostnames,
		"an empty controllerName accepts any Gateway, which is what the existing tests rely on")
	assert.Equal(t, wiringEveryListeners, cfg.Rules[0].Listeners)
	assert.ElementsMatch(t, []string{wiringOurHost, wiringForeignHost}, cfg.Rules[1].Hostnames)
	assert.Equal(t, wiringEveryListeners, cfg.Rules[1].Listeners)
}
