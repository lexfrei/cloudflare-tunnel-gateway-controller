package controller

import (
	"context"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// kindGateway is the Gateway API kind for Gateway resources.
const kindGateway = "Gateway"

// RequestsFunc returns reconcile requests for a given context.
type RequestsFunc func(ctx context.Context) []reconcile.Request

// ConfigMapper provides shared mapping logic for GatewayClassConfig and Secret events.
type ConfigMapper struct {
	Client           client.Client
	GatewayClassName string
	ConfigResolver   *config.Resolver
}

// MapConfigToRequests returns a mapper function for GatewayClassConfig events.
func (m *ConfigMapper) MapConfigToRequests(getRequests RequestsFunc) func(context.Context, client.Object) []reconcile.Request {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		cfg, ok := obj.(*v1alpha1.GatewayClassConfig)
		if !ok {
			return nil
		}

		if !m.isConfigForOurClass(ctx, cfg) {
			return nil
		}

		return getRequests(ctx)
	}
}

// MapSecretToRequests returns a mapper function for Secret events.
func (m *ConfigMapper) MapSecretToRequests(getRequests RequestsFunc) func(context.Context, client.Object) []reconcile.Request {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		secret, ok := obj.(*corev1.Secret)
		if !ok {
			return nil
		}

		if !m.isSecretReferencedByConfig(ctx, secret) {
			return nil
		}

		return getRequests(ctx)
	}
}

func (m *ConfigMapper) isConfigForOurClass(ctx context.Context, cfg *v1alpha1.GatewayClassConfig) bool {
	gatewayClass := &gatewayv1.GatewayClass{}

	err := m.Client.Get(ctx, types.NamespacedName{Name: m.GatewayClassName}, gatewayClass)
	if err != nil {
		return false
	}

	if gatewayClass.Spec.ParametersRef == nil {
		return false
	}

	return gatewayClass.Spec.ParametersRef.Name == cfg.Name
}

func (m *ConfigMapper) isSecretReferencedByConfig(ctx context.Context, secret *corev1.Secret) bool {
	gatewayClass := &gatewayv1.GatewayClass{}

	err := m.Client.Get(ctx, types.NamespacedName{Name: m.GatewayClassName}, gatewayClass)
	if err != nil {
		return false
	}

	cfg, cfgErr := m.ConfigResolver.GetConfigForGatewayClass(ctx, gatewayClass)
	if cfgErr != nil {
		return false
	}

	return SecretMatchesConfig(secret, cfg)
}

// SecretMatchesConfig checks if a Secret is referenced by the GatewayClassConfig.
func SecretMatchesConfig(secret *corev1.Secret, cfg *v1alpha1.GatewayClassConfig) bool {
	credRef := cfg.Spec.CloudflareCredentialsSecretRef
	if secret.Name == credRef.Name && (credRef.Namespace == "" || credRef.Namespace == secret.Namespace) {
		return true
	}

	if cfg.Spec.TunnelTokenSecretRef != nil {
		tokenRef := cfg.Spec.TunnelTokenSecretRef
		if secret.Name == tokenRef.Name && (tokenRef.Namespace == "" || tokenRef.Namespace == secret.Namespace) {
			return true
		}
	}

	return false
}

// Route describes a Gateway API route (HTTPRoute, GRPCRoute, etc.).
type Route interface {
	GetName() string
	GetNamespace() string
	GetHostnames() []gatewayv1.Hostname
	GetParentRefs() []gatewayv1.ParentReference
	GetRouteKind() gatewayv1.Kind
	// GetCrossNamespaceBackendNamespaces returns namespaces referenced by backends
	// that differ from the route's own namespace.
	GetCrossNamespaceBackendNamespaces() []string
}

// RouteFilterFunc determines if a route is relevant (e.g., managed by our Gateway).
type RouteFilterFunc func(ctx context.Context, name, namespace string) bool

// FindRoutesForReferenceGrant returns reconcile requests for routes that have
// cross-namespace references to Services in the ReferenceGrant's namespace.
// This is used by both HTTPRoute and GRPCRoute controllers to watch ReferenceGrant changes.
func FindRoutesForReferenceGrant(
	obj client.Object,
	routes []Route,
) []reconcile.Request {
	refGrant, ok := obj.(*gatewayv1beta1.ReferenceGrant)
	if !ok {
		return nil
	}

	// ReferenceGrant is in the target namespace (where Services are)
	targetNamespace := refGrant.Namespace

	var requests []reconcile.Request

	for _, route := range routes {
		crossNsBackends := route.GetCrossNamespaceBackendNamespaces()

		if slices.Contains(crossNsBackends, targetNamespace) {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      route.GetName(),
					Namespace: route.GetNamespace(),
				},
			})
		}
	}

	return requests
}

// extractCrossNamespaceBackends returns unique namespaces from backend refs
// that differ from the route's own namespace.
func extractCrossNamespaceBackends(routeNamespace string, refs []gatewayv1.BackendRef) []string {
	var namespaces []string

	seen := make(map[string]bool)

	for _, ref := range refs {
		if ref.Namespace != nil {
			backendNs := string(*ref.Namespace)
			if backendNs != routeNamespace && !seen[backendNs] {
				namespaces = append(namespaces, backendNs)
				seen[backendNs] = true
			}
		}
	}

	return namespaces
}

// HTTPRouteWrapper wraps HTTPRoute to implement Route.
type HTTPRouteWrapper struct {
	*gatewayv1.HTTPRoute
}

// GetCrossNamespaceBackendNamespaces returns namespaces of backends in other namespaces.
func (w HTTPRouteWrapper) GetCrossNamespaceBackendNamespaces() []string {
	var refs []gatewayv1.BackendRef

	for _, rule := range w.Spec.Rules {
		for i := range rule.BackendRefs {
			refs = append(refs, rule.BackendRefs[i].BackendRef)
		}
	}

	return extractCrossNamespaceBackends(w.Namespace, refs)
}

// GRPCRouteWrapper wraps GRPCRoute to implement Route.
type GRPCRouteWrapper struct {
	*gatewayv1.GRPCRoute
}

// GetCrossNamespaceBackendNamespaces returns namespaces of backends in other namespaces.
func (w GRPCRouteWrapper) GetCrossNamespaceBackendNamespaces() []string {
	var refs []gatewayv1.BackendRef

	for _, rule := range w.Spec.Rules {
		for i := range rule.BackendRefs {
			refs = append(refs, rule.BackendRefs[i].BackendRef)
		}
	}

	return extractCrossNamespaceBackends(w.Namespace, refs)
}

// GetHostnames returns the hostnames from the HTTPRoute spec.
func (w HTTPRouteWrapper) GetHostnames() []gatewayv1.Hostname {
	return w.Spec.Hostnames
}

// GetParentRefs returns the parent references from the HTTPRoute spec.
func (w HTTPRouteWrapper) GetParentRefs() []gatewayv1.ParentReference {
	return w.Spec.ParentRefs
}

// GetRouteKind returns the route kind for HTTPRoute.
func (w HTTPRouteWrapper) GetRouteKind() gatewayv1.Kind {
	return routebinding.KindHTTPRoute
}

// GetHostnames returns the hostnames from the GRPCRoute spec.
func (w GRPCRouteWrapper) GetHostnames() []gatewayv1.Hostname {
	return w.Spec.Hostnames
}

// GetParentRefs returns the parent references from the GRPCRoute spec.
func (w GRPCRouteWrapper) GetParentRefs() []gatewayv1.ParentReference {
	return w.Spec.ParentRefs
}

// GetRouteKind returns the route kind for GRPCRoute.
func (w GRPCRouteWrapper) GetRouteKind() gatewayv1.Kind {
	return routebinding.KindGRPCRoute
}

// FindRoutesForGateway returns reconcile requests for routes that reference the given Gateway.
func FindRoutesForGateway(obj client.Object, gatewayClassName string, routes []Route) []reconcile.Request {
	gateway, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}

	if gateway.Spec.GatewayClassName != gatewayv1.ObjectName(gatewayClassName) {
		return nil
	}

	var requests []reconcile.Request

	for _, route := range routes {
		for _, ref := range route.GetParentRefs() {
			if string(ref.Name) == gateway.Name {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKey{
						Name:      route.GetName(),
						Namespace: route.GetNamespace(),
					},
				})

				break
			}
		}
	}

	return requests
}

// FilterAcceptedRoutes returns reconcile requests for routes accepted by a Gateway of the specified class.
func FilterAcceptedRoutes(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	gatewayClassName string,
	routes []Route,
) []reconcile.Request {
	var requests []reconcile.Request

	for _, route := range routes {
		if IsRouteAcceptedByGateway(ctx, cli, validator, gatewayClassName, route) {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKey{
					Name:      route.GetName(),
					Namespace: route.GetNamespace(),
				},
			})
		}
	}

	return requests
}

// IsRouteAcceptedByGateway checks if a route has at least one accepted binding
// to a Gateway of the specified class. This is used by both HTTPRoute and GRPCRoute
// controllers to determine if a route should be processed.
func IsRouteAcceptedByGateway(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	gatewayClassName string,
	route Route,
) bool {
	for _, ref := range route.GetParentRefs() {
		if ref.Kind != nil && *ref.Kind != kindGateway {
			continue
		}

		namespace := route.GetNamespace()
		if ref.Namespace != nil {
			namespace = string(*ref.Namespace)
		}

		var gateway gatewayv1.Gateway

		err := cli.Get(ctx, client.ObjectKey{Name: string(ref.Name), Namespace: namespace}, &gateway)
		if err != nil {
			continue
		}

		if gateway.Spec.GatewayClassName != gatewayv1.ObjectName(gatewayClassName) {
			continue
		}

		routeInfo := &routebinding.RouteInfo{
			Name:        route.GetName(),
			Namespace:   route.GetNamespace(),
			Hostnames:   route.GetHostnames(),
			Kind:        route.GetRouteKind(),
			SectionName: ref.SectionName,
		}

		result, err := validator.ValidateBinding(ctx, &gateway, routeInfo)
		if err != nil {
			logging.FromContext(ctx).Error("failed to validate route binding",
				"route", route.GetNamespace()+"/"+route.GetName(),
				"gateway", gateway.Name,
				"error", err)

			continue
		}

		if result.Accepted {
			return true
		}
	}

	return false
}
