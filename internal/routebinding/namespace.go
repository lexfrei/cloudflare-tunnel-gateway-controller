package routebinding

import (
	"context"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// Validator performs route binding validation against Gateway listeners.
type Validator struct {
	client client.Client
}

// NewValidator creates a new Validator with the given client.
func NewValidator(cli client.Client) *Validator {
	return &Validator{client: cli}
}

// IsNamespaceAllowed checks if a route from routeNamespace is allowed to attach
// to a listener based on its allowedRoutes configuration.
// Per Gateway API spec:
//   - nil/empty allowedRoutes defaults to Same namespace.
//   - Same: only routes from the same namespace as the Gateway.
//   - All: routes from any namespace.
//   - Selector: routes from namespaces matching the label selector.
func (v *Validator) IsNamespaceAllowed(
	ctx context.Context,
	allowedRoutes *gatewayv1.AllowedRoutes,
	gatewayNamespace string,
	routeNamespace string,
) (bool, error) {
	from := getNamespaceFrom(allowedRoutes)

	switch from {
	case gatewayv1.NamespacesFromSame:
		return gatewayNamespace == routeNamespace, nil

	case gatewayv1.NamespacesFromAll:
		return true, nil

	case gatewayv1.NamespacesFromSelector:
		return v.namespaceMatchesSelector(ctx, allowedRoutes, routeNamespace)

	case gatewayv1.NamespacesFromNone:
		return false, nil
	}

	return gatewayNamespace == routeNamespace, nil
}

// getNamespaceFrom extracts the From field from allowedRoutes, defaulting to Same.
func getNamespaceFrom(allowedRoutes *gatewayv1.AllowedRoutes) gatewayv1.FromNamespaces {
	if allowedRoutes == nil {
		return gatewayv1.NamespacesFromSame
	}

	if allowedRoutes.Namespaces == nil {
		return gatewayv1.NamespacesFromSame
	}

	if allowedRoutes.Namespaces.From == nil {
		return gatewayv1.NamespacesFromSame
	}

	return *allowedRoutes.Namespaces.From
}

// namespaceMatchesSelector checks if the route namespace matches the selector.
func (v *Validator) namespaceMatchesSelector(
	ctx context.Context,
	allowedRoutes *gatewayv1.AllowedRoutes,
	routeNamespace string,
) (bool, error) {
	if allowedRoutes.Namespaces.Selector == nil {
		return false, nil
	}

	selector, err := metav1.LabelSelectorAsSelector(allowedRoutes.Namespaces.Selector)
	if err != nil {
		return false, errors.Wrap(err, "invalid label selector")
	}

	var namespace corev1.Namespace

	err = v.client.Get(ctx, client.ObjectKey{Name: routeNamespace}, &namespace)
	if err != nil {
		return false, nil //nolint:nilerr // namespace not found means not allowed
	}

	return selector.Matches(labels.Set(namespace.Labels)), nil
}
