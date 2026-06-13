package controller

import (
	"context"
	"encoding/pem"
	"fmt"
	"math"
	"time"

	"github.com/cockroachdb/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

const (
	cfArgotunnelSuffix = ".cfargotunnel.com"

	// Shared per-listener condition messages used by both Gateway and
	// ListenerSet listener status writers.
	listenerMsgAccepted              = "Listener accepted"
	listenerMsgProgrammed            = "Listener programmed"
	listenerMsgInvalidUnresolved     = "Listener has unresolved references"
	listenerMsgNoSupportedRouteKinds = "None of the specified route kinds are supported"
	listenerMsgInvalidRouteKinds     = "One or more specified route kinds are not supported"

	// configErrorRequeueDelay is the delay before retrying when config resolution fails.
	configErrorRequeueDelay = 30 * time.Second

	// msgReferencesResolved is the standard message for ResolvedRefs condition.
	msgReferencesResolved = "References resolved"

	// msgGatewayAccepted is the standard message for Accepted/Programmed conditions
	// on Gateways managed by this controller.
	msgGatewayAccepted = "Gateway accepted by cloudflare-tunnel controller"

	// kindSecret is the resource kind for Kubernetes Secrets.
	kindSecret = "Secret"

	// maxConditionMessageLength is the maximum length for condition messages.
	// Used by truncateMessage to cap status condition messages.
	maxConditionMessageLength = 256

	// legacyCloudflaredFinalizer is the finalizer that the v2 controller
	// attached to every Gateway it reconciled while it owned the cloudflared
	// deployment lifecycle. v3 never adds it (the chart owns proxy lifecycle
	// now), but Gateways that existed before the v3 upgrade still carry it,
	// and without explicit cleanup they would hang forever in Terminating
	// when deleted. The deletion branch strips it on first reconcile.
	legacyCloudflaredFinalizer = "cloudflare-tunnel.gateway.networking.k8s.io/cloudflared"
)

// clampedInt32Pointer turns the count of attached ListenerSets into the
// pointer the Gateway status field expects, clamping above MaxInt32 so the
// status field can never overflow when an unexpectedly large list slips
// through CRD validation.
func clampedInt32Pointer(count int) *int32 {
	clamped := min(count, math.MaxInt32)
	val := int32(clamped) //nolint:gosec // clamped to MaxInt32 above

	return &val
}

// truncateMessage truncates a message to maxConditionMessageLength.
func truncateMessage(msg string) string {
	if len(msg) > maxConditionMessageLength {
		return msg[:maxConditionMessageLength-3] + "..."
	}

	return msg
}

// GatewayReconciler reconciles Gateway resources for the cloudflare-tunnel GatewayClass.
//
// It performs the following functions:
//   - Watches Gateway resources whose GatewayClass matches the configured ControllerName
//   - Reads configuration from GatewayClassConfig via parametersRef
//   - Updates Gateway status with tunnel CNAME address (for external-dns integration)
//
// Starting v3 the controller no longer manages a separate cloudflared deployment;
// the in-process L7 proxy embeds cloudflared transport and is deployed alongside
// the controller by the Helm chart. The controller only reconciles status.
type GatewayReconciler struct {
	client.Client

	// Scheme is the runtime scheme for API type registration.
	Scheme *runtime.Scheme

	// ControllerName identifies this controller. Per Gateway API spec,
	// controllerName is the binding mechanism between GatewayClass and controller.
	// The controller watches all GatewayClasses with matching controllerName.
	ControllerName string

	// ConfigResolver resolves configuration from GatewayClassConfig.
	ConfigResolver *config.Resolver

	// ViewStore caches the per-Gateway ListenerSet merge view across reconciles.
	// Shared with the route and ListenerSet reconcilers (issue #332). May be nil.
	ViewStore *mergeViewStore
}

func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var gateway gatewayv1.Gateway

	if err := r.Get(ctx, req.NamespacedName, &gateway); err != nil {
		if apierrors.IsNotFound(err) {
			// Gateway is gone: drop its cached merge view so the shared store
			// does not retain entries for deleted Gateways (issue #332).
			r.ViewStore.forget(req.NamespacedName)

			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to get gateway")
	}

	// Legacy-finalizer strip runs BEFORE every other check. The finalizer
	// name is unique to this controller's v2 incarnation so the strip is
	// unambiguous even when:
	//   - the GatewayClass has been deleted (typical v2 -> v3 cleanup order:
	//     operator uninstalls the v2 Helm release first, then drains Gateways);
	//   - the controller no longer owns the Gateway's GatewayClass (someone
	//     repointed parametersRef);
	//   - the GatewayClassConfig or credentials Secret is missing.
	// Without this early strip the Gateway would hang in Terminating forever,
	// contradicting the migration guide's "automatic on delete" promise.
	if !gateway.DeletionTimestamp.IsZero() &&
		controllerutil.ContainsFinalizer(&gateway, legacyCloudflaredFinalizer) {
		controllerutil.RemoveFinalizer(&gateway, legacyCloudflaredFinalizer)

		if err := r.Update(ctx, &gateway); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to remove legacy cloudflared finalizer")
		}

		return ctrl.Result{}, nil
	}

	if !isGatewayManagedByController(ctx, r.Client, &gateway, r.ControllerName) {
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling gateway", "name", gateway.Name, "namespace", gateway.Namespace)

	// Deletion path for v3-managed Gateways without a legacy finalizer: nothing
	// to do (proxy lifecycle is managed by the Helm chart, not per-Gateway).
	// The legacy-finalizer strip above already returned for v2-tagged Gateways.
	if !gateway.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	resolvedConfig, perGatewayMode, err := r.resolveGatewayConfig(ctx, &gateway)
	if err != nil {
		logger.Error(err, "failed to resolve gateway configuration")

		// The per-Gateway resolver classifies only deterministic spec problems
		// as ErrInvalidParameters; anything else on an opted-in Gateway is a
		// transient API failure that says nothing about the spec. Stamping
		// InvalidParameters over it would misreport a healthy Gateway and
		// clear its listener statuses on every cache hiccup — propagate for
		// backoff instead and leave the last written status standing. The
		// class chain (no parametersRef) keeps its historic
		// stamp-on-any-error behavior.
		if config.HasInfrastructureParametersRef(&gateway) && !errors.Is(err, config.ErrInvalidParameters) {
			return ctrl.Result{}, err
		}

		if statusErr := r.setConfigErrorStatus(ctx, &gateway, err); statusErr != nil {
			logger.Error(statusErr, "failed to update gateway status")
		}

		return ctrl.Result{RequeueAfter: configErrorRequeueDelay, Priority: new(priorityGateway)}, nil
	}

	if err := r.updateStatus(ctx, &gateway, resolvedConfig, perGatewayMode); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to update gateway status")
	}

	return ctrl.Result{}, nil
}

// resolveGatewayConfig resolves the Gateway's effective configuration: the
// per-Gateway data plane (infrastructure.parametersRef → tunnel identity from
// the connector token) when opted in, the GatewayClass chain otherwise. The
// bool reports per-Gateway mode so the status writer gates Programmed on the
// rendered Deployment. An invalid parametersRef errors with
// config.ErrInvalidParameters, surfacing as Accepted=False/InvalidParameters.
func (r *GatewayReconciler) resolveGatewayConfig(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) (*config.ResolvedConfig, bool, error) {
	perGateway, err := r.ConfigResolver.ResolveForGateway(ctx, gateway)
	if err != nil {
		return nil, false, errors.Wrap(err, "per-gateway configuration")
	}

	if perGateway != nil {
		return &perGateway.ResolvedConfig, true, nil
	}

	classConfig, err := r.ConfigResolver.ResolveFromGatewayClassName(ctx, string(gateway.Spec.GatewayClassName))
	if err != nil {
		return nil, false, errors.Wrap(err, "GatewayClass configuration")
	}

	return classConfig, false, nil
}

//nolint:funlen // status update logic with retry
func (r *GatewayReconciler) updateStatus(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	cfg *config.ResolvedConfig,
	perGatewayMode bool,
) error {
	gatewayKey := types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the gateway to avoid conflict errors
		var freshGateway gatewayv1.Gateway
		if err := r.Get(ctx, gatewayKey, &freshGateway); err != nil {
			return errors.Wrap(err, "failed to get fresh gateway")
		}

		// Skip if a newer reconcile already advanced this Gateway's status past
		// the generation we observed; Gateway API forbids regressing
		// observedGeneration, and that reconcile will write the current view.
		if gatewayStatusStale(gateway.Generation, &freshGateway) {
			return nil
		}

		now := metav1.Now()

		attachedRoutes := r.countAttachedRoutes(ctx, &freshGateway)

		views := newListenerViewCache(r.Client, r.ViewStore)

		attachedCount, mergeErr := summariseAttachedListenerSets(ctx, r.Client, &freshGateway, views)
		if mergeErr != nil {
			log.FromContext(ctx).Error(mergeErr, "failed to summarise attached listenersets; reporting 0")

			attachedCount = 0
		}

		freshGateway.Status.AttachedListenerSets = clampedInt32Pointer(attachedCount)

		freshGateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{
			{
				Type:  new(gatewayv1.HostnameAddressType),
				Value: cfg.TunnelID + cfArgotunnelSuffix,
			},
		}

		_, _, clientCertErr := loadGatewayClientCertPEM(ctx, r.Client, &freshGateway, r.checkSecretReferenceGrant)

		accepted := metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionAccepted),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: freshGateway.Generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.GatewayReasonAccepted),
			Message:            msgGatewayAccepted,
		}

		// A Gateway that contains conflicted listeners (its own listeners or
		// merged ListenerSet entries that clash on hostname/protocol) MUST be
		// marked ListenersNotValid per the spec, whether or not the listeners
		// accept the Gateway (gateway_types.go:187).
		if conflictMsg, conflicted := gatewayConflictedListenersMessage(ctx, views, &freshGateway); conflicted {
			accepted.Status = metav1.ConditionFalse
			accepted.Reason = string(gatewayv1.GatewayReasonListenersNotValid)
			accepted.Message = conflictMsg
		}

		programmed := metav1.Condition{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: freshGateway.Generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.GatewayReasonProgrammed),
			Message:            "Gateway programmed in Cloudflare Tunnel",
		}

		// A dedicated data plane is only "programmed" once it can actually
		// carry traffic: the rendered proxy Deployment needs at least one
		// ready replica (= a registered tunnel connector). The shared plane
		// keeps the historic semantics (chart-managed proxy, always present).
		if perGatewayMode {
			programmed = r.perGatewayProgrammedCondition(ctx, &freshGateway, now)
		}

		applyGatewayConditions(&freshGateway.Status.Conditions, []metav1.Condition{
			accepted,
			programmed,
		}, buildClientCertResolvedRefsCondition(freshGateway.Generation, now, clientCertErr))

		listenerStatuses := make([]gatewayv1.ListenerStatus, 0, len(freshGateway.Spec.Listeners))

		// The merged view (cached) annotates each Gateway-owned listener that
		// conflicts with a higher-precedence one, used below to emit the
		// per-listener Conflicted condition.
		gwView, _ := views.forGateway(ctx, &freshGateway)

		for i := range freshGateway.Spec.Listeners {
			listener := &freshGateway.Spec.Listeners[i]

			// Validate route kinds - filter to only supported kinds
			supportedKinds, hasValidKind, hasInvalidKind := routebinding.FilterSupportedKinds(
				listener.AllowedRoutes,
				listener.Protocol,
			)

			// Validate TLS certificate refs (if applicable)
			tlsStatus, tlsReason, tlsMessage := r.validateTLSCertificateRefs(
				ctx, &freshGateway, listener,
			)

			// Determine final ResolvedRefs condition
			resolvedRefsCondition := r.buildResolvedRefsCondition(
				freshGateway.Generation, now, hasValidKind, hasInvalidKind, tlsStatus, tlsReason, tlsMessage,
			)
			if !hasValidKind {
				supportedKinds = []gatewayv1.RouteGroupKind{} // Empty slice (not nil) when no valid kinds
			}

			programmedCondition := metav1.Condition{
				Type:               string(gatewayv1.ListenerConditionProgrammed),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: freshGateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.ListenerReasonProgrammed),
				Message:            listenerMsgProgrammed,
			}

			if resolvedRefsCondition.Status == metav1.ConditionFalse {
				programmedCondition.Status = metav1.ConditionFalse
				programmedCondition.Reason = string(gatewayv1.ListenerReasonInvalid)
				programmedCondition.Message = listenerMsgInvalidUnresolved
			}

			acceptedCondition := buildListenerAcceptedCondition(listener.Protocol, freshGateway.Generation, now)

			// A listener this controller cannot serve at all (unsupported
			// protocol) is not programmed either.
			if acceptedCondition.Status == metav1.ConditionFalse {
				programmedCondition.Status = metav1.ConditionFalse
				programmedCondition.Reason = string(gatewayv1.ListenerReasonInvalid)
				programmedCondition.Message = acceptedCondition.Message
			}

			conditions := []metav1.Condition{acceptedCondition, programmedCondition, resolvedRefsCondition}

			// A Gateway-owned listener that conflicts with a higher-precedence
			// listener MUST carry Conflicted=True and is neither Accepted nor
			// Programmed (gateway_types.go:168-170).
			if conflicted := conflictedGatewayListenerConditions(
				gwView, listener.Name, freshGateway.Generation, now, &resolvedRefsCondition,
			); conflicted != nil {
				conditions = conflicted
			}

			listenerStatuses = append(listenerStatuses, gatewayv1.ListenerStatus{
				Name:           listener.Name,
				SupportedKinds: supportedKinds,
				AttachedRoutes: attachedRoutes[listener.Name],
				Conditions:     conditions,
			})
		}

		freshGateway.Status.Listeners = listenerStatuses

		if err := r.Status().Update(ctx, &freshGateway); err != nil {
			return errors.Wrap(err, "failed to update gateway status")
		}

		return nil
	})

	return errors.Wrap(err, "failed to update gateway status after retries")
}

// perGatewayProgrammedCondition derives Programmed for a Gateway with a
// dedicated data plane from its rendered proxy Deployment: at least one ready
// replica means a tunnel connector is registered and traffic can flow.
func (r *GatewayReconciler) perGatewayProgrammedCondition(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	now metav1.Time,
) metav1.Condition {
	condition := metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionProgrammed),
		Status:             metav1.ConditionFalse,
		ObservedGeneration: gateway.Generation,
		LastTransitionTime: now,
		Reason:             string(gatewayv1.GatewayReasonPending),
	}

	var deployment appsv1.Deployment

	err := r.Get(ctx, types.NamespacedName{
		Name: render.DeploymentName(gateway), Namespace: gateway.Namespace,
	}, &deployment)

	switch {
	case apierrors.IsNotFound(err):
		condition.Message = "Per-Gateway proxy deployment not yet created"
	case err != nil:
		condition.Message = truncateMessage("Failed to read per-Gateway proxy deployment: " + err.Error())
	case deployment.Status.ReadyReplicas >= 1:
		condition.Status = metav1.ConditionTrue
		condition.Reason = string(gatewayv1.GatewayReasonProgrammed)
		condition.Message = "Per-Gateway proxy deployment has ready replicas"
	default:
		condition.Message = "Per-Gateway proxy deployment has no ready replicas yet"
	}

	return condition
}

// gatewayStatusStale reports whether the freshly-fetched Gateway already
// carries status conditions (top-level or per-listener) stamped with a
// generation newer than reconciledGen, in which case this reconcile MUST NOT
// overwrite the status (Gateway API observedGeneration regression guard).
//
// Only this controller's own top-level conditions count: a foreign controller's
// condition (e.g. special.io/...) carries an unrelated generation and MUST NOT
// be touched. The per-listener status array, by contrast, is wholly owned here.
func gatewayStatusStale(reconciledGen int64, gateway *gatewayv1.Gateway) bool {
	if ownedConditionsStale(gateway.Status.Conditions, reconciledGen,
		string(gatewayv1.GatewayConditionAccepted),
		string(gatewayv1.GatewayConditionProgrammed),
		string(gatewayv1.GatewayConditionResolvedRefs),
	) {
		return true
	}

	listenerConds := make([][]metav1.Condition, 0, len(gateway.Status.Listeners))
	for i := range gateway.Status.Listeners {
		listenerConds = append(listenerConds, gateway.Status.Listeners[i].Conditions)
	}

	return statusGenerationStale(reconciledGen, listenerConds...)
}

func (r *GatewayReconciler) setConfigErrorStatus(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	configErr error,
) error {
	gatewayKey := types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get fresh copy of the gateway to avoid conflict errors
		var freshGateway gatewayv1.Gateway
		if err := r.Get(ctx, gatewayKey, &freshGateway); err != nil {
			return errors.Wrap(err, "failed to get fresh gateway")
		}

		// Skip if a newer reconcile already advanced this Gateway's status past
		// the generation we observed (observedGeneration regression guard).
		if gatewayStatusStale(gateway.Generation, &freshGateway) {
			return nil
		}

		now := metav1.Now()
		errMsg := truncateMessage("Failed to resolve Gateway configuration: " + configErr.Error())

		// Clear addresses on config error (no valid tunnel to point to)
		freshGateway.Status.Addresses = nil

		_, _, clientCertErr := loadGatewayClientCertPEM(ctx, r.Client, &freshGateway, r.checkSecretReferenceGrant)

		applyGatewayConditions(&freshGateway.Status.Conditions, []metav1.Condition{
			{
				Type:               string(gatewayv1.GatewayConditionAccepted),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: freshGateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.GatewayReasonInvalidParameters),
				Message:            errMsg,
			},
			{
				Type:               string(gatewayv1.GatewayConditionProgrammed),
				Status:             metav1.ConditionFalse,
				ObservedGeneration: freshGateway.Generation,
				LastTransitionTime: now,
				Reason:             string(gatewayv1.GatewayReasonInvalid),
				Message:            errMsg,
			},
		}, buildClientCertResolvedRefsCondition(freshGateway.Generation, now, clientCertErr))

		// Clear listener statuses on config error
		freshGateway.Status.Listeners = nil

		if err := r.Status().Update(ctx, &freshGateway); err != nil {
			return errors.Wrap(err, "failed to update gateway status")
		}

		return nil
	})

	return errors.Wrap(err, "failed to update gateway status after retries")
}

//nolint:gocognit,gocyclo,cyclop,dupl,funlen // complexity due to counting two route types
func (r *GatewayReconciler) countAttachedRoutes(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) map[gatewayv1.SectionName]int32 {
	logger := logging.FromContext(ctx)
	result := make(map[gatewayv1.SectionName]int32)

	for _, listener := range gateway.Spec.Listeners {
		result[listener.Name] = 0
	}

	validator := routebinding.NewValidator(r.Client)

	// Count HTTPRoutes with binding validation
	var httpRouteList gatewayv1.HTTPRouteList

	err := r.List(ctx, &httpRouteList)
	if err != nil {
		logger.Error("failed to list HTTPRoutes for attached routes count", "error", err)
	} else {
		for i := range httpRouteList.Items {
			route := &httpRouteList.Items[i]

			for _, ref := range route.Spec.ParentRefs {
				if !r.refMatchesGateway(ref, gateway, route.Namespace) {
					continue
				}

				routeInfo := &routebinding.RouteInfo{
					Name:        route.Name,
					Namespace:   route.Namespace,
					Hostnames:   route.Spec.Hostnames,
					Kind:        routebinding.KindHTTPRoute,
					SectionName: ref.SectionName,
					Port:        ref.Port,
				}

				bindingResult, bindErr := validator.ValidateBinding(ctx, gateway, routeInfo)
				if bindErr != nil || !bindingResult.Accepted {
					continue
				}

				// Count this route for each matched listener
				for _, listenerName := range bindingResult.MatchedListeners {
					result[listenerName]++
				}
			}
		}
	}

	// Count GRPCRoutes with binding validation
	var grpcRouteList gatewayv1.GRPCRouteList

	err = r.List(ctx, &grpcRouteList)
	if err != nil {
		logger.Error("failed to list GRPCRoutes for attached routes count", "error", err)
	} else {
		for i := range grpcRouteList.Items {
			route := &grpcRouteList.Items[i]

			for _, ref := range route.Spec.ParentRefs {
				if !r.refMatchesGateway(ref, gateway, route.Namespace) {
					continue
				}

				routeInfo := &routebinding.RouteInfo{
					Name:        route.Name,
					Namespace:   route.Namespace,
					Hostnames:   route.Spec.Hostnames,
					Kind:        routebinding.KindGRPCRoute,
					SectionName: ref.SectionName,
					Port:        ref.Port,
				}

				bindingResult, bindErr := validator.ValidateBinding(ctx, gateway, routeInfo)
				if bindErr != nil || !bindingResult.Accepted {
					continue
				}

				// Count this route for each matched listener
				for _, listenerName := range bindingResult.MatchedListeners {
					result[listenerName]++
				}
			}
		}
	}

	return result
}

func (r *GatewayReconciler) refMatchesGateway(
	ref gatewayv1.ParentReference,
	gateway *gatewayv1.Gateway,
	routeNamespace string,
) bool {
	if string(ref.Name) != gateway.Name {
		return false
	}

	refNamespace := routeNamespace
	if ref.Namespace != nil {
		refNamespace = string(*ref.Namespace)
	}

	return refNamespace == gateway.Namespace
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapper := &ConfigMapper{
		Client:         r.Client,
		ControllerName: r.ControllerName,
		ConfigResolver: r.ConfigResolver,
	}

	//nolint:wrapcheck // controller-runtime builder pattern
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.Gateway{}).
		// Watch GatewayClass for parametersRef changes
		Watches(
			&gatewayv1.GatewayClass{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayClassToGateways),
		).
		// Watch GatewayClassConfig for config changes
		Watches(
			&v1alpha1.GatewayClassConfig{},
			handler.EnqueueRequestsFromMapFunc(mapper.MapConfigToRequests(r.getAllManagedGateways)),
		).
		// Watch GatewayConfig (per-Gateway data planes) so an edit that does
		// not change the rendered Deployment still refreshes the Gateway's
		// status (the single status writer lives here, not in the infra
		// reconciler).
		Watches(
			&v1alpha1.GatewayConfig{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayConfigToGateways),
		).
		// Watch Secrets for credential changes
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(mapper.MapSecretToRequests(r.getAllManagedGateways)),
		).
		// Watch ReferenceGrants for cross-namespace Secret access changes
		Watches(
			&gatewayv1beta1.ReferenceGrant{},
			handler.EnqueueRequestsFromMapFunc(r.referenceGrantToGateways),
		).
		// Watch HTTPRoutes to update AttachedRoutes count when routes change
		Watches(
			&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(r.routeToGateways),
		).
		// Watch GRPCRoutes to update AttachedRoutes count when routes change
		Watches(
			&gatewayv1.GRPCRoute{},
			handler.EnqueueRequestsFromMapFunc(r.routeToGateways),
		).
		// Watch rendered per-Gateway proxy Deployments (controller-owned by
		// their Gateway) so Programmed refreshes when replica readiness flips.
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), &gatewayv1.Gateway{}, handler.OnlyControllerOwner()),
		).
		// Watch ListenerSets so status.attachedListenerSets refreshes when a
		// ListenerSet is created, edited, or deleted — without this the count
		// would stay stale until an unrelated event triggered a Gateway
		// reconcile.
		Watches(
			&gatewayv1.ListenerSet{},
			handler.EnqueueRequestsFromMapFunc(r.listenerSetToGateways),
		).
		Complete(r)
}

// listenerSetToGateways maps a ListenerSet event to a reconcile request for
// the Gateway it points at, when the parent is one of ours.
func (r *GatewayReconciler) listenerSetToGateways(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	listenerSet, ok := obj.(*gatewayv1.ListenerSet)
	if !ok {
		return nil
	}

	parent, found := listenerSetParentGateway(ctx, r.Client, listenerSet)
	if !found {
		return nil
	}

	classNames, err := managedClassNames(ctx, r.Client, r.ControllerName)
	if err != nil {
		return nil
	}

	if !classNames[string(parent.Spec.GatewayClassName)] {
		return nil
	}

	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: parent.Name, Namespace: parent.Namespace},
	}}
}

// gatewayClassToGateways maps GatewayClass events to Gateway reconcile requests.
func (r *GatewayReconciler) gatewayClassToGateways(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	gatewayClass, ok := obj.(*gatewayv1.GatewayClass)
	if !ok {
		return nil
	}

	if string(gatewayClass.Spec.ControllerName) != r.ControllerName {
		return nil
	}

	return r.getAllManagedGateways(ctx)
}

func (r *GatewayReconciler) getAllManagedGateways(ctx context.Context) []reconcile.Request {
	var gatewayList gatewayv1.GatewayList

	err := r.List(ctx, &gatewayList)
	if err != nil {
		return nil
	}

	classNames, err := managedClassNames(ctx, r.Client, r.ControllerName)
	if err != nil {
		logging.FromContext(ctx).Warn("failed to get managed class names in getAllManagedGateways",
			"error", err)

		return nil
	}

	var requests []reconcile.Request

	for i := range gatewayList.Items {
		gw := &gatewayList.Items[i]
		if classNames[string(gw.Spec.GatewayClassName)] {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      gw.Name,
					Namespace: gw.Namespace,
				},
			})
		}
	}

	return requests
}

// gatewayConfigToGateways maps a GatewayConfig event to the managed Gateways
// in its namespace that reference it via infrastructure.parametersRef. Without
// this, an edit that does not change the rendered Deployment (e.g. swapping
// the credential ref, or editing the GatewayConfig before the Gateway exists)
// would not re-trigger the single status writer, leaving the Gateway status
// stale.
func (r *GatewayReconciler) gatewayConfigToGateways(ctx context.Context, obj client.Object) []reconcile.Request {
	var gateways gatewayv1.GatewayList
	if err := r.List(ctx, &gateways, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}

	var requests []reconcile.Request

	for i := range gateways.Items {
		gateway := &gateways.Items[i]

		if !config.HasInfrastructureParametersRef(gateway) {
			continue
		}

		if gateway.Spec.Infrastructure.ParametersRef.Name != obj.GetName() {
			continue
		}

		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: gateway.Name, Namespace: gateway.Namespace,
		}})
	}

	return requests
}

// referenceGrantToGateways maps ReferenceGrant events to Gateway reconcile requests.
// When a ReferenceGrant changes, we need to re-reconcile all Gateways that might
// reference Secrets in the ReferenceGrant's namespace.
func (r *GatewayReconciler) referenceGrantToGateways(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	grant, ok := obj.(*gatewayv1beta1.ReferenceGrant)
	if !ok {
		return nil
	}

	// Check if this ReferenceGrant allows Gateway access to Secrets
	allowsGatewayToSecrets := false

	for _, from := range grant.Spec.From {
		if from.Group == gatewayv1.GroupName && from.Kind == kindGateway {
			for _, to := range grant.Spec.To {
				if to.Group == "" && to.Kind == kindSecret {
					allowsGatewayToSecrets = true

					break
				}
			}
		}
	}

	if !allowsGatewayToSecrets {
		return nil
	}

	// Find all Gateways that reference Secrets in this namespace
	var gatewayList gatewayv1.GatewayList
	if err := r.List(ctx, &gatewayList); err != nil {
		return nil
	}

	classNames, err := managedClassNames(ctx, r.Client, r.ControllerName)
	if err != nil {
		logging.FromContext(ctx).Warn("failed to get managed class names in referenceGrantToGateways",
			"error", err)

		return nil
	}

	var requests []reconcile.Request

	for i := range gatewayList.Items {
		gateway := &gatewayList.Items[i]
		if !classNames[string(gateway.Spec.GatewayClassName)] {
			continue
		}

		// Check if this Gateway references Secrets in the ReferenceGrant's namespace
		if r.gatewayReferencesSecretsInNamespace(gateway, grant.Namespace) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      gateway.Name,
					Namespace: gateway.Namespace,
				},
			})
		}
	}

	return requests
}

// routeToGateways maps HTTPRoute/GRPCRoute events to Gateway reconcile requests.
// This ensures AttachedRoutes is updated when routes are created, updated, or deleted.
func (r *GatewayReconciler) routeToGateways(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	var parentRefs []gatewayv1.ParentReference

	switch route := obj.(type) {
	case *gatewayv1.HTTPRoute:
		parentRefs = route.Spec.ParentRefs
	case *gatewayv1.GRPCRoute:
		parentRefs = route.Spec.ParentRefs
	default:
		return nil
	}

	classNames, err := managedClassNames(ctx, r.Client, r.ControllerName)
	if err != nil {
		logging.FromContext(ctx).Warn("failed to get managed class names in routeToGateways",
			"error", err)

		return nil
	}

	seen := make(map[types.NamespacedName]bool)

	var requests []reconcile.Request

	for _, ref := range parentRefs {
		if ref.Kind != nil && *ref.Kind != kindGateway {
			continue
		}

		gwNamespace := obj.GetNamespace()
		if ref.Namespace != nil {
			gwNamespace = string(*ref.Namespace)
		}

		gwKey := types.NamespacedName{Name: string(ref.Name), Namespace: gwNamespace}
		if seen[gwKey] {
			continue
		}

		seen[gwKey] = true

		var gateway gatewayv1.Gateway
		if err := r.Get(ctx, gwKey, &gateway); err != nil {
			continue
		}

		if !classNames[string(gateway.Spec.GatewayClassName)] {
			continue
		}

		requests = append(requests, reconcile.Request{NamespacedName: gwKey})
	}

	return requests
}

// gatewayReferencesSecretsInNamespace checks if a Gateway references any Secrets
// in the given namespace through its TLS configuration. Two surfaces are
// inspected: each Listener's TLS.CertificateRefs (frontend cert refs) and the
// Gateway-level Spec.TLS.Backend.ClientCertificateRef (backend mTLS keypair).
// Both surfaces participate in ReferenceGrant-driven cross-namespace lookups,
// so a grant change in `namespace` must enqueue the Gateway whenever EITHER
// surface points at a Secret there.
func (r *GatewayReconciler) gatewayReferencesSecretsInNamespace(
	gateway *gatewayv1.Gateway,
	namespace string,
) bool {
	for i := range gateway.Spec.Listeners {
		listener := &gateway.Spec.Listeners[i]
		if listener.TLS == nil {
			continue
		}

		for _, ref := range listener.TLS.CertificateRefs {
			refNamespace := gateway.Namespace
			if ref.Namespace != nil {
				refNamespace = string(*ref.Namespace)
			}

			if refNamespace == namespace {
				return true
			}
		}
	}

	if backendRef := gatewayClientCertRef(gateway); backendRef != nil {
		refNamespace := gateway.Namespace
		if backendRef.Namespace != nil {
			refNamespace = string(*backendRef.Namespace)
		}

		if refNamespace == namespace {
			return true
		}
	}

	return false
}

// buildListenerAcceptedCondition builds the per-listener Accepted condition.
// This controller serves only HTTP and HTTPS listeners (which carry HTTPRoute /
// GRPCRoute through the in-process proxy). TCP, TLS, and UDP listeners have no
// data plane here — Cloudflare Tunnel is HTTP-focused and terminates TLS at the
// edge — so they are Accepted=False / UnsupportedProtocol per the Gateway API
// spec rather than the misleading Accepted=True they would otherwise get.
func buildListenerAcceptedCondition(protocol gatewayv1.ProtocolType, generation int64, now metav1.Time) metav1.Condition {
	condition := metav1.Condition{
		Type:               string(gatewayv1.ListenerConditionAccepted),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             string(gatewayv1.ListenerReasonAccepted),
		Message:            listenerMsgAccepted,
	}

	switch protocol {
	case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType:
		return condition
	case gatewayv1.TCPProtocolType, gatewayv1.TLSProtocolType, gatewayv1.UDPProtocolType:
		// No TCP/TLS/UDPRoute data plane — fall through to unsupported below.
	}

	condition.Status = metav1.ConditionFalse
	condition.Reason = string(gatewayv1.ListenerReasonUnsupportedProtocol)
	condition.Message = "Listener protocol " + string(protocol) + " is not supported; " +
		"this controller serves only HTTP and HTTPS listeners (HTTPRoute / GRPCRoute) through Cloudflare Tunnel. " +
		"Use an HTTP or HTTPS listener."

	return condition
}

// validateTLSCertificateRefs validates TLS certificate references for a listener.
// Returns the condition status, reason, and message for the ResolvedRefs condition.
// Per Gateway API spec, TLS certificateRefs must point to valid Secrets of type
// kubernetes.io/tls, and cross-namespace references require ReferenceGrant.
func (r *GatewayReconciler) validateTLSCertificateRefs(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	listener *gatewayv1.Listener,
) (metav1.ConditionStatus, string, string) {
	// No TLS config - nothing to validate
	if listener.TLS == nil || len(listener.TLS.CertificateRefs) == 0 {
		return metav1.ConditionTrue,
			string(gatewayv1.ListenerReasonResolvedRefs),
			"References resolved"
	}

	for _, ref := range listener.TLS.CertificateRefs {
		status, reason, msg := r.validateSingleCertRef(ctx, gateway, ref)
		if status == metav1.ConditionFalse {
			return status, reason, msg
		}
	}

	return metav1.ConditionTrue,
		string(gatewayv1.ListenerReasonResolvedRefs),
		msgReferencesResolved
}

// validateSingleCertRef validates a single certificate reference.
func (r *GatewayReconciler) validateSingleCertRef(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	ref gatewayv1.SecretObjectReference,
) (metav1.ConditionStatus, string, string) {
	// Default to Secret in core v1
	refKind := kindSecret
	if ref.Kind != nil {
		refKind = string(*ref.Kind)
	}

	refGroup := ""
	if ref.Group != nil {
		refGroup = string(*ref.Group)
	}

	// Only support core/v1 Secrets
	if refGroup != "" || refKind != kindSecret {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Unsupported certificate ref kind: %s/%s", refGroup, refKind)
	}

	// Determine namespace
	refNamespace := gateway.Namespace
	if ref.Namespace != nil {
		refNamespace = string(*ref.Namespace)
	}

	// Check cross-namespace access
	if refNamespace != gateway.Namespace {
		allowed, err := r.checkSecretReferenceGrant(ctx, gateway, refNamespace, ref)
		if err != nil {
			return metav1.ConditionFalse,
				string(gatewayv1.ListenerReasonRefNotPermitted),
				fmt.Sprintf("Failed to check ReferenceGrant: %v", err)
		}

		if !allowed {
			return metav1.ConditionFalse,
				string(gatewayv1.ListenerReasonRefNotPermitted),
				fmt.Sprintf("Cross-namespace reference to %s/%s not permitted", refNamespace, ref.Name)
		}
	}

	// Check Secret exists and has correct type
	return r.validateSecretExists(ctx, refNamespace, ref)
}

// validateSecretExists checks if a Secret exists and has type kubernetes.io/tls.
func (r *GatewayReconciler) validateSecretExists(
	ctx context.Context,
	namespace string,
	ref gatewayv1.SecretObjectReference,
) (metav1.ConditionStatus, string, string) {
	secret := &corev1.Secret{}

	err := r.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      string(ref.Name),
	}, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.ConditionFalse,
				string(gatewayv1.ListenerReasonInvalidCertificateRef),
				fmt.Sprintf("Secret %s/%s not found", namespace, ref.Name)
		}

		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Failed to get secret: %v", err)
	}

	// Validate Secret type
	if secret.Type != corev1.SecretTypeTLS {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Secret %s/%s is not of type kubernetes.io/tls", namespace, ref.Name)
	}

	// Validate certificate data exists and is valid PEM
	certData, hasCert := secret.Data[corev1.TLSCertKey]
	if !hasCert || len(certData) == 0 {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Secret %s/%s missing tls.crt data", namespace, ref.Name)
	}

	keyData, hasKey := secret.Data[corev1.TLSPrivateKeyKey]
	if !hasKey || len(keyData) == 0 {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Secret %s/%s missing tls.key data", namespace, ref.Name)
	}

	// Validate that certificate contains valid PEM data
	block, _ := pem.Decode(certData)
	if block == nil {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Secret %s/%s contains invalid certificate PEM data", namespace, ref.Name)
	}

	return metav1.ConditionTrue, "", ""
}

// buildResolvedRefsCondition creates the ResolvedRefs condition based on validation results.
// Per Gateway API spec:
//   - If no supported kinds exist: ResolvedRefs=False, InvalidRouteKinds
//   - If any explicitly specified kinds are invalid: ResolvedRefs=False, InvalidRouteKinds
//   - If TLS validation fails: ResolvedRefs=False, with TLS-specific reason
//   - Otherwise: ResolvedRefs=True
func (r *GatewayReconciler) buildResolvedRefsCondition(
	generation int64,
	now metav1.Time,
	hasValidKind, hasInvalidKind bool,
	tlsStatus metav1.ConditionStatus,
	tlsReason, tlsMessage string,
) metav1.Condition {
	switch {
	case !hasValidKind:
		// No supported kinds at all
		return metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonInvalidRouteKinds),
			Message:            listenerMsgNoSupportedRouteKinds,
		}
	case hasInvalidKind:
		// Some valid kinds exist, but some explicitly specified kinds are invalid
		return metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonInvalidRouteKinds),
			Message:            listenerMsgInvalidRouteKinds,
		}
	case tlsStatus == metav1.ConditionFalse:
		return metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             tlsStatus,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             tlsReason,
			Message:            tlsMessage,
		}
	default:
		return metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
			Message:            msgReferencesResolved,
		}
	}
}

// checkSecretReferenceGrant checks if a cross-namespace Secret reference is allowed
// by a ReferenceGrant in the target namespace.
func (r *GatewayReconciler) checkSecretReferenceGrant(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	targetNamespace string,
	ref gatewayv1.SecretObjectReference,
) (bool, error) {
	var grants gatewayv1beta1.ReferenceGrantList
	if err := r.List(ctx, &grants, client.InNamespace(targetNamespace)); err != nil {
		return false, errors.Wrap(err, "failed to list ReferenceGrants")
	}

	for i := range grants.Items {
		grant := &grants.Items[i]

		if !r.grantAllowsGateway(grant, gateway.Namespace) {
			continue
		}

		// Check To: must allow Secret with matching name
		// Per Gateway API spec, if to.Name is nil or empty, it allows ALL secrets in namespace
		for _, to := range grant.Spec.To {
			if to.Group == "" && to.Kind == kindSecret {
				// nil or empty name means "all secrets in namespace"
				if to.Name == nil || *to.Name == "" || string(*to.Name) == string(ref.Name) {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// grantAllowsGateway checks if a ReferenceGrant allows Gateway from the given namespace.
func (r *GatewayReconciler) grantAllowsGateway(
	grant *gatewayv1beta1.ReferenceGrant,
	gatewayNamespace string,
) bool {
	for _, from := range grant.Spec.From {
		if from.Group == gatewayv1.GroupName &&
			from.Kind == kindGateway &&
			string(from.Namespace) == gatewayNamespace {
			return true
		}
	}

	return false
}
