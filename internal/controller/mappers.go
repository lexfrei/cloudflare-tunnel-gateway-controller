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
)

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

// RouteWithCrossNamespaceRefs describes a route that may have cross-namespace backend references.
type RouteWithCrossNamespaceRefs interface {
	GetName() string
	GetNamespace() string
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
	routes []RouteWithCrossNamespaceRefs,
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

// HTTPRouteWrapper wraps HTTPRoute to implement RouteWithCrossNamespaceRefs.
type HTTPRouteWrapper struct {
	*gatewayv1.HTTPRoute
}

// GetCrossNamespaceBackendNamespaces returns namespaces of backends in other namespaces.
func (w HTTPRouteWrapper) GetCrossNamespaceBackendNamespaces() []string {
	var namespaces []string

	seen := make(map[string]bool)

	for _, rule := range w.Spec.Rules {
		for _, backendRef := range rule.BackendRefs {
			if backendRef.Namespace != nil {
				backendNs := string(*backendRef.Namespace)
				if backendNs != w.Namespace && !seen[backendNs] {
					namespaces = append(namespaces, backendNs)
					seen[backendNs] = true
				}
			}
		}
	}

	return namespaces
}

// GRPCRouteWrapper wraps GRPCRoute to implement RouteWithCrossNamespaceRefs.
type GRPCRouteWrapper struct {
	*gatewayv1.GRPCRoute
}

// GetCrossNamespaceBackendNamespaces returns namespaces of backends in other namespaces.
func (w GRPCRouteWrapper) GetCrossNamespaceBackendNamespaces() []string {
	var namespaces []string

	seen := make(map[string]bool)

	for _, rule := range w.Spec.Rules {
		for _, backendRef := range rule.BackendRefs {
			if backendRef.Namespace != nil {
				backendNs := string(*backendRef.Namespace)
				if backendNs != w.Namespace && !seen[backendNs] {
					namespaces = append(namespaces, backendNs)
					seen[backendNs] = true
				}
			}
		}
	}

	return namespaces
}
