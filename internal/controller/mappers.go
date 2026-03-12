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

// isGatewayManagedByController checks if a Gateway belongs to a GatewayClass
// managed by the given controller. Per Gateway API spec, controllerName is the
// binding mechanism between GatewayClass and controller, not the class name.
func isGatewayManagedByController(
	ctx context.Context,
	cli client.Client,
	gateway *gatewayv1.Gateway,
	controllerName string,
) bool {
	var gatewayClass gatewayv1.GatewayClass

	err := cli.Get(ctx, types.NamespacedName{Name: string(gateway.Spec.GatewayClassName)}, &gatewayClass)
	if err != nil {
		logging.FromContext(ctx).Debug("GatewayClass not found for gateway",
			"gateway", gateway.Namespace+"/"+gateway.Name,
			"gatewayClassName", string(gateway.Spec.GatewayClassName),
			"error", err)

		return false
	}

	return string(gatewayClass.Spec.ControllerName) == controllerName
}

// listGatewayClassesForController returns all GatewayClasses that reference
// the given controllerName.
func listGatewayClassesForController(
	ctx context.Context,
	cli client.Client,
	controllerName string,
) []gatewayv1.GatewayClass {
	var classList gatewayv1.GatewayClassList

	if err := cli.List(ctx, &classList); err != nil {
		logging.FromContext(ctx).Warn("failed to list GatewayClasses",
			"controllerName", controllerName, "error", err)

		return nil
	}

	var matched []gatewayv1.GatewayClass

	for i := range classList.Items {
		if string(classList.Items[i].Spec.ControllerName) == controllerName {
			matched = append(matched, classList.Items[i])
		}
	}

	return matched
}

// managedClassNames returns the set of GatewayClass names managed by the
// given controllerName. Used for batch operations to avoid per-Gateway lookups.
//
// Note: with controller-runtime, both Get and List hit the local informer cache,
// not the API server. This helper still improves efficiency by replacing N Get
// calls with a single List + local map lookup.
func managedClassNames(
	ctx context.Context,
	cli client.Client,
	controllerName string,
) map[string]bool {
	classes := listGatewayClassesForController(ctx, cli, controllerName)
	names := make(map[string]bool, len(classes))

	for i := range classes {
		names[classes[i].Name] = true
	}

	return names
}

// kindGateway is the Gateway API kind for Gateway resources.
const kindGateway = "Gateway"

// RequestsFunc returns reconcile requests for a given context.
type RequestsFunc func(ctx context.Context) []reconcile.Request

// ConfigMapper provides shared mapping logic for GatewayClassConfig and Secret events.
type ConfigMapper struct {
	Client         client.Client
	ControllerName string
	ConfigResolver *config.Resolver
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
	classes := listGatewayClassesForController(ctx, m.Client, m.ControllerName)

	for i := range classes {
		gc := &classes[i]
		if gc.Spec.ParametersRef != nil && gc.Spec.ParametersRef.Name == cfg.Name {
			return true
		}
	}

	return false
}

func (m *ConfigMapper) isSecretReferencedByConfig(ctx context.Context, secret *corev1.Secret) bool {
	classes := listGatewayClassesForController(ctx, m.Client, m.ControllerName)

	for i := range classes {
		gc := &classes[i]

		cfg, cfgErr := m.ConfigResolver.GetConfigForGatewayClass(ctx, gc)
		if cfgErr != nil {
			continue
		}

		if SecretMatchesConfig(secret, cfg) {
			return true
		}
	}

	return false
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
	totalRefs := 0
	for _, rule := range w.Spec.Rules {
		totalRefs += len(rule.BackendRefs)
	}

	refs := make([]gatewayv1.BackendRef, 0, totalRefs)

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
	totalRefs := 0
	for _, rule := range w.Spec.Rules {
		totalRefs += len(rule.BackendRefs)
	}

	refs := make([]gatewayv1.BackendRef, 0, totalRefs)

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
// It checks whether the Gateway's GatewayClass is managed by the given controllerName.
func FindRoutesForGateway(
	ctx context.Context,
	cli client.Client,
	obj client.Object,
	controllerName string,
	routes []Route,
) []reconcile.Request {
	gateway, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}

	if !isGatewayManagedByController(ctx, cli, gateway, controllerName) {
		return nil
	}

	var requests []reconcile.Request

	for _, route := range routes {
		for _, ref := range route.GetParentRefs() {
			refNamespace := route.GetNamespace()
			if ref.Namespace != nil {
				refNamespace = string(*ref.Namespace)
			}

			if string(ref.Name) == gateway.Name && refNamespace == gateway.Namespace {
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

// FilterAcceptedRoutes returns reconcile requests for routes accepted by a Gateway
// managed by the given controllerName.
func FilterAcceptedRoutes(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	controllerName string,
	routes []Route,
) []reconcile.Request {
	var requests []reconcile.Request

	for _, route := range routes {
		if IsRouteAcceptedByGateway(ctx, cli, validator, controllerName, route) {
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

// routeReferencesOurGateways checks if a route references at least one Gateway
// managed by the given controllerName. Unlike IsRouteAcceptedByGateway, this does
// not perform binding validation — it only checks if the parentRef points to our Gateway.
// This is used to decide whether to trigger a full sync (which includes status updates
// for both accepted and rejected routes).
func routeReferencesOurGateways(
	ctx context.Context,
	cli client.Client,
	controllerName string,
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

		if isGatewayManagedByController(ctx, cli, &gateway, controllerName) {
			return true
		}
	}

	return false
}

// IsRouteAcceptedByGateway checks if a route has at least one accepted binding
// to a Gateway managed by the given controllerName. This is used by both HTTPRoute
// and GRPCRoute controllers to determine if a route should be processed.
func IsRouteAcceptedByGateway(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	controllerName string,
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

		if !isGatewayManagedByController(ctx, cli, &gateway, controllerName) {
			continue
		}

		routeInfo := &routebinding.RouteInfo{
			Name:        route.GetName(),
			Namespace:   route.GetNamespace(),
			Hostnames:   route.GetHostnames(),
			Kind:        route.GetRouteKind(),
			SectionName: ref.SectionName,
			Port:        ref.Port,
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
