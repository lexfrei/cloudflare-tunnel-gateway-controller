package routebinding

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
)

func setupFakeClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = gatewayv1.Install(scheme)

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
}

func TestIsNamespaceAllowed(t *testing.T) {
	t.Parallel()

	fromSame := gatewayv1.NamespacesFromSame
	fromAll := gatewayv1.NamespacesFromAll
	fromSelector := gatewayv1.NamespacesFromSelector
	fromNone := gatewayv1.NamespacesFromNone

	tests := []struct {
		name             string
		allowedRoutes    *gatewayv1.AllowedRoutes
		gatewayNamespace string
		routeNamespace   string
		namespaces       []client.Object
		expected         bool
	}{
		{
			name:             "nil allowedRoutes defaults to Same namespace",
			allowedRoutes:    nil,
			gatewayNamespace: "default",
			routeNamespace:   "default",
			expected:         true,
		},
		{
			name:             "nil allowedRoutes rejects different namespace",
			allowedRoutes:    nil,
			gatewayNamespace: "default",
			routeNamespace:   "other",
			expected:         false,
		},
		{
			name: "nil namespaces in allowedRoutes defaults to Same",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: nil,
			},
			gatewayNamespace: "default",
			routeNamespace:   "default",
			expected:         true,
		},
		{
			name: "nil From in namespaces defaults to Same",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: nil,
				},
			},
			gatewayNamespace: "default",
			routeNamespace:   "default",
			expected:         true,
		},
		{
			name: "Same allows same namespace",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: &fromSame,
				},
			},
			gatewayNamespace: "default",
			routeNamespace:   "default",
			expected:         true,
		},
		{
			name: "Same rejects different namespace",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: &fromSame,
				},
			},
			gatewayNamespace: "default",
			routeNamespace:   "other",
			expected:         false,
		},
		{
			name: "All allows any namespace",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: &fromAll,
				},
			},
			gatewayNamespace: "default",
			routeNamespace:   "any-namespace",
			expected:         true,
		},
		{
			name: "None rejects all namespaces",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: &fromNone,
				},
			},
			gatewayNamespace: "default",
			routeNamespace:   "default",
			expected:         false,
		},
		{
			name: "None rejects even same namespace",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: &fromNone,
				},
			},
			gatewayNamespace: "gateway-ns",
			routeNamespace:   "gateway-ns",
			expected:         false,
		},
		{
			name: "Selector with matching labels allows namespace",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: &fromSelector,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"gateway-access": "allowed",
						},
					},
				},
			},
			gatewayNamespace: "default",
			routeNamespace:   "labeled-ns",
			namespaces: []client.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "labeled-ns",
						Labels: map[string]string{
							"gateway-access": "allowed",
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "Selector with non-matching labels rejects namespace",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: &fromSelector,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"gateway-access": "allowed",
						},
					},
				},
			},
			gatewayNamespace: "default",
			routeNamespace:   "unlabeled-ns",
			namespaces: []client.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "unlabeled-ns",
						Labels: map[string]string{
							"gateway-access": "denied",
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "Selector with no labels rejects namespace",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: &fromSelector,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"gateway-access": "allowed",
						},
					},
				},
			},
			gatewayNamespace: "default",
			routeNamespace:   "no-labels-ns",
			namespaces: []client.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "no-labels-ns",
					},
				},
			},
			expected: false,
		},
		{
			name: "Selector with namespace not found rejects",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: &fromSelector,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"gateway-access": "allowed",
						},
					},
				},
			},
			gatewayNamespace: "default",
			routeNamespace:   "nonexistent-ns",
			namespaces:       []client.Object{},
			expected:         false,
		},
		{
			name: "Selector with MatchExpressions",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: &fromSelector,
					Selector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "env",
								Operator: metav1.LabelSelectorOpIn,
								Values:   []string{"prod", "staging"},
							},
						},
					},
				},
			},
			gatewayNamespace: "default",
			routeNamespace:   "prod-ns",
			namespaces: []client.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "prod-ns",
						Labels: map[string]string{
							"env": "prod",
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "Selector allows gateway namespace even with labels",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Namespaces: &gatewayv1.RouteNamespaces{
					From: &fromSelector,
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"gateway-access": "allowed",
						},
					},
				},
			},
			gatewayNamespace: "gateway-ns",
			routeNamespace:   "gateway-ns",
			namespaces: []client.Object{
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: "gateway-ns",
						Labels: map[string]string{
							"gateway-access": "allowed",
						},
					},
				},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cli := setupFakeClient(tt.namespaces...)
			validator := NewValidator(cli)

			result, err := validator.IsNamespaceAllowed(
				context.Background(),
				tt.allowedRoutes,
				tt.gatewayNamespace,
				tt.routeNamespace,
			)

			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsNamespaceAllowed_SelectorError(t *testing.T) {
	t.Parallel()

	fromSelector := gatewayv1.NamespacesFromSelector

	cli := setupFakeClient()
	validator := NewValidator(cli)

	allowedRoutes := &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{
			From: &fromSelector,
			Selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "invalid",
						Operator: "InvalidOperator",
						Values:   []string{"value"},
					},
				},
			},
		},
	}

	_, err := validator.IsNamespaceAllowed(
		context.Background(),
		allowedRoutes,
		"default",
		"route-ns",
	)

	assert.Error(t, err)
}
