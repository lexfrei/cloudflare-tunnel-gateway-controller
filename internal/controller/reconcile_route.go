package controller

import (
	"context"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	syncAndUpdate   func(ctx context.Context) (ctrl.Result, error)
}

// reconcileRoute is the generic Reconcile implementation shared by
// HTTPRouteReconciler and GRPCRouteReconciler. It eliminates duplication
// between the two controllers that follow an identical reconcile pattern:
// wait for startup → get route → check ownership → sync.
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
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling " + params.componentName)

	return params.syncAndUpdate(ctx)
}

// routeControllerSetupParams holds parameters for setting up a route controller
// with standard watches (Gateway, GatewayClassConfig, Secret, ReferenceGrant).
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

// setupRouteController sets up the controller-runtime builder with standard
// watches shared between HTTPRoute and GRPCRoute controllers.
func setupRouteController(mgr ctrl.Manager, params *routeControllerSetupParams) error {
	mapper := &ConfigMapper{
		Client:         params.k8sClient,
		ControllerName: params.controllerName,
		ConfigResolver: params.configResolver,
	}

	// The generation predicate is applied PER WATCH (replicating the former
	// global WithEventFilter verbatim), not globally: the Namespace watch
	// below must see label-only updates, which a global generation filter
	// would eat — namespace label edits do not bump generation.
	generationChanged := ctrlbuilder.WithPredicates(predicate.GenerationChangedPredicate{})

	builder := ctrl.NewControllerManagedBy(mgr).
		For(params.routeObject, generationChanged).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(params.findRoutesForGateway),
			generationChanged,
		).
		Watches(
			&gatewayv1.ListenerSet{},
			handler.EnqueueRequestsFromMapFunc(params.findRoutesForListenerSet),
			generationChanged,
		).
		Watches(
			&v1alpha1.GatewayClassConfig{},
			handler.EnqueueRequestsFromMapFunc(mapper.MapConfigToRequests(params.getAllRelevantRoutes)),
			generationChanged,
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(mapper.MapSecretToRequests(params.getAllRelevantRoutes)),
			generationChanged,
		).
		Watches(
			&gatewayv1beta1.ReferenceGrant{},
			handler.EnqueueRequestsFromMapFunc(params.findRoutesForRefGrant),
			generationChanged,
		)

	if params.watchNamespaceLabels {
		builder = builder.Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(namespaceScopedRequests(params.getAllRelevantRoutes)),
			ctrlbuilder.WithPredicates(predicate.LabelChangedPredicate{}),
		)
	}

	builder = addProxyOnlyWatches(builder, params)

	err := builder.Complete(params.reconciler)
	if err != nil {
		return errors.Wrap(err, "failed to setup route controller")
	}

	if err := mgr.Add(params.runnable); err != nil {
		return errors.Wrap(err, "failed to add startup sync runnable")
	}

	return nil
}

// addProxyOnlyWatches adds the watches the proxy-driving route controllers
// need. Both HTTPRoute and GRPCRoute watch Service so a route stuck at 500
// because its backend did not exist yet recovers when the Service appears
// (gated on findRoutesForService). Both also watch BackendTLSPolicy and the
// CA ConfigMap (gated on watchBackendTLS) now that gRPC backends honor a
// matching policy by upgrading to TLS + ALPN-negotiated HTTP/2. Extracted
// from setupRouteController to keep its function length within the linter
// budget.
func addProxyOnlyWatches(
	builder *ctrl.Builder,
	params *routeControllerSetupParams,
) *ctrl.Builder {
	// Same per-watch replication of the former global generation filter as in
	// setupRouteController — these watches keep their historic event surface.
	generationChanged := ctrlbuilder.WithPredicates(predicate.GenerationChangedPredicate{})

	if params.findRoutesForService != nil {
		builder = builder.Watches(
			&corev1.Service{},
			handler.EnqueueRequestsFromMapFunc(params.findRoutesForService),
			generationChanged,
		)
	}

	if params.findRoutesForEndpointSlice != nil {
		builder = builder.Watches(
			&discoveryv1.EndpointSlice{},
			handler.EnqueueRequestsFromMapFunc(params.findRoutesForEndpointSlice),
			generationChanged,
		)
	}

	if params.findRoutesForExternalBackend != nil {
		builder = builder.Watches(
			&v1alpha1.ExternalBackend{},
			handler.EnqueueRequestsFromMapFunc(params.findRoutesForExternalBackend),
			generationChanged,
		)
	}

	if !params.watchBackendTLS {
		return builder
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

	return builder.
		Watches(&gatewayv1.BackendTLSPolicy{}, enqueueAllRoutes, generationChanged).
		Watches(&corev1.ConfigMap{}, enqueueRoutesForCAConfigMap, generationChanged)
}
