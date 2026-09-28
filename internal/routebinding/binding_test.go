package routebinding

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
)

func TestValidateBinding(t *testing.T) {
	t.Parallel()

	fromAll := gatewayv1.NamespacesFromAll
	fromSame := gatewayv1.NamespacesFromSame

	tests := []struct {
		name             string
		gateway          *gatewayv1.Gateway
		route            *RouteInfo
		objects          []client.Object
		expectedAccepted bool
		expectedReason   gatewayv1.RouteConditionReason
		expectedMatched  []gatewayv1.SectionName
	}{
		{
			name: "route accepted - all validations pass",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:      "test-route",
				Namespace: "default",
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Kind:      "HTTPRoute",
			},
			expectedAccepted: true,
			expectedReason:   gatewayv1.RouteReasonAccepted,
			expectedMatched:  []gatewayv1.SectionName{"http"},
		},
		{
			name: "route rejected - hostname mismatch",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							Hostname: ptr(gatewayv1.Hostname("*.example.com")),
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:      "test-route",
				Namespace: "default",
				Hostnames: []gatewayv1.Hostname{"other.com"},
				Kind:      "HTTPRoute",
			},
			expectedAccepted: false,
			expectedReason:   gatewayv1.RouteReasonNoMatchingListenerHostname,
			expectedMatched:  nil,
		},
		{
			name: "route rejected - namespace not allowed",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "gateway-ns",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromSame,
								},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:      "test-route",
				Namespace: "other-ns",
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Kind:      "HTTPRoute",
			},
			expectedAccepted: false,
			expectedReason:   gatewayv1.RouteReasonNotAllowedByListeners,
			expectedMatched:  nil,
		},
		{
			name: "route rejected - kind not allowed",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
								Kinds: []gatewayv1.RouteGroupKind{
									{Kind: "GRPCRoute"},
								},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:      "test-route",
				Namespace: "default",
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Kind:      "HTTPRoute",
			},
			expectedAccepted: false,
			expectedReason:   gatewayv1.RouteReasonNotAllowedByListeners,
			expectedMatched:  nil,
		},
		{
			name: "route with SectionName matches specific listener",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
						{
							Name:     "https",
							Port:     443,
							Protocol: gatewayv1.HTTPSProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:        "test-route",
				Namespace:   "default",
				Hostnames:   []gatewayv1.Hostname{"example.com"},
				Kind:        "HTTPRoute",
				SectionName: ptr(gatewayv1.SectionName("https")),
			},
			expectedAccepted: true,
			expectedReason:   gatewayv1.RouteReasonAccepted,
			expectedMatched:  []gatewayv1.SectionName{"https"},
		},
		{
			name: "route with SectionName not found",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:        "test-route",
				Namespace:   "default",
				Hostnames:   []gatewayv1.Hostname{"example.com"},
				Kind:        "HTTPRoute",
				SectionName: ptr(gatewayv1.SectionName("nonexistent")),
			},
			expectedAccepted: false,
			expectedReason:   gatewayv1.RouteReasonNoMatchingParent,
			expectedMatched:  nil,
		},
		{
			name: "route matches multiple listeners",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
						{
							Name:     "https",
							Port:     443,
							Protocol: gatewayv1.HTTPSProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:      "test-route",
				Namespace: "default",
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Kind:      "HTTPRoute",
			},
			expectedAccepted: true,
			expectedReason:   gatewayv1.RouteReasonAccepted,
			expectedMatched:  []gatewayv1.SectionName{"http", "https"},
		},
		{
			name: "wildcard listener hostname matches route",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							Hostname: ptr(gatewayv1.Hostname("*.example.com")),
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:      "test-route",
				Namespace: "default",
				Hostnames: []gatewayv1.Hostname{"app.example.com"},
				Kind:      "HTTPRoute",
			},
			expectedAccepted: true,
			expectedReason:   gatewayv1.RouteReasonAccepted,
			expectedMatched:  []gatewayv1.SectionName{"http"},
		},
		{
			name: "no listeners in gateway",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{},
				},
			},
			route: &RouteInfo{
				Name:      "test-route",
				Namespace: "default",
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Kind:      "HTTPRoute",
			},
			expectedAccepted: false,
			expectedReason:   gatewayv1.RouteReasonNoMatchingParent,
			expectedMatched:  nil,
		},
		{
			name: "route with SectionName and matching Port accepted",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:        "test-route",
				Namespace:   "default",
				Hostnames:   []gatewayv1.Hostname{"example.com"},
				Kind:        "HTTPRoute",
				SectionName: ptr(gatewayv1.SectionName("http")),
				Port:        ptr(gatewayv1.PortNumber(80)),
			},
			expectedAccepted: true,
			expectedReason:   gatewayv1.RouteReasonAccepted,
			expectedMatched:  []gatewayv1.SectionName{"http"},
		},
		{
			name: "route with SectionName matching but Port mismatching rejected",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:        "test-route",
				Namespace:   "default",
				Hostnames:   []gatewayv1.Hostname{"example.com"},
				Kind:        "HTTPRoute",
				SectionName: ptr(gatewayv1.SectionName("http")),
				Port:        ptr(gatewayv1.PortNumber(81)),
			},
			expectedAccepted: false,
			expectedReason:   gatewayv1.RouteReasonNoMatchingParent,
			expectedMatched:  nil,
		},
		{
			// This case also pins the upstream conformance fixture
			// httproute-invalid-parentref-not-matching-listener-port.yaml:
			// single listener on port 80, parentRef.port=81, no sectionName.
			// Spec requires Accepted=False with Reason=NoMatchingParent.
			name: "route with Port only (no SectionName) rejected when no listener matches port",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:      "test-route",
				Namespace: "default",
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Kind:      "HTTPRoute",
				Port:      ptr(gatewayv1.PortNumber(81)),
			},
			expectedAccepted: false,
			expectedReason:   gatewayv1.RouteReasonNoMatchingParent,
			expectedMatched:  nil,
		},
		{
			name: "route with Port only (no SectionName) accepted when listener matches port",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
						{
							Name:     "alt",
							Port:     8080,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:      "test-route",
				Namespace: "default",
				Hostnames: []gatewayv1.Hostname{"example.com"},
				Kind:      "HTTPRoute",
				Port:      ptr(gatewayv1.PortNumber(8080)),
			},
			expectedAccepted: true,
			expectedReason:   gatewayv1.RouteReasonAccepted,
			expectedMatched:  []gatewayv1.SectionName{"alt"},
		},
		{
			name: "partial match - one listener matches one does not",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gateway",
					Namespace: "default",
				},
				Spec: gatewayv1.GatewaySpec{
					Listeners: []gatewayv1.Listener{
						{
							Name:     "http-public",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							Hostname: ptr(gatewayv1.Hostname("public.example.com")),
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
						{
							Name:     "http-internal",
							Port:     8080,
							Protocol: gatewayv1.HTTPProtocolType,
							Hostname: ptr(gatewayv1.Hostname("internal.example.com")),
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{
									From: &fromAll,
								},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:      "test-route",
				Namespace: "default",
				Hostnames: []gatewayv1.Hostname{"public.example.com"},
				Kind:      "HTTPRoute",
			},
			expectedAccepted: true,
			expectedReason:   gatewayv1.RouteReasonAccepted,
			expectedMatched:  []gatewayv1.SectionName{"http-public"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cli := setupFakeClient(tt.objects...)
			validator := NewValidator(cli)

			result, err := validator.ValidateBinding(context.Background(), tt.gateway, tt.route)

			require.NoError(t, err)
			assert.Equal(t, tt.expectedAccepted, result.Accepted)
			assert.Equal(t, tt.expectedReason, result.Reason)
			assert.ElementsMatch(t, tt.expectedMatched, result.MatchedListeners)
		})
	}
}

func TestValidateBinding_WithNamespaceSelector(t *testing.T) {
	t.Parallel()

	fromSelector := gatewayv1.NamespacesFromSelector

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gateway",
			Namespace: "gateway-ns",
		},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{
					Name:     "http",
					Port:     80,
					Protocol: gatewayv1.HTTPProtocolType,
					AllowedRoutes: &gatewayv1.AllowedRoutes{
						Namespaces: &gatewayv1.RouteNamespaces{
							From: &fromSelector,
							Selector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"gateway-access": "allowed",
								},
							},
						},
					},
				},
			},
		},
	}

	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "allowed-ns",
			Labels: map[string]string{
				"gateway-access": "allowed",
			},
		},
	}

	cli := setupFakeClient(namespace)
	validator := NewValidator(cli)

	route := &RouteInfo{
		Name:      "test-route",
		Namespace: "allowed-ns",
		Hostnames: []gatewayv1.Hostname{"example.com"},
		Kind:      "HTTPRoute",
	}

	result, err := validator.ValidateBinding(context.Background(), gateway, route)

	require.NoError(t, err)
	assert.True(t, result.Accepted)
	assert.Equal(t, gatewayv1.RouteReasonAccepted, result.Reason)
	assert.Equal(t, []gatewayv1.SectionName{"http"}, result.MatchedListeners)
}

// bogusSelector is a label selector LabelSelectorAsSelector refuses to parse.
func bogusSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "team", Operator: "BogusOperator", Values: []string{"x"}},
		},
	}
}

// selectorListener builds an HTTP listener admitting routes by namespace
// selector.
func selectorListener(name gatewayv1.SectionName, selector *metav1.LabelSelector) gatewayv1.Listener {
	fromSelector := gatewayv1.NamespacesFromSelector

	return gatewayv1.Listener{
		Name:     name,
		Port:     80,
		Protocol: gatewayv1.HTTPProtocolType,
		AllowedRoutes: &gatewayv1.AllowedRoutes{
			Namespaces: &gatewayv1.RouteNamespaces{From: &fromSelector, Selector: selector},
		},
	}
}

// TestValidateBinding_InvalidSelectorRejectsWithMessage pins where an
// unparseable allowedRoutes selector ends up: a listener with one admits no
// route, and the rejection message names that listener so the route's status
// shows why. The selector itself stays out of the message: the route may live
// in a namespace whose authors cannot read the Gateway, so the parse error goes
// to the controller log instead.
func TestValidateBinding_InvalidSelectorRejectsWithMessage(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{selectorListener("broken", bogusSelector())},
		},
	}

	route := &RouteInfo{Namespace: "apps", Kind: KindHTTPRoute}

	logger, logs := logging.TestLogger(t)
	ctx := logging.WithLogger(context.Background(), logger)

	result, err := NewValidator(setupFakeClient()).ValidateBinding(ctx, gateway, route)
	require.NoError(t, err)

	assert.False(t, result.Accepted)
	assert.Equal(t, gatewayv1.RouteReasonNotAllowedByListeners, result.Reason)
	assert.Contains(t, result.Message, `listener "broken" has an invalid allowedRoutes selector`)
	assert.NotContains(t, result.Message, "BogusOperator", "the Gateway's selector must not reach route status")
	assert.Contains(t, logs.String(), "BogusOperator", "the parse error is kept for the controller log")
}

// TestValidateBinding_InvalidSelectorStaysWithItsListener pins that one
// listener's unparseable selector does not fail the whole Gateway: a route with
// no sectionName still binds to the sibling listener that admits it.
func TestValidateBinding_InvalidSelectorStaysWithItsListener(t *testing.T) {
	t.Parallel()

	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				selectorListener("broken", bogusSelector()),
				selectorListener("good", &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}),
			},
		},
	}

	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "apps",
		Labels: map[string]string{"team": "a"},
	}}

	route := &RouteInfo{Namespace: "apps", Kind: KindHTTPRoute}

	result, err := NewValidator(setupFakeClient(namespace)).ValidateBinding(context.Background(), gateway, route)
	require.NoError(t, err)

	assert.True(t, result.Accepted)
	assert.Equal(t, []gatewayv1.SectionName{"good"}, result.MatchedListeners)
}
