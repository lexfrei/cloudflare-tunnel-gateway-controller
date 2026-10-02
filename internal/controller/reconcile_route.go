package controller

import (
	"context"
	"maps"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
)

// reconcileRouteParams holds common parameters for the generic route reconcile loop.
type reconcileRouteParams[T client.Object] struct {
	startupComplete *atomic.Bool
	k8sClient       client.Client
	controllerName  string
	componentName   string
	wrapRoute       func(T) Route
	newAccessor     func() routeAccessor
	syncAndUpdate   func(ctx context.Context) (ctrl.Result, error)
}

// reconcileRoute is the generic Reconcile implementation shared by
// HTTPRouteReconciler and GRPCRouteReconciler. It eliminates duplication
// between the two controllers that follow an identical reconcile pattern:
// wait for startup → get route → check ownership → sync, or release this
// controller's stale status entries when the route is not ours.
func reconcileRoute[T client.Object](
	ctx context.Context,
	req ctrl.Request,
	route T,
	params reconcileRouteParams[T],
) (ctrl.Result, error) {
	if !params.startupComplete.Load() {
		return ctrl.Result{RequeueAfter: startupPendingRequeueDelay, Priority: new(priorityRoute)}, nil
	}

	ctx = logging.WithReconcileID(ctx)

	logger := logging.Component(ctx, params.componentName+"-reconciler").With(params.componentName, req.String())
	ctx = logging.WithLogger(ctx, logger)

	if err := params.k8sClient.Get(ctx, req.NamespacedName, route); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info(params.componentName + " deleted, triggering full sync")

			return params.syncAndUpdate(ctx)
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to get "+params.componentName)
	}

	wrapped := params.wrapRoute(route)
	if !routeReferencesOurGateways(ctx, params.k8sClient, params.controllerName, wrapped) {
		return ctrl.Result{}, releaseOwnParentStatus(ctx, params.k8sClient, params.controllerName,
			req.NamespacedName, params.newAccessor)
	}

	logger.Info("reconciling " + params.componentName)

	return params.syncAndUpdate(ctx)
}

// routeControllerSetupParams holds parameters for setting up a route controller
// with standard watches (Gateway, GatewayClass, GatewayClassConfig, Secret,
// ReferenceGrant).
type routeControllerSetupParams struct {
	routeObject              client.Object
	reconciler               reconcile.Reconciler
	runnable                 manager.Runnable
	k8sClient                client.Client
	controllerName           string
	configResolver           *config.Resolver
	findRoutesForGateway     handler.MapFunc
	findRoutesForListenerSet handler.MapFunc
	findRoutesForRefGrant    handler.MapFunc
	findRoutesForService     handler.MapFunc
	// findRoutesForEndpointSlice enqueues routes referencing the Service that
	// owns a changed EndpointSlice, so the proxy's zero-ready-endpoint 503
	// marking refreshes when pods go Ready/NotReady. Gated like the Service
	// watch: nil means no EndpointSlice watch is registered.
	findRoutesForEndpointSlice handler.MapFunc
	// findRoutesForExternalBackend enqueues routes referencing a changed
	// ExternalBackend, so editing or creating one re-syncs the proxy config and
	// clears a route's BackendNotFound condition. nil means no watch.
	findRoutesForExternalBackend handler.MapFunc
	// watchBackendTLS adds the BackendTLSPolicy + CA ConfigMap watches. Both
	// HTTPRoute and GRPCRoute now honor BackendTLSPolicy (gRPC backends are
	// upgraded to TLS + ALPN-negotiated HTTP/2 when a policy targets the
	// Service), so both reconcilers flip this flag on. The Service watch is
	// gated separately on findRoutesForService.
	watchBackendTLS bool
	// watchNamespaceLabels adds a Namespace watch keyed on LABEL changes:
	// hostname-ownership binds a namespace to its allowed suffix via a label,
	// and relabelling must re-converge the namespace's routes both ways
	// (revocation rejects programmed violators, a granted label clears
	// HostnameNotPermitted). Label edits do not bump generation, which is why
	// this watch carries its own predicate. Off when ownership enforcement is
	// disabled — namespace labels then influence nothing.
	watchNamespaceLabels bool
	getAllRelevantRoutes RequestsFunc
}

// namespaceScopedRequests narrows getAllRelevantRoutes to the routes of the
// event namespace. Any single enqueued route triggers a full sync (which
// re-evaluates ownership for every route), so the namespace's own routes are
// exactly the right granularity — they are also the ones whose statuses must
// change.
func namespaceScopedRequests(getAll RequestsFunc) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var scoped []reconcile.Request

		for _, req := range getAll(ctx) {
			if req.Namespace == obj.GetName() {
				scoped = append(scoped, req)
			}
		}

		return scoped
	}
}

// routeWatch is one secondary watch of a route controller: the watched type,
// the handler that maps its events to route requests, and the predicates an
// event must pass first. Kept as data so tests can drive events through
// exactly what setupRouteController registers.
type routeWatch struct {
	object     client.Object
	handler    handler.EventHandler
	predicates []predicate.Predicate
}

// setupRouteController sets up the controller-runtime builder with standard
// watches shared between HTTPRoute and GRPCRoute controllers.
func setupRouteController(mgr ctrl.Manager, params *routeControllerSetupParams) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(params.routeObject, ctrlbuilder.WithPredicates(predicate.GenerationChangedPredicate{}))

	for _, watch := range routeWatches(params) {
		builder = builder.Watches(watch.object, watch.handler, ctrlbuilder.WithPredicates(watch.predicates...))
	}

	err := builder.Complete(params.reconciler)
	if err != nil {
		return errors.Wrap(err, "failed to setup route controller")
	}

	if err := mgr.Add(params.runnable); err != nil {
		return errors.Wrap(err, "failed to add startup sync runnable")
	}

	return nil
}

// routeWatches returns every secondary watch of a route controller.
func routeWatches(params *routeControllerSetupParams) []routeWatch {
	mapper := &ConfigMapper{
		Client:         params.k8sClient,
		ControllerName: params.controllerName,
		ConfigResolver: params.configResolver,
	}

	// The generation predicate is applied PER WATCH, and only to kinds whose
	// spec edits bump metadata.generation. Secret, Service, ConfigMap and
	// Namespace never carry one, so the gate would drop every update to them;
	// their watches run unfiltered or on a predicate of their own.
	generationChanged := []predicate.Predicate{predicate.GenerationChangedPredicate{}}

	watches := []routeWatch{
		{
			object:     &gatewayv1.Gateway{},
			handler:    handler.EnqueueRequestsFromMapFunc(params.findRoutesForGateway),
			predicates: generationChanged,
		},
		{
			// A Gateway whose tunnel claim stops being refused, or whose data
			// plane goes away, changes only its conditions. Its routes must
			// re-sync to follow, and the full relevant set is enqueued rather
			// than the Gateway's direct routes: a Gateway whose routes all
			// attach through a ListenerSet has none.
			object: &gatewayv1.Gateway{},
			handler: handler.EnqueueRequestsFromMapFunc(
				managedGatewayRoutes(params.k8sClient, params.controllerName, params.getAllRelevantRoutes)),
			predicates: []predicate.Predicate{gatewayVerdictChanged()},
		},
		{
			// Acceptance keys on the class's controllerName, so a class of
			// ours appearing, going away or changing parametersRef moves
			// routes in or out without any route or Gateway event.
			object:     &gatewayv1.GatewayClass{},
			handler:    handler.EnqueueRequestsFromMapFunc(ownClassRoutes(params.controllerName, params.getAllRelevantRoutes)),
			predicates: generationChanged,
		},
		{
			object:     &gatewayv1.ListenerSet{},
			handler:    handler.EnqueueRequestsFromMapFunc(params.findRoutesForListenerSet),
			predicates: generationChanged,
		},
		{
			object:     &v1alpha1.GatewayClassConfig{},
			handler:    handler.EnqueueRequestsFromMapFunc(mapper.MapConfigToRequests(params.getAllRelevantRoutes)),
			predicates: generationChanged,
		},
		{
			object:  &corev1.Secret{},
			handler: handler.EnqueueRequestsFromMapFunc(mapper.MapSecretToRequests(params.getAllRelevantRoutes)),
		},
		{
			object:     &gatewayv1beta1.ReferenceGrant{},
			handler:    handler.EnqueueRequestsFromMapFunc(params.findRoutesForRefGrant),
			predicates: generationChanged,
		},
	}

	if params.watchNamespaceLabels {
		watches = append(watches, routeWatch{
			object:     &corev1.Namespace{},
			handler:    handler.EnqueueRequestsFromMapFunc(namespaceScopedRequests(params.getAllRelevantRoutes)),
			predicates: []predicate.Predicate{predicate.LabelChangedPredicate{}},
		})
	}

	return append(watches, proxyOnlyWatches(params)...)
}

// gatewayVerdictChanged passes only a Gateway update whose top-level
// conditions changed type, status or reason. Message, timestamps and listener
// status are ignored: the Gateway reconciler rewrites the listeners'
// attachedRoutes count whenever routes change, and passing that would turn
// each rewrite into another full route sync. Create, delete and generic
// events are left to the generation-gated Gateway watch.
func gatewayVerdictChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(updateEvent event.UpdateEvent) bool {
			oldGateway, oldOK := updateEvent.ObjectOld.(*gatewayv1.Gateway)
			newGateway, newOK := updateEvent.ObjectNew.(*gatewayv1.Gateway)

			if !oldOK || !newOK {
				return false
			}

			return !maps.Equal(conditionVerdicts(oldGateway.Status.Conditions),
				conditionVerdicts(newGateway.Status.Conditions))
		},
	}
}

// conditionVerdicts keys each condition's status and reason by its type.
func conditionVerdicts(conditions []metav1.Condition) map[string]string {
	verdicts := make(map[string]string, len(conditions))
	for _, condition := range conditions {
		verdicts[condition.Type] = string(condition.Status) + "/" + condition.Reason
	}

	return verdicts
}

// managedGatewayRoutes enqueues the relevant routes when a Gateway of one of
// this controller's classes changes; any one of them runs the full sync.
func managedGatewayRoutes(cli client.Client, controllerName string, getAll RequestsFunc) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		gateway, ok := obj.(*gatewayv1.Gateway)
		if !ok || !isGatewayManagedByController(ctx, cli, gateway, controllerName) {
			return nil
		}

		return getAll(ctx)
	}
}

// ownClassRoutes enqueues the relevant routes when a GatewayClass carrying
// this controller's name changes; any one of them runs the full sync. A
// deleted class is matched on its last state, but its own routes are no
// longer relevant by then: the sync runs only if another managed class still
// has an accepted route, and nothing is enqueued otherwise.
func ownClassRoutes(controllerName string, getAll RequestsFunc) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		class, ok := obj.(*gatewayv1.GatewayClass)
		if !ok || string(class.Spec.ControllerName) != controllerName {
			return nil
		}

		return getAll(ctx)
	}
}

// proxyOnlyWatches returns the watches the proxy-driving route controllers
// need. Both HTTPRoute and GRPCRoute watch Service so a route stuck at 500
// because its backend did not exist yet recovers when the Service appears
// (gated on findRoutesForService). Both also watch BackendTLSPolicy and the
// CA ConfigMap (gated on watchBackendTLS) now that gRPC backends honor a
// matching policy by upgrading to TLS + ALPN-negotiated HTTP/2.
func proxyOnlyWatches(params *routeControllerSetupParams) []routeWatch {
	// Generation-gated only where generation moves, as in routeWatches.
	generationChanged := []predicate.Predicate{predicate.GenerationChangedPredicate{}}

	var watches []routeWatch

	if params.findRoutesForService != nil {
		watches = append(watches, routeWatch{
			object:  &corev1.Service{},
			handler: handler.EnqueueRequestsFromMapFunc(params.findRoutesForService),
		})
	}

	if params.findRoutesForEndpointSlice != nil {
		watches = append(watches, routeWatch{
			object:     &discoveryv1.EndpointSlice{},
			handler:    handler.EnqueueRequestsFromMapFunc(params.findRoutesForEndpointSlice),
			predicates: generationChanged,
		})
	}

	if params.findRoutesForExternalBackend != nil {
		watches = append(watches, routeWatch{
			object:     &v1alpha1.ExternalBackend{},
			handler:    handler.EnqueueRequestsFromMapFunc(params.findRoutesForExternalBackend),
			predicates: generationChanged,
		})
	}

	if !params.watchBackendTLS {
		return watches
	}

	enqueueAllRoutes := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, _ client.Object) []reconcile.Request {
			return params.getAllRelevantRoutes(ctx)
		},
	)
	enqueueRoutesForCAConfigMap := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			configMap, ok := obj.(*corev1.ConfigMap)
			if !ok {
				return nil
			}

			if !isConfigMapReferencedByBackendTLSPolicy(ctx, params.k8sClient, configMap) {
				return nil
			}

			return params.getAllRelevantRoutes(ctx)
		},
	)

	return append(watches,
		routeWatch{object: &gatewayv1.BackendTLSPolicy{}, handler: enqueueAllRoutes, predicates: generationChanged},
		routeWatch{object: &corev1.ConfigMap{}, handler: enqueueRoutesForCAConfigMap},
	)
}
