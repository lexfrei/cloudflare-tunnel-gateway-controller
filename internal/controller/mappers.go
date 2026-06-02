package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
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
) ([]gatewayv1.GatewayClass, error) {
	var classList gatewayv1.GatewayClassList

	if err := cli.List(ctx, &classList); err != nil {
		return nil, fmt.Errorf("listing GatewayClasses for controller %s: %w", controllerName, err)
	}

	var matched []gatewayv1.GatewayClass

	for i := range classList.Items {
		if string(classList.Items[i].Spec.ControllerName) == controllerName {
			matched = append(matched, classList.Items[i])
		}
	}

	return matched, nil
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
) (map[string]bool, error) {
	classes, err := listGatewayClassesForController(ctx, cli, controllerName)
	if err != nil {
		return nil, err
	}

	names := make(map[string]bool, len(classes))

	for i := range classes {
		names[classes[i].Name] = true
	}

	return names, nil
}

// kindGateway / kindListenerSet are the Gateway API kinds used when route
// parentRefs and ReferenceGrant from/to entries select the resource type.
const (
	kindGateway     = "Gateway"
	kindListenerSet = "ListenerSet"
)

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
	classes, err := listGatewayClassesForController(ctx, m.Client, m.ControllerName)
	if err != nil {
		logging.FromContext(ctx).Warn("failed to list GatewayClasses in isConfigForOurClass",
			"error", err)

		return false
	}

	for i := range classes {
		gc := &classes[i]
		if gc.Spec.ParametersRef != nil && gc.Spec.ParametersRef.Name == cfg.Name {
			return true
		}
	}

	return false
}

func (m *ConfigMapper) isSecretReferencedByConfig(ctx context.Context, secret *corev1.Secret) bool {
	if m.isSecretReferencedByCredentials(ctx, secret) {
		return true
	}

	return m.isSecretReferencedByManagedGateway(ctx, secret)
}

// isSecretReferencedByCredentials matches Secrets that back the
// GatewayClassConfig.cloudflareCredentialsSecretRef of any managed
// GatewayClass. This is the original credentials-rotation path.
func (m *ConfigMapper) isSecretReferencedByCredentials(ctx context.Context, secret *corev1.Secret) bool {
	classes, err := listGatewayClassesForController(ctx, m.Client, m.ControllerName)
	if err != nil {
		logging.FromContext(ctx).Warn("failed to list GatewayClasses in isSecretReferencedByCredentials",
			"error", err)

		return false
	}

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

// isSecretReferencedByManagedGateway matches Secrets that back the
// Gateway.spec.tls.backend.clientCertificateRef of any managed Gateway,
// including cross-namespace refs guarded by a matching ReferenceGrant.
// Without this match, a rotation of the backend client cert Secret does
// not enqueue routes and the proxy keeps presenting the previous keypair
// until some unrelated event drives the next reconcile.
func (m *ConfigMapper) isSecretReferencedByManagedGateway(ctx context.Context, secret *corev1.Secret) bool {
	classNames, err := managedClassNames(ctx, m.Client, m.ControllerName)
	if err != nil {
		logging.FromContext(ctx).Warn("failed to list GatewayClasses in isSecretReferencedByManagedGateway",
			"error", err)

		return false
	}

	if len(classNames) == 0 {
		return false
	}

	var gateways gatewayv1.GatewayList
	if listErr := m.Client.List(ctx, &gateways); listErr != nil {
		logging.FromContext(ctx).Warn("failed to list Gateways in isSecretReferencedByManagedGateway",
			"error", listErr)

		return false
	}

	for i := range gateways.Items {
		gateway := &gateways.Items[i]
		if !classNames[string(gateway.Spec.GatewayClassName)] {
			continue
		}

		if m.gatewayReferencesSecret(ctx, gateway, secret) {
			return true
		}
	}

	return false
}

// gatewayReferencesSecret reports whether `gateway`'s
// spec.tls.backend.clientCertificateRef points at `secret`, honoring
// same-namespace refs without a grant check and cross-namespace refs
// guarded by a permitting ReferenceGrant.
//
// Fails open on a transient grant List error: a non-NotFound List
// failure (auth refresh, API-server 5xx, pre-informer warmup) MUST NOT
// drop the Secret rotation event, otherwise the proxy keeps the stale
// keypair until an unrelated event fires. The reconcile path's
// loadGatewayClientCertPEM re-runs the grant check authoritatively, so
// the cost of failing open is one extra reconcile, no security risk.
func (m *ConfigMapper) gatewayReferencesSecret(ctx context.Context, gateway *gatewayv1.Gateway, secret *corev1.Secret) bool {
	ref := gatewayClientCertRef(gateway)
	if ref == nil || !isCoreSecretRef(ref) {
		return false
	}

	targetNS := gateway.Namespace
	if ref.Namespace != nil {
		targetNS = string(*ref.Namespace)
	}

	if targetNS != secret.Namespace || string(ref.Name) != secret.Name {
		return false
	}

	if targetNS == gateway.Namespace {
		return true
	}

	allowed, grantErr := checkSecretReferenceGrantForGateway(ctx, m.Client, gateway, targetNS, *ref)
	if grantErr != nil {
		logging.FromContext(ctx).Warn("transient ReferenceGrant List error during Secret rotation matcher — failing open and enqueueing the Secret event",
			"gateway", gateway.Name,
			"gateway_namespace", gateway.Namespace,
			"secret", secret.Name,
			"secret_namespace", secret.Namespace,
			"error", grantErr)

		return true
	}

	return allowed
}

// SecretMatchesConfig checks if a Secret is referenced by the GatewayClassConfig.
func SecretMatchesConfig(secret *corev1.Secret, cfg *v1alpha1.GatewayClassConfig) bool {
	credRef := cfg.Spec.CloudflareCredentialsSecretRef
	if secret.Name == credRef.Name && (credRef.Namespace == "" || credRef.Namespace == secret.Namespace) {
		return true
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
	// ReferencesService reports whether any backendRef on this route resolves
	// to the Service identified by (namespace, name).
	ReferencesService(namespace, name string) bool
	// ReferencesExternalBackend reports whether any backendRef on this route
	// resolves to the ExternalBackend identified by (namespace, name).
	ReferencesExternalBackend(namespace, name string) bool
}

// FindRoutesForService returns reconcile requests for routes that reference the
// Service object in any of their backendRefs (across all rules). Used by both
// HTTPRoute and GRPCRoute controllers to watch Service changes — a Service
// appProtocol flip (e.g. enabling h2c) must enqueue all referencing routes.
func FindRoutesForService(
	obj client.Object,
	routes []Route,
) []reconcile.Request {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return nil
	}

	var requests []reconcile.Request

	for _, route := range routes {
		if !route.ReferencesService(svc.Namespace, svc.Name) {
			continue
		}

		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      route.GetName(),
				Namespace: route.GetNamespace(),
			},
		})
	}

	return requests
}

// FindRoutesForExternalBackend returns reconcile requests for routes that
// reference the changed ExternalBackend object in any of their backendRefs.
// Editing an ExternalBackend (e.g. repointing its host) must re-sync every
// route that targets it, and creating a previously-missing ExternalBackend must
// clear the route's BackendNotFound condition.
func FindRoutesForExternalBackend(
	obj client.Object,
	routes []Route,
) []reconcile.Request {
	external, ok := obj.(*v1alpha1.ExternalBackend)
	if !ok {
		return nil
	}

	var requests []reconcile.Request

	for _, route := range routes {
		if !route.ReferencesExternalBackend(external.Namespace, external.Name) {
			continue
		}

		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      route.GetName(),
				Namespace: route.GetNamespace(),
			},
		})
	}

	return requests
}

// FindRoutesForEndpointSlice enqueues every route that references the Service
// owning the given EndpointSlice. An endpoint readiness change (a pod going
// Ready/NotReady) updates the EndpointSlice, not the Service, so the proxy's
// zero-ready-endpoint 503 marking would go stale without this watch. The owning
// Service is identified by the standard kubernetes.io/service-name label.
func FindRoutesForEndpointSlice(
	obj client.Object,
	routes []Route,
) []reconcile.Request {
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return nil
	}

	serviceName := slice.Labels[discoveryv1.LabelServiceName]
	if serviceName == "" {
		return nil
	}

	var requests []reconcile.Request

	for _, route := range routes {
		if !route.ReferencesService(slice.Namespace, serviceName) {
			continue
		}

		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{
				Name:      route.GetName(),
				Namespace: route.GetNamespace(),
			},
		})
	}

	return requests
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

// backendRefMatchesService reports whether a Gateway API backendRef points at
// the Service identified by (svcNamespace, svcName). routeNamespace is the
// fallback used when the backendRef omits an explicit namespace.
func backendRefMatchesService(ref *gatewayv1.BackendRef, routeNamespace, svcNamespace, svcName string) bool {
	if !proxy.IsServiceBackendRef(ref.BackendObjectReference) {
		return false
	}

	refNS := routeNamespace
	if ref.Namespace != nil {
		refNS = string(*ref.Namespace)
	}

	return refNS == svcNamespace && string(ref.Name) == svcName
}

// backendRefMatchesExternalBackend reports whether a backendRef points at the
// ExternalBackend identified by (ebNamespace, ebName). routeNamespace is the
// fallback used when the backendRef omits an explicit namespace.
func backendRefMatchesExternalBackend(ref *gatewayv1.BackendRef, routeNamespace, ebNamespace, ebName string) bool {
	if !proxy.IsExternalBackendRef(ref.BackendObjectReference) {
		return false
	}

	refNS := routeNamespace
	if ref.Namespace != nil {
		refNS = string(*ref.Namespace)
	}

	return refNS == ebNamespace && string(ref.Name) == ebName
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

// ReferencesService reports whether this HTTPRoute has any Service backendRef
// matching (namespace, name).
func (w HTTPRouteWrapper) ReferencesService(namespace, name string) bool {
	for ruleIdx := range w.Spec.Rules {
		for refIdx := range w.Spec.Rules[ruleIdx].BackendRefs {
			if backendRefMatchesService(&w.Spec.Rules[ruleIdx].BackendRefs[refIdx].BackendRef, w.Namespace, namespace, name) {
				return true
			}
		}
	}

	return false
}

// ReferencesExternalBackend reports whether this HTTPRoute has any
// ExternalBackend backendRef matching (namespace, name).
func (w HTTPRouteWrapper) ReferencesExternalBackend(namespace, name string) bool {
	for ruleIdx := range w.Spec.Rules {
		for refIdx := range w.Spec.Rules[ruleIdx].BackendRefs {
			if backendRefMatchesExternalBackend(&w.Spec.Rules[ruleIdx].BackendRefs[refIdx].BackendRef, w.Namespace, namespace, name) {
				return true
			}
		}
	}

	return false
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

// ReferencesService reports whether this GRPCRoute has any Service backendRef
// matching (namespace, name).
func (w GRPCRouteWrapper) ReferencesService(namespace, name string) bool {
	for ruleIdx := range w.Spec.Rules {
		for refIdx := range w.Spec.Rules[ruleIdx].BackendRefs {
			if backendRefMatchesService(&w.Spec.Rules[ruleIdx].BackendRefs[refIdx].BackendRef, w.Namespace, namespace, name) {
				return true
			}
		}
	}

	return false
}

// ReferencesExternalBackend reports whether this GRPCRoute has any
// ExternalBackend backendRef matching (namespace, name).
func (w GRPCRouteWrapper) ReferencesExternalBackend(namespace, name string) bool {
	for ruleIdx := range w.Spec.Rules {
		for refIdx := range w.Spec.Rules[ruleIdx].BackendRefs {
			if backendRefMatchesExternalBackend(&w.Spec.Rules[ruleIdx].BackendRefs[refIdx].BackendRef, w.Namespace, namespace, name) {
				return true
			}
		}
	}

	return false
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
	views *listenerViewCache,
) []reconcile.Request {
	views = views.orNew(cli)

	var requests []reconcile.Request

	for _, route := range routes {
		if IsRouteAcceptedByGateway(ctx, cli, validator, controllerName, route, views) {
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
		gateway, found := resolveParentGatewayFromRef(ctx, cli, ref, route.GetNamespace())
		if !found {
			continue
		}

		if isGatewayManagedByController(ctx, cli, gateway, controllerName) {
			return true
		}
	}

	return false
}

// resolveParentGatewayFromRef returns the Gateway selected by a route's
// parentRef. The ref may target the Gateway directly (Kind=Gateway) or via a
// ListenerSet (Kind=ListenerSet), in which case the ListenerSet's
// spec.parentRef is followed to the Gateway. Returns (nil, false) when the
// ref's Group is foreign to the Gateway API, the Kind is anything other than
// Gateway/ListenerSet, or the named resource cannot be loaded.
func resolveParentGatewayFromRef(
	ctx context.Context,
	cli client.Client,
	ref gatewayv1.ParentReference,
	routeNamespace string,
) (*gatewayv1.Gateway, bool) {
	if ref.Group != nil && string(*ref.Group) != "" && string(*ref.Group) != gatewayv1.GroupName {
		return nil, false
	}

	kind := kindGateway
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}

	namespace := routeNamespace
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}

	switch kind {
	case kindGateway:
		var gateway gatewayv1.Gateway
		if err := cli.Get(ctx, client.ObjectKey{Name: string(ref.Name), Namespace: namespace}, &gateway); err != nil {
			return nil, false
		}

		return &gateway, true
	case kindListenerSet:
		var listenerSet gatewayv1.ListenerSet
		if err := cli.Get(ctx, client.ObjectKey{Name: string(ref.Name), Namespace: namespace}, &listenerSet); err != nil {
			return nil, false
		}

		return listenerSetParentGateway(ctx, cli, &listenerSet)
	}

	return nil, false
}

// IsRouteAcceptedByGateway checks if a route has at least one accepted binding
// to a Gateway managed by the given controllerName, either directly or via
// an attached ListenerSet. Used by both HTTPRoute and GRPCRoute controllers
// to decide whether a route should be processed.
func IsRouteAcceptedByGateway(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	controllerName string,
	route Route,
	views *listenerViewCache,
) bool {
	views = views.orNew(cli)

	routeInfoTemplate := &routebinding.RouteInfo{
		Name:      route.GetName(),
		Namespace: route.GetNamespace(),
		Hostnames: route.GetHostnames(),
		Kind:      route.GetRouteKind(),
	}

	for _, ref := range route.GetParentRefs() {
		binding, err := resolveRouteParentBinding(ctx, cli, validator, controllerName, ref, route.GetNamespace(), withRefFilters(routeInfoTemplate, ref), views)
		if err != nil {
			logging.FromContext(ctx).Error("failed to validate route binding",
				"route", route.GetNamespace()+"/"+route.GetName(),
				"error", err)

			continue
		}

		if binding.ManagedByThisController && binding.Result.Accepted {
			return true
		}
	}

	return false
}

// withRefFilters returns a shallow copy of the RouteInfo template with the
// per-ref SectionName/Port filters applied. Avoids mutating the template
// across multiple parentRefs.
func withRefFilters(template *routebinding.RouteInfo, ref gatewayv1.ParentReference) *routebinding.RouteInfo {
	clone := *template
	clone.SectionName = ref.SectionName
	clone.Port = ref.Port

	return &clone
}
