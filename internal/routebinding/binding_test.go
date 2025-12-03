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
