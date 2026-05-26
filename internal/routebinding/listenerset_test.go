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

func TestEvaluateListenerSetAcceptance(t *testing.T) {
	t.Parallel()

	fromNone := gatewayv1.NamespacesFromNone
	fromSame := gatewayv1.NamespacesFromSame
	fromAll := gatewayv1.NamespacesFromAll
	fromSelector := gatewayv1.NamespacesFromSelector

	tests := []struct {
		name           string
		gateway        *gatewayv1.Gateway
		listenerSet    *gatewayv1.ListenerSet
		objects        []client.Object
		expectAccepted bool
		expectReason   gatewayv1.ListenerSetConditionReason
	}{
		{
			name: "rejects ListenerSet when allowedListeners is unset (defaults to None)",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
				Spec:       gatewayv1.GatewaySpec{},
			},
			listenerSet: &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
			},
			expectAccepted: false,
			expectReason:   gatewayv1.ListenerSetReasonNotAllowed,
		},
		{
			name: "rejects ListenerSet when allowedListeners.namespaces.from is None",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
				Spec: gatewayv1.GatewaySpec{
					AllowedListeners: &gatewayv1.AllowedListeners{
						Namespaces: &gatewayv1.ListenerNamespaces{From: &fromNone},
					},
				},
			},
			listenerSet: &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
			},
			expectAccepted: false,
			expectReason:   gatewayv1.ListenerSetReasonNotAllowed,
		},
		{
			name: "accepts same-namespace ListenerSet when from=Same",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
				Spec: gatewayv1.GatewaySpec{
					AllowedListeners: &gatewayv1.AllowedListeners{
						Namespaces: &gatewayv1.ListenerNamespaces{From: &fromSame},
					},
				},
			},
			listenerSet: &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
			},
			expectAccepted: true,
			expectReason:   gatewayv1.ListenerSetReasonAccepted,
		},
		{
			name: "rejects cross-namespace ListenerSet when from=Same",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
				Spec: gatewayv1.GatewaySpec{
					AllowedListeners: &gatewayv1.AllowedListeners{
						Namespaces: &gatewayv1.ListenerNamespaces{From: &fromSame},
					},
				},
			},
			listenerSet: &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "team-a"},
			},
			expectAccepted: false,
			expectReason:   gatewayv1.ListenerSetReasonNotAllowed,
		},
		{
			name: "accepts any-namespace ListenerSet when from=All",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
				Spec: gatewayv1.GatewaySpec{
					AllowedListeners: &gatewayv1.AllowedListeners{
						Namespaces: &gatewayv1.ListenerNamespaces{From: &fromAll},
					},
				},
			},
			listenerSet: &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "team-a"},
			},
			expectAccepted: true,
			expectReason:   gatewayv1.ListenerSetReasonAccepted,
		},
		{
			name: "accepts selector-matched ListenerSet namespace",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
				Spec: gatewayv1.GatewaySpec{
					AllowedListeners: &gatewayv1.AllowedListeners{
						Namespaces: &gatewayv1.ListenerNamespaces{
							From: &fromSelector,
							Selector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"trusted": "yes"},
							},
						},
					},
				},
			},
			listenerSet: &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "team-a"},
			},
			objects: []client.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "team-a",
						Labels: map[string]string{"trusted": "yes"},
					},
				},
			},
			expectAccepted: true,
			expectReason:   gatewayv1.ListenerSetReasonAccepted,
		},
		{
			name: "rejects selector-mismatched ListenerSet namespace",
			gateway: &gatewayv1.Gateway{
				ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "infra"},
				Spec: gatewayv1.GatewaySpec{
					AllowedListeners: &gatewayv1.AllowedListeners{
						Namespaces: &gatewayv1.ListenerNamespaces{
							From: &fromSelector,
							Selector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"trusted": "yes"},
							},
						},
					},
				},
			},
			listenerSet: &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "team-b"},
			},
			objects: []client.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "team-b",
						Labels: map[string]string{"trusted": "no"},
					},
				},
			},
			expectAccepted: false,
			expectReason:   gatewayv1.ListenerSetReasonNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			validator := NewValidator(setupFakeClient(tt.objects...))

			result, err := validator.EvaluateListenerSetAcceptance(context.Background(), tt.gateway, tt.listenerSet)
			require.NoError(t, err)
			assert.Equal(t, tt.expectAccepted, result.Accepted)
			assert.Equal(t, tt.expectReason, result.Reason)
		})
	}
}

func TestValidateBindingForListenerSet(t *testing.T) {
	t.Parallel()

	fromAll := gatewayv1.NamespacesFromAll
	fromSame := gatewayv1.NamespacesFromSame

	tests := []struct {
		name             string
		listenerSet      *gatewayv1.ListenerSet
		route            *RouteInfo
		expectedAccepted bool
		expectedReason   gatewayv1.RouteConditionReason
		expectedMatched  []gatewayv1.SectionName
	}{
		{
			name: "accepts route matching one of the ListenerSet entries",
			listenerSet: &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
				Spec: gatewayv1.ListenerSetSpec{
					Listeners: []gatewayv1.ListenerEntry{
						{
							Name:     "extra",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:      "r",
				Namespace: "team-a",
				Hostnames: []gatewayv1.Hostname{"extra.example.com"},
				Kind:      KindHTTPRoute,
			},
			expectedAccepted: true,
			expectedReason:   gatewayv1.RouteReasonAccepted,
			expectedMatched:  []gatewayv1.SectionName{"extra"},
		},
		{
			name: "honours sectionName against ListenerSet entry name",
			listenerSet: &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
				Spec: gatewayv1.ListenerSetSpec{
					Listeners: []gatewayv1.ListenerEntry{
						{
							Name:     "one",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll},
							},
						},
						{
							Name:     "two",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:        "r",
				Namespace:   "team-a",
				Hostnames:   nil,
				Kind:        KindHTTPRoute,
				SectionName: ptr(gatewayv1.SectionName("two")),
			},
			expectedAccepted: true,
			expectedReason:   gatewayv1.RouteReasonAccepted,
			expectedMatched:  []gatewayv1.SectionName{"two"},
		},
		{
			name: "rejects route with sectionName not present in ListenerSet",
			listenerSet: &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
				Spec: gatewayv1.ListenerSetSpec{
					Listeners: []gatewayv1.ListenerEntry{
						{
							Name:     "only",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{From: &fromAll},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:        "r",
				Namespace:   "team-a",
				Hostnames:   nil,
				Kind:        KindHTTPRoute,
				SectionName: ptr(gatewayv1.SectionName("missing")),
			},
			expectedAccepted: false,
			expectedReason:   gatewayv1.RouteReasonNoMatchingParent,
		},
		{
			name: "rejects route in disallowed namespace per ListenerEntry allowedRoutes",
			listenerSet: &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
				Spec: gatewayv1.ListenerSetSpec{
					Listeners: []gatewayv1.ListenerEntry{
						{
							Name:     "only",
							Port:     80,
							Protocol: gatewayv1.HTTPProtocolType,
							AllowedRoutes: &gatewayv1.AllowedRoutes{
								Namespaces: &gatewayv1.RouteNamespaces{From: &fromSame},
							},
						},
					},
				},
			},
			route: &RouteInfo{
				Name:      "r",
				Namespace: "team-a",
				Hostnames: nil,
				Kind:      KindHTTPRoute,
			},
			expectedAccepted: false,
			expectedReason:   gatewayv1.RouteReasonNotAllowedByListeners,
		},
		{
			name: "rejects route when ListenerSet has no listeners",
			listenerSet: &gatewayv1.ListenerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ls", Namespace: "infra"},
				Spec:       gatewayv1.ListenerSetSpec{},
			},
			route: &RouteInfo{
				Name:      "r",
				Namespace: "team-a",
				Hostnames: nil,
				Kind:      KindHTTPRoute,
			},
			expectedAccepted: false,
			expectedReason:   gatewayv1.RouteReasonNoMatchingParent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			validator := NewValidator(setupFakeClient())

			result, err := validator.ValidateBindingForListenerSet(context.Background(), tt.listenerSet, tt.route)
			require.NoError(t, err)
			assert.Equal(t, tt.expectedAccepted, result.Accepted)
			assert.Equal(t, tt.expectedReason, result.Reason)
			assert.Equal(t, tt.expectedMatched, result.MatchedListeners)
		})
	}
}
