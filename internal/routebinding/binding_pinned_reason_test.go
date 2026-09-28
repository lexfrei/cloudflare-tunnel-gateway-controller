package routebinding

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// pinnedReasonCase is a parentRef pinned by sectionName or port to a listener,
// and the reason the spec gives when the route is not accepted there.
type pinnedReasonCase struct {
	name        string
	sectionName *gatewayv1.SectionName
	port        *gatewayv1.PortNumber
	routeNS     string
	routeKind   gatewayv1.Kind
	hostnames   []gatewayv1.Hostname
	want        gatewayv1.RouteConditionReason
}

// pinnedReasonCases follow the vendored RouteConditionReason docs
// (apis/v1/shared_types.go): NoMatchingParent is for a sectionName or port
// that matches no listener. A listener that exists but refuses the route
// reports why: NotAllowedByListeners for its allowedRoutes, and
// NoMatchingListenerHostname for its hostname.
func pinnedReasonCases() []pinnedReasonCase {
	return []pinnedReasonCase{
		{
			name: "sectionName, refused by namespace", sectionName: ptr(gatewayv1.SectionName("pinned")),
			routeNS: "elsewhere", routeKind: KindHTTPRoute, want: gatewayv1.RouteReasonNotAllowedByListeners,
		},
		{
			name: "port, refused by namespace", port: ptr(gatewayv1.PortNumber(8080)),
			routeNS: "elsewhere", routeKind: KindHTTPRoute, want: gatewayv1.RouteReasonNotAllowedByListeners,
		},
		{
			name: "sectionName, refused by kind", sectionName: ptr(gatewayv1.SectionName("pinned")),
			routeNS: "infra", routeKind: KindGRPCRoute, want: gatewayv1.RouteReasonNotAllowedByListeners,
		},
		{
			name: "sectionName, refused by hostname", sectionName: ptr(gatewayv1.SectionName("pinned")),
			routeNS: "infra", routeKind: KindHTTPRoute, hostnames: []gatewayv1.Hostname{"other.example.org"},
			want: gatewayv1.RouteReasonNoMatchingListenerHostname,
		},
		{
			name: "sectionName matching no listener", sectionName: ptr(gatewayv1.SectionName("missing")),
			routeNS: "infra", routeKind: KindHTTPRoute, want: gatewayv1.RouteReasonNoMatchingParent,
		},
		{
			name: "port matching no listener", port: ptr(gatewayv1.PortNumber(9999)),
			routeNS: "infra", routeKind: KindHTTPRoute, want: gatewayv1.RouteReasonNoMatchingParent,
		},
	}
}

// pinnedListener admits HTTPRoutes from its own namespace for its hostname.
func pinnedListener() (gatewayv1.SectionName, gatewayv1.PortNumber, *gatewayv1.Hostname, *gatewayv1.AllowedRoutes) {
	fromSame := gatewayv1.NamespacesFromSame

	return "pinned", 8080, ptr(gatewayv1.Hostname("app.example.com")), &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{From: &fromSame},
		Kinds:      []gatewayv1.RouteGroupKind{{Kind: KindHTTPRoute}},
	}
}

func (tc pinnedReasonCase) route() *RouteInfo {
	hostnames := tc.hostnames
	if hostnames == nil {
		hostnames = []gatewayv1.Hostname{"app.example.com"}
	}

	return &RouteInfo{
		Name: "r", Namespace: tc.routeNS, Kind: tc.routeKind, Hostnames: hostnames,
		SectionName: tc.sectionName, Port: tc.port,
	}
}

func pinnedNamespaces() []*corev1.Namespace {
	return []*corev1.Namespace{
		{ObjectMeta: metav1.ObjectMeta{Name: "infra"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere"}},
	}
}

// TestValidateBinding_PinnedListenerReportsWhyItRefused pins the reason for a
// parentRef pinned to a Gateway listener.
func TestValidateBinding_PinnedListenerReportsWhyItRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range pinnedReasonCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			name, port, hostname, allowed := pinnedListener()
			gateway := &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
				Spec: gatewayv1.GatewaySpec{Listeners: []gatewayv1.Listener{{
					Name: name, Port: port, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostname, AllowedRoutes: allowed,
				}}},
			}

			namespaces := pinnedNamespaces()
			result, err := NewValidator(setupFakeClient(namespaces[0], namespaces[1])).ValidateBinding(context.Background(), gateway, tc.route())
			require.NoError(t, err)

			assert.False(t, result.Accepted)
			assert.Equal(t, tc.want, result.Reason)
		})
	}
}

// TestValidateBindingForListenerSet_PinnedEntryReportsWhyItRefused pins the
// same for a parentRef pinned to a ListenerSet entry.
func TestValidateBindingForListenerSet_PinnedEntryReportsWhyItRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range pinnedReasonCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			name, port, hostname, allowed := pinnedListener()
			listenerSet := &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
				Spec: gatewayv1.ListenerSetSpec{Listeners: []gatewayv1.ListenerEntry{{
					Name: name, Port: port, Protocol: gatewayv1.HTTPProtocolType, Hostname: hostname, AllowedRoutes: allowed,
				}}},
			}

			namespaces := pinnedNamespaces()
			result, err := NewValidator(setupFakeClient(namespaces[0], namespaces[1])).
				ValidateBindingForListenerSet(context.Background(), listenerSet, tc.route())
			require.NoError(t, err)

			assert.False(t, result.Accepted)
			assert.Equal(t, tc.want, result.Reason)
		})
	}
}
