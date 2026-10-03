package controller

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func redirectFilterPort(route *gatewayv1.HTTPRoute) *gatewayv1.PortNumber {
	return route.Spec.Rules[0].Filters[0].RequestRedirect.Port
}

// TestWithDefaultRedirectScheme_EmptySchemeTakesListenerPort pins the Gateway
// API rule for HTTPRequestRedirectFilter.Port: "If redirect scheme is empty,
// the redirect port MUST be the Gateway Listener port". The port of a scheme's
// well-known listener is left out of Location, which the same godoc asks for,
// and so is a port the Cloudflare edge does not accept for that scheme: no
// client can have reached the listener through it.
func TestWithDefaultRedirectScheme_EmptySchemeTakesListenerPort(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		listeners []gatewayv1.Listener
		wantPort  *gatewayv1.PortNumber
	}{
		{name: "http on 8080", listeners: redirectListeners(gatewayv1.HTTPProtocolType, 8080), wantPort: new(gatewayv1.PortNumber(8080))},
		{name: "https on 8443", listeners: redirectListeners(gatewayv1.HTTPSProtocolType, 8443), wantPort: new(gatewayv1.PortNumber(8443))},
		{name: "http on 80 adds no port", listeners: redirectListeners(gatewayv1.HTTPProtocolType, 80)},
		{name: "port the edge does not serve adds no port", listeners: redirectListeners(gatewayv1.HTTPProtocolType, 9000)},
		{name: "https port the edge serves only for http adds no port", listeners: redirectListeners(gatewayv1.HTTPSProtocolType, 8080)},
		{
			name:      "lowest edge-served port wins",
			listeners: redirectListeners(gatewayv1.HTTPProtocolType, 8880, 8080),
			wantPort:  new(gatewayv1.PortNumber(8080)),
		},
		{
			name:      "edge-served port wins over one the edge does not serve",
			listeners: redirectListeners(gatewayv1.HTTPProtocolType, 2000, 8880),
			wantPort:  new(gatewayv1.PortNumber(8880)),
		},
		{name: "https on 443 adds no port", listeners: redirectListeners(gatewayv1.HTTPSProtocolType, 443)},
		{name: "well-known port wins over another", listeners: redirectListeners(gatewayv1.HTTPProtocolType, 8080, 80)},
		{
			name:      "port of the listener whose scheme wins",
			listeners: append(redirectListeners(gatewayv1.HTTPProtocolType, 80), redirectListeners(gatewayv1.HTTPSProtocolType, 8443)...),
			wantPort:  new(gatewayv1.PortNumber(8443)),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gw := gatewayWithListenerProtocol(gatewayv1.HTTPProtocolType, 80)
			gw.Spec.Listeners = tc.listeners
			route := redirectRoute(nil)

			out := withDefaultRedirectScheme(context.Background(), buildGatewayFakeClient(t, gw), "",
				[]*gatewayv1.HTTPRoute{route}, nil)
			require.Len(t, out, 1)
			assert.Equal(t, tc.wantPort, redirectFilterPort(out[0]))
			assert.Nil(t, redirectFilterPort(route), "input route must not be mutated")
		})
	}
}

// TestWithDefaultRedirectScheme_ExplicitSchemeOrPortKeepsPort pins the cases
// the listener port must not touch: an explicit scheme implies its own
// well-known port, and an explicit port stays as written.
func TestWithDefaultRedirectScheme_ExplicitSchemeOrPortKeepsPort(t *testing.T) {
	t.Parallel()

	gw := gatewayWithListenerProtocol(gatewayv1.HTTPProtocolType, 8080)

	explicitScheme := redirectRoute(new(redirectSchemeHTTPS))
	explicitPort := redirectRoute(nil)
	explicitPort.Spec.Rules[0].Filters[0].RequestRedirect.Port = new(gatewayv1.PortNumber(9000))

	out := withDefaultRedirectScheme(context.Background(), buildGatewayFakeClient(t, gw), "",
		[]*gatewayv1.HTTPRoute{explicitScheme, explicitPort}, nil)
	require.Len(t, out, 2)
	assert.Nil(t, redirectFilterPort(out[0]))
	assert.Equal(t, new(gatewayv1.PortNumber(9000)), redirectFilterPort(out[1]))
}

func redirectListeners(protocol gatewayv1.ProtocolType, ports ...gatewayv1.PortNumber) []gatewayv1.Listener {
	listeners := make([]gatewayv1.Listener, 0, len(ports))

	for _, port := range ports {
		listener := gatewayWithListenerProtocol(protocol, port).Spec.Listeners[0]
		listener.Name = gatewayv1.SectionName(string(protocol) + "-" + strconv.Itoa(int(port)))
		listeners = append(listeners, listener)
	}

	return listeners
}

// TestWithDefaultRedirectScheme_ListenerSetEntryPort pins that a route
// attached through a ListenerSet entry takes the entry's port.
func TestWithDefaultRedirectScheme_ListenerSetEntryPort(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		port gatewayv1.PortNumber
		want *gatewayv1.PortNumber
	}{
		{port: 8080, want: new(gatewayv1.PortNumber(8080))},
		{port: 80},
	} {
		t.Run(strconv.Itoa(int(tc.port)), func(t *testing.T) {
			t.Parallel()

			entryHost := gatewayv1.Hostname("ls.example.com")
			ls := &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
				Spec: gatewayv1.ListenerSetSpec{
					ParentRef: gatewayv1.ParentGatewayReference{Name: "gw"},
					Listeners: []gatewayv1.ListenerEntry{{
						Name: "only", Port: tc.port, Protocol: gatewayv1.HTTPProtocolType, Hostname: &entryHost,
						AllowedRoutes: &gatewayv1.AllowedRoutes{
							Namespaces: &gatewayv1.RouteNamespaces{From: namespacesFromAllPtr()},
						},
					}},
				},
			}

			out := withDefaultRedirectScheme(context.Background(), buildGatewayFakeClient(t, ls), "",
				[]*gatewayv1.HTTPRoute{listenerSetRedirectRoute()}, nil)
			require.Len(t, out, 1)
			assert.Equal(t, tc.want, redirectFilterPort(out[0]))
		})
	}
}

// TestWithDefaultRedirectScheme_BackendRefFilterTakesListenerPort pins that
// the listener port reaches a backendRef-level redirect too.
func TestWithDefaultRedirectScheme_BackendRefFilterTakesListenerPort(t *testing.T) {
	t.Parallel()

	gw := gatewayWithListenerProtocol(gatewayv1.HTTPProtocolType, 8080)

	out := withDefaultRedirectScheme(context.Background(), buildGatewayFakeClient(t, gw), "",
		[]*gatewayv1.HTTPRoute{backendRefRedirectRoute()}, nil)
	require.Len(t, out, 1)
	assert.Equal(t, new(gatewayv1.PortNumber(8080)),
		out[0].Spec.Rules[0].BackendRefs[0].Filters[0].RequestRedirect.Port)
}
