package controller

import (
	"context"
	"encoding/pem"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/parentref"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelproof"
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

	// listenerConditionPermissiveHostname is an advisory condition set on a
	// listener that combines allowedRoutes.namespaces.from: All with no hostname
	// pin — the hostname-capture vector (any namespace can claim any hostname).
	// Domain-prefixed and informational: the combination is legal Gateway API, so
	// the listener stays Accepted; this only makes the risk visible in status
	// rather than only in the multi-tenancy guidance.
	listenerConditionPermissiveHostname     = cfConditionDomainPrefix + "PermissiveHostname"
	listenerReasonUnpinnedHostnameAllowsAll = "UnpinnedHostnameAllowsAllNamespaces"
	listenerMsgPermissiveHostname           = "listener allows routes from all namespaces (allowedRoutes.namespaces.from: All) " +
		"with no hostname pin, so any namespace can claim any hostname on it — pin the listener hostname or scope " +
		"allowedRoutes to a namespace selector to bound the capture surface"

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

// truncateMessage truncates a Gateway/GatewayClassConfig condition message to
// maxConditionMessageLength, rune-safe (see truncateUTF8): a byte-boundary cut
// through a multi-byte rune would yield invalid UTF-8 the apiserver rejects.
func truncateMessage(msg string) string {
	return truncateUTF8(msg, maxConditionMessageLength)
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

	// Recorder emits Warning Events for refusals the operator must not miss.
	// Nil is a no-op (unit tests).
	Recorder events.EventRecorder

	// ProxyImage is the controller-level default proxy image for per-Gateway
	// data planes (the chart's --proxy-image). Mirrors GatewayInfraReconciler's
	// RenderDefaults.ProxyImage so the status path can detect the same
	// "no image configured" misconfig the infra reconciler refuses to render,
	// and surface it as Accepted=False/InvalidParameters instead of leaving the
	// Gateway stuck Programmed=Pending with the cause only in a Warning Event.
	ProxyImage string

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
	if stripped, err := r.stripLegacyFinalizer(ctx, &gateway); stripped || err != nil {
		return ctrl.Result{}, err
	}

	if managed, err := gatewayIsManaged(ctx, r.Client, r.ControllerName, &gateway); err != nil || !managed {
		return ctrl.Result{}, err
	}

	logger.Info("reconciling gateway", "name", gateway.Name, "namespace", gateway.Namespace)

	// Deletion path for v3-managed Gateways without a legacy finalizer: nothing
	// to do (proxy lifecycle is managed by the Helm chart, not per-Gateway).
	// The legacy-finalizer strip above already returned for v2-tagged Gateways.
	if !gateway.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	r.warnInvalidAllowedListeners(&gateway)

	if routebinding.RequestsFrontendValidation(&gateway) {
		return ctrl.Result{}, r.refuseFrontendValidation(ctx, &gateway)
	}

	if addressType, unsupported := routebinding.UnsupportedAddressType(&gateway); unsupported {
		return ctrl.Result{}, r.refuseUnsupportedAddress(ctx, &gateway, addressType)
	}

	resolvedConfig, perGatewayMode, err := r.resolveGatewayConfig(ctx, &gateway)
	if err != nil {
		return r.handleResolveError(ctx, &gateway, err, "failed to resolve gateway configuration")
	}

	if result, handled, err := r.refuseDedicatedPlane(ctx, &gateway, perGatewayMode); handled {
		return result, err
	}

	// Route sync programs nothing while the managed classes disagree, for
	// shared and dedicated planes alike, so no Gateway may report Accepted.
	// Checked after the refusals, as the infra reconciler does: a refusal
	// removes the plane there, and only a status written here announces a
	// lapsed Cloudflare confirmation to it. The refusal's requeue, and this
	// one, both come back within tunnelproof.RecheckInterval.
	if err := managedClassConfigConflict(ctx, r.Client, r.ControllerName); err != nil {
		return r.handleResolveError(ctx, &gateway, err, "managed GatewayClasses disagree")
	}

	if err := r.updateStatus(ctx, &gateway, resolvedConfig, perGatewayMode); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to update gateway status")
	}

	// A dedicated Gateway holds its tunnel on a Cloudflare confirmation that
	// lapses with no event in the cluster, so come back to re-check it. The
	// status this writes is what brings the infra reconciler along.
	if perGatewayMode {
		return ctrl.Result{RequeueAfter: tunnelproof.RecheckInterval}, nil
	}

	return ctrl.Result{}, nil
}

// stripLegacyFinalizer removes the v2 cloudflared finalizer from a Gateway
// being deleted. It reports whether it handled the Gateway.
func (r *GatewayReconciler) stripLegacyFinalizer(ctx context.Context, gateway *gatewayv1.Gateway) (bool, error) {
	if gateway.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(gateway, legacyCloudflaredFinalizer) {
		return false, nil
	}

	controllerutil.RemoveFinalizer(gateway, legacyCloudflaredFinalizer)

	if err := r.Update(ctx, gateway); err != nil {
		return true, errors.Wrap(err, "failed to remove legacy cloudflared finalizer")
	}

	return true, nil
}

// eventReasonInvalidAllowedListeners names the Warning Event raised on a
// Gateway whose allowedListeners.namespaces.selector does not parse.
const eventReasonInvalidAllowedListeners = "InvalidAllowedListeners"

// eventActionEvaluateAllowedListeners labels that Event's check.
const eventActionEvaluateAllowedListeners = "EvaluateAllowedListeners"

// warnInvalidAllowedListeners tells the Gateway's owner that its
// allowedListeners selector does not parse, which refuses every ListenerSet.
// The Gateway API has no condition for it and the Gateway's own listeners
// still serve, so it is an Event rather than a condition; each ListenerSet's
// own status carries the refusal. The message does not quote the selector.
// Repeats collapse through normal Event aggregation.
func (r *GatewayReconciler) warnInvalidAllowedListeners(gateway *gatewayv1.Gateway) {
	if r.Recorder == nil || !routebinding.ListenerNamespaceSelectorInvalid(gateway.Spec.AllowedListeners) {
		return
	}

	r.Recorder.Eventf(gateway, nil, corev1.EventTypeWarning,
		eventReasonInvalidAllowedListeners, eventActionEvaluateAllowedListeners,
		"allowedListeners.namespaces.selector is invalid; every ListenerSet is refused")
}

// refuseDedicatedPlane settles whether this Gateway may have a dedicated data
// plane at all: whether its tunnel claim holds, and whether its namespace is
// already at the operator's cap. handled reports that the outcome is decided
// here and the caller must not go on to write an Accepted status — a refused
// Gateway is not programmed by the route syncer and no plane is rendered for
// it, so reporting it Accepted would leave the operator with a healthy-looking
// Gateway whose routes silently never work.
//
// The tunnel claim is settled first. A Gateway that both claims a tunnel it
// does not own and sits over the cap has a security problem and a capacity
// problem; the first is the one whose remedy matters.
func (r *GatewayReconciler) refuseDedicatedPlane(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	perGatewayMode bool,
) (ctrl.Result, bool, error) {
	// Only a dedicated plane can claim a tunnel or consume a slot; everything
	// else serves the class tunnel from the shared plane, which neither rule
	// says anything about.
	if !perGatewayMode {
		return ctrl.Result{}, false, nil
	}

	// Read from THIS Gateway's class, while SyncAllRoutes reads from the first
	// managed class. Why that is not a divergence, and what serving a second
	// GatewayClassConfig would have to reconcile, is in applyPlaneRefusals.
	policy, resolveErr := r.ConfigResolver.ResolveTunnelPolicyForGatewayClass(ctx, string(gateway.Spec.GatewayClassName))
	if resolveErr != nil {
		err := errors.Wrap(resolveErr, "resolving the GatewayClass tunnel policy")

		// A deterministic problem with the GatewayClass is permanent, and
		// retrying it forever would leave the Gateway with no condition at all
		// — the operator would have only controller logs. Report it the way
		// every other deterministic config error is reported. That writes no
		// address (setConfigErrorStatus only preserves an existing one for a
		// per-Gateway Gateway), so it surrenders no possession.
		if errors.Is(err, config.ErrInvalidParameters) {
			result, statusErr := r.handleResolveError(ctx, gateway, err,
				"failed to read the GatewayClass tunnel policy for arbitration")

			return result, true, statusErr
		}

		// Anything else is ambiguous — a missing GatewayClassConfig may be
		// mid-apply. Falling through would write the Gateway's tunnel address,
		// and an advertised address IS possession, so a claimant would be
		// handed the holder's seat during a transient read failure and keep it
		// afterwards. Nothing may advertise a tunnel whose ownership is
		// unknown.
		return ctrl.Result{}, true, err
	}

	// One listing feeds both rules, so they judge the same Gateways.
	gateways, err := managedInfraGateways(ctx, r.Client, r.ControllerName)
	if err != nil {
		// No ErrInvalidParameters branch here, unlike the resolve above: the
		// listing fails only on API reads. Give it a deterministic config error
		// one day and the Gateway would requeue forever with no condition
		// written, so route it through handleResolveError at that point.
		return ctrl.Result{}, true, errors.Wrap(err, "listing managed Gateways")
	}

	if rejection := r.tunnelRejection(ctx, gateway, policy, gateways); rejection != nil {
		r.reportTunnelRejection(ctx, gateway, *rejection)

		return ctrl.Result{RequeueAfter: configErrorRequeueDelay, Priority: new(priorityGateway)}, true, nil
	}

	result, handled := r.refuseOverQuota(ctx, gateway, policy.MaxDataPlanesPerNamespace, gateways)

	return result, handled, nil
}

// refuseOverQuota reports a Gateway whose namespace already holds as many
// dedicated data planes as the operator allows. Same shape as the tunnel
// refusal above, and the same reason for it: the infra reconciler renders no
// plane for a Gateway over the cap, so the status must not claim otherwise.
func (r *GatewayReconciler) refuseOverQuota(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	capacity *int32,
	gateways []*gatewayv1.Gateway,
) (ctrl.Result, bool) {
	if !overQuotaGateways(capacity, collectDataPlaneClaims(gateways))[gateway.Namespace+"/"+gateway.Name] {
		return ctrl.Result{}, false
	}

	r.reportQuotaRefusal(ctx, gateway, *capacity)

	return ctrl.Result{RequeueAfter: configErrorRequeueDelay, Priority: new(priorityGateway)}, true
}

// reportQuotaRefusal makes a capacity refusal impossible to miss: an Error log
// for the operator's pipeline, a Warning Event on the Gateway, and
// Accepted=False/DataPlaneQuotaExceeded naming the cap.
func (r *GatewayReconciler) reportQuotaRefusal(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	capacity int32,
) {
	logger := log.FromContext(ctx)
	err := dataPlaneQuotaError(capacity)

	// The refusal requeues every configErrorRequeueDelay for as long as the
	// Gateway stands, so reporting on each pass would let the refused tenant
	// choose the log and event volume. Report only when the verdict is new;
	// the condition is what persists.
	if !isRefusalReported(gateway, dataPlaneQuotaMessage(capacity)) {
		logger.Error(err, "refusing a Gateway whose namespace is at its dedicated data-plane cap",
			"gateway", gateway.Namespace+"/"+gateway.Name,
			"cap", capacity)

		if r.Recorder != nil {
			r.Recorder.Eventf(gateway, nil, corev1.EventTypeWarning,
				reasonDataPlaneQuotaExceeded, eventActionEnforceQuota, "%s", err.Error())
		}
	}

	if statusErr := r.setConfigErrorStatus(ctx, gateway, err); statusErr != nil {
		logger.Error(statusErr, "failed to update gateway status")
	}
}

// dataPlaneQuotaError builds the error a capacity refusal is reported through.
// Marked and classified rather than wrapped, for the reasons
// tunnelRefusalError explains.
//
//nolint:wrapcheck // classifying rather than wrapping is the point, per above
func dataPlaneQuotaError(capacity int32) error {
	return config.MarkInvalidParameters(
		errors.Mark(errors.New(dataPlaneQuotaMessage(capacity)), errDataPlaneQuotaExceeded),
	)
}

// handleResolveError turns a failed configuration resolve into either a
// retryable error or an InvalidParameters status, depending on whether the
// failure says anything about the spec.
func (r *GatewayReconciler) handleResolveError(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	err error,
	what string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Error(err, what)

	// An error not marked ErrInvalidParameters is treated as a read failure
	// that says nothing about the spec. Stamping InvalidParameters over it
	// would misreport a healthy Gateway and, on the shared plane, clear the
	// address external-dns publishes, dropping DNS for every hostname on it.
	// Propagate for backoff instead and leave the last written status standing.
	if !errors.Is(err, config.ErrInvalidParameters) {
		return ctrl.Result{}, err
	}

	if statusErr := r.setConfigErrorStatus(ctx, gateway, err); statusErr != nil {
		logger.Error(statusErr, "failed to update gateway status")
	}

	return ctrl.Result{RequeueAfter: configErrorRequeueDelay, Priority: new(priorityGateway)}, nil
}

// frontendValidationMessage is the Accepted=False message of a Gateway that
// sets spec.tls.frontend. It names where client certificates can be checked
// instead, and must fit maxConditionMessageLength with refusedConditionPrefix.
const frontendValidationMessage = "spec.tls.frontend (client certificate validation) is not supported: " +
	"clients complete TLS with the Cloudflare edge, not this Gateway. " +
	"Remove it and check client certificates at the Cloudflare edge (mTLS or Access rules)."

// errFrontendValidationRefused marks a Gateway refused because it sets
// spec.tls.frontend, so the status writer reports it as Invalid.
var errFrontendValidationRefused = errors.New(frontendValidationMessage)

// refuseFrontendValidation reports a Gateway that sets spec.tls.frontend as
// Accepted=False and Programmed=False with reason Invalid. Route binding
// refuses the same Gateway through routebinding.RequestsFrontendValidation,
// so none of its routes is programmed. No requeue: only a spec edit, which
// is an event of its own, can change the verdict.
func (r *GatewayReconciler) refuseFrontendValidation(ctx context.Context, gateway *gatewayv1.Gateway) error {
	if !isRefusalReported(gateway, frontendValidationMessage) {
		log.FromContext(ctx).Info("refusing a Gateway that sets spec.tls.frontend",
			"gateway", gateway.Namespace+"/"+gateway.Name)
	}

	return r.setConfigErrorStatus(ctx, gateway, errFrontendValidationRefused)
}

// errUnsupportedAddress marks a Gateway refused because spec.addresses
// requests a type other than Hostname, so the status writer reports it with
// reason UnsupportedAddress.
var errUnsupportedAddress = errors.New("unsupported spec.addresses type")

// refuseUnsupportedAddress reports a Gateway whose spec.addresses requests a
// type other than Hostname as Accepted=False with reason UnsupportedAddress.
// Route binding refuses the same Gateway through
// routebinding.UnsupportedAddressType. No requeue: only a spec edit can change
// the verdict.
func (r *GatewayReconciler) refuseUnsupportedAddress(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	addressType gatewayv1.AddressType,
) error {
	message := fmt.Sprintf("spec.addresses requests a %s address, which is not supported: "+
		"a Cloudflare Tunnel is reachable only at its cfargotunnel.com hostname. "+
		"Remove spec.addresses or use a Hostname address.", addressType)

	if !isRefusalReported(gateway, message) {
		log.FromContext(ctx).Info("refusing a Gateway whose spec.addresses requests an unsupported type",
			"gateway", gateway.Namespace+"/"+gateway.Name, "type", addressType)
	}

	return r.setConfigErrorStatus(ctx, gateway, errors.Mark(errors.New(message), errUnsupportedAddress))
}

// errTunnelClaimRefused marks a Gateway refused by the tunnel-ownership rule,
// letting the status writer say "refused" rather than "failed to resolve" —
// the configuration resolved fine, the tunnel it named was not available.
// Refusals are marked with config.ErrInvalidParameters as well, so existing
// branches keyed on that keep matching.
var errTunnelClaimRefused = errors.New("tunnel claim refused")

// refusedConditionPrefix opens the Accepted=False message of a refused claim.
// The dedup searches the stored condition for the rendered message, so prefix
// and message together must stay inside maxConditionMessageLength — pinned by
// TestTunnelRejectionMessageSurvivesConditionTruncation.
const refusedConditionPrefix = "Refused: "

// isTunnelRefusalReported reports whether this exact refusal already stands on
// the Gateway's Accepted condition, so a repeating requeue does not re-log and
// re-event a verdict nothing has changed about.
func isTunnelRefusalReported(gateway *gatewayv1.Gateway, rejection tunnelownership.Rejection) bool {
	// Compare the whole rendered message, not just the tunnel ID: the same
	// tunnel can be refused for different reasons with different remedies (a
	// neighbour holds it, versus it being the class tunnel), and a verdict that
	// changed is news the operator has not heard yet.
	return isRefusalReported(gateway, tunnelRejectionMessage(rejection))
}

// isRefusalReported reports whether an Accepted=False carrying this exact
// message already stands on the Gateway.
func isRefusalReported(gateway *gatewayv1.Gateway, message string) bool {
	for _, condition := range gateway.Status.Conditions {
		if condition.Type != string(gatewayv1.GatewayConditionAccepted) {
			continue
		}

		return condition.Status == metav1.ConditionFalse &&
			strings.Contains(condition.Message, message)
	}

	return false
}

// eventReasonTunnelClaimRejected names the Warning Event raised when a
// Gateway claims a tunnel it does not own, or one Cloudflare does not confirm
// its connector token holds.
const eventReasonTunnelClaimRejected = "TunnelClaimRejected"

// reasonDataPlaneQuotaExceeded names both the Accepted=False reason and the
// Warning Event raised when a namespace is at its dedicated data-plane cap.
//
// It is an implementation-specific reason, which the Gateway API allows:
// GatewayConditionAccepted documents its own reasons as the ones "a controller
// should use", and states that controllers may raise the condition with other
// reasons. No listed reason describes a Gateway refused for capacity rather
// than for anything wrong with its spec.
const reasonDataPlaneQuotaExceeded = "DataPlaneQuotaExceeded"

// errDataPlaneQuotaExceeded marks a Gateway refused because its namespace is at
// the operator's dedicated data-plane cap, so the status writer can say so with
// its own reason rather than the generic InvalidParameters.
var errDataPlaneQuotaExceeded = errors.New("dedicated data-plane cap reached")

// eventActionArbitrate labels the tunnel-ownership decision. Distinct from the
// infra reconciler's render action: this layer decides, it does not render.
const eventActionArbitrate = "ArbitrateTunnel"

// eventActionEnforceQuota labels the capacity decision. A separate action from
// the tunnel one so an operator reading a DataPlaneQuotaExceeded Event is not
// pointed at tunnel arbitration, which has nothing to do with it.
const eventActionEnforceQuota = "EnforceDataPlaneQuota"

// reportTunnelRejection makes a refused claim impossible to miss: an Error log
// for the operator's pipeline, a Warning Event on the Gateway, and
// Accepted=False/InvalidParameters naming the contested tunnel. The Gateway is
// not programmed either way — this is only how the operator finds out.
func (r *GatewayReconciler) reportTunnelRejection(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	rejection tunnelownership.Rejection,
) {
	logger := log.FromContext(ctx)

	// The refusal requeues every configErrorRequeueDelay for as long as the
	// claim stands, so logging and eventing on each pass would emit thousands
	// of identical lines a day per refused Gateway, at a volume the refused
	// tenant chooses. Report only when the verdict is new; the condition is
	// what persists.
	alreadyReported := isTunnelRefusalReported(gateway, rejection)

	err := tunnelRefusalError(rejection)

	if !alreadyReported {
		logger.Error(err, "refusing a Gateway that claims a tunnel it does not own",
			"gateway", gateway.Namespace+"/"+gateway.Name,
			"tunnel", rejection.TunnelID,
			"heldBy", rejection.HeldBy,
			"unproven", rejection.Unproven)

		if r.Recorder != nil {
			r.Recorder.Eventf(gateway, nil, corev1.EventTypeWarning,
				eventReasonTunnelClaimRejected, eventActionArbitrate, "%s", err.Error())
		}
	}

	if statusErr := r.setConfigErrorStatus(ctx, gateway, err); statusErr != nil {
		logger.Error(statusErr, "failed to update gateway status")
	}
}

// tunnelRefusalError builds the error a refused claim is reported through.
//
// Mark, not Wrap: the tenant reads this message, and wrapping would append the
// internal chain after the actionable sentence, crowding out the part they can
// act on. Marking leaves the message untouched.
//
// Classified as config.ErrInvalidParameters as well as marked with
// errTunnelClaimRefused, because Mark does not carry the reference's own
// unwrap chain: marking only errTunnelClaimRefused would leave
// errors.Is(err, config.ErrInvalidParameters) false, and a refusal routed
// through handleResolveError would then take its transient branch and requeue
// forever without ever writing a condition. MarkInvalidParameters keeps the
// message as it is and is visible to the standard library's errors.Is too.
//
//nolint:wrapcheck // classifying rather than wrapping is the point, per above
func tunnelRefusalError(rejection tunnelownership.Rejection) error {
	return config.MarkInvalidParameters(
		errors.Mark(errors.New(tunnelRejectionMessage(rejection)), errTunnelClaimRefused),
	)
}

// tunnelRejectionMessage renders a rejection for the Gateway's own status and
// Event — surfaces the refused TENANT reads.
//
// It never names the Gateway holding the tunnel: that would tell the refused
// tenant the namespace and name of a neighbour, which is the cross-namespace
// disclosure this whole rule exists to prevent. The tenant gets what they can
// act on (the tunnel their Gateway claims is not theirs); the operator gets
// both sides from the controller log.
//
// It also never attributes the claim to the connector token. A claim has two
// sources — the tunnel the token names, and the one already advertised in
// status — and the second is what a Gateway migrating off the shared plane
// carries while its token is still unreadable. Naming the token would then
// accuse it of saying something it never said.
func tunnelRejectionMessage(rejection tunnelownership.Rejection) string {
	// An unproven claim names only the tunnel, and says nothing about why
	// Cloudflare refused it: telling a missing tunnel from a mismatched secret
	// would tell the tenant which tunnel UUIDs exist in the account.
	if rejection.Unproven {
		if rejection.Proof == tunnelownership.ProofUnknown {
			return "this Gateway's claim on tunnel " + rejection.TunnelID +
				" could not be checked with Cloudflare, which is unreachable or rejects the API credential;" +
				" the check is retried automatically"
		}

		return "Cloudflare did not confirm that this Gateway's connector token holds tunnel " +
			rejection.TunnelID + "; use the tunnel's current token, and an API credential that can edit the tunnel"
	}

	if rejection.IsClassTunnel {
		return "this Gateway claims the GatewayClass tunnel " + rejection.TunnelID +
			", which serves every Gateway without a dedicated data plane; " +
			"give this Gateway its own Cloudflare Tunnel"
	}

	return "this Gateway claims tunnel " + rejection.TunnelID +
		", which is already in use and not available to it; " +
		"give this Gateway its own Cloudflare Tunnel"
}

// tunnelRejection reports whether this Gateway's claimed tunnel belongs to
// someone else, or is not confirmed by Cloudflare. It runs the same
// arbitration as the route syncer over the same claim set, so status and
// programming reach the same verdict from the same inputs. Cloudflare's
// confirmation is one of those inputs and it lapses on a clock, so the two can
// differ until each has run since it changed. The Reconcile requeue for
// dedicated Gateways bounds that window; the Accepted change it writes is what
// brings the data plane and the route sync along. nil means not refused.
func (r *GatewayReconciler) tunnelRejection(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	policy *config.TunnelPolicy,
	gateways []*gatewayv1.Gateway,
) *tunnelownership.Rejection {
	classTunnel := canonicalTunnelID(policy.TunnelID)

	claims := collectTunnelClaims(ctx, gateways, r.ConfigResolver, classTunnel)

	// Same sharing opt-in the route syncer passes. Both layers must honour it
	// or an operator who enabled sharing would see Gateways stuck
	// Accepted=False while their routes were programmed perfectly.
	rejections := tunnelownership.Arbitrate(classTunnel, policy.AllowSharedTunnels, claims)

	rejection, ok := rejections[gateway.Namespace+"/"+gateway.Name]
	if !ok {
		return nil
	}

	return &rejection
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
	// Status-only resolve: deliberately does NOT read the generated config-API
	// auth Secret. That Secret is created by the infra reconciler, so reading
	// it here would fail transiently in the bootstrap window and leave the
	// Gateway statusless until it lands — yet the status path never consumes it.
	perGateway, err := r.ConfigResolver.ResolveStatusConfigForGateway(ctx, gateway)
	if err != nil {
		return nil, false, errors.Wrap(err, "per-gateway configuration")
	}

	if perGateway != nil {
		// Mirror the infra reconciler's render-skip guard: with neither a
		// per-Gateway image override nor a controller-level default, the data
		// plane can never be rendered. Classify it as a deterministic,
		// user-fixable spec problem so the Gateway surfaces
		// Accepted=False/InvalidParameters with the cause in its condition
		// message — instead of sitting Programmed=Pending forever while the
		// reason lives only in a transient Warning Event.
		if perGateway.GatewayConfig.Spec.Image == "" && r.ProxyImage == "" {
			return nil, true, errors.Wrap(config.ErrInvalidParameters,
				"no proxy image configured for the per-Gateway data plane: "+
					"set the controller's --proxy-image flag or GatewayConfig.spec.image")
		}

		return &perGateway.ResolvedConfig, true, nil
	}

	classConfig, err := r.ConfigResolver.ResolveFromGatewayClassName(ctx, string(gateway.Spec.GatewayClassName))
	if err != nil {
		return nil, false, errors.Wrap(err, "GatewayClass configuration")
	}

	return classConfig, false, nil
}

func (r *GatewayReconciler) updateStatus(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	cfg *config.ResolvedConfig,
	perGatewayMode bool,
) error {
	gatewayKey := types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace}

	var countErr, mergeErr, certErr error

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

		priorStatus := freshGateway.Status.DeepCopy()
		now := metav1.Now()

		views := newListenerViewCache(r.Client, r.ViewStore)

		// Written without the merged view, the Gateway's own conflicted
		// listeners would lose Conflicted=True; skip the write and retry.
		gwView, err := views.forGateway(ctx, &freshGateway)
		if err != nil {
			return errors.Wrap(err, "building the merged listener view")
		}

		mergeErr = applyAttachedListenerSets(ctx, r.Client, &freshGateway, views)

		tunnelHostname := cfg.TunnelID + cfArgotunnelSuffix
		freshGateway.Status.Addresses = []gatewayv1.GatewayStatusAddress{
			{
				Type:  new(gatewayv1.HostnameAddressType),
				Value: tunnelHostname,
			},
		}

		certErr = r.applyTopLevelGatewayConditions(ctx, &freshGateway, gwView, perGatewayMode, tunnelHostname, now)

		countErr, err = r.applyListenerStatuses(ctx, &freshGateway, gwView, now)
		if err != nil {
			return err
		}

		if apiequality.Semantic.DeepEqual(priorStatus, &freshGateway.Status) {
			return nil
		}

		if err := r.Status().Update(ctx, &freshGateway); err != nil {
			return errors.Wrap(err, "failed to update gateway status")
		}

		return nil
	})

	return errors.Wrap(errors.Join(err, countErr, mergeErr, certErr), "updating gateway status")
}

// applyTopLevelGatewayConditions computes and writes the Gateway-level
// Accepted, Programmed, and ResolvedRefs (client cert) conditions for the
// happy path. A client certificate that could not be read keeps its previous
// ResolvedRefs condition and is returned as an error.
func (r *GatewayReconciler) applyTopLevelGatewayConditions(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	gwView *gatewayListenerView,
	perGatewayMode bool,
	tunnelHostname string,
	now metav1.Time,
) error {
	_, _, clientCertErr := loadGatewayClientCertPEM(ctx, r.Client, gateway, r.checkSecretReferenceGrant)

	accepted := gatewayAcceptedCondition(gwView, gateway, now)

	programmed := metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionProgrammed),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gateway.Generation,
		LastTransitionTime: now,
		Reason:             string(gatewayv1.GatewayReasonProgrammed),
		Message:            "Gateway programmed in Cloudflare Tunnel",
	}

	// A dedicated data plane is only "programmed" once it can actually carry
	// traffic: the rendered proxy Deployment needs at least one ready replica
	// (= a registered tunnel connector). The shared plane keeps the historic
	// semantics (chart-managed proxy, always present).
	if perGatewayMode {
		programmed = r.perGatewayProgrammedCondition(ctx, gateway, now)
	}

	if requested, unusable := unusableHostnameAddress(gateway, tunnelHostname); unusable {
		programmed.Status = metav1.ConditionFalse
		programmed.Reason = string(gatewayv1.GatewayReasonAddressNotUsable)
		programmed.Message = truncateMessage(fmt.Sprintf("spec.addresses requests hostname %q, "+
			"but this Gateway is reachable only at %s; request that hostname, or leave the value empty",
			requested, tunnelHostname))
	}

	// Accepted judges only the Gateway's own listeners, while an attached
	// ListenerSet's entries are served as listeners of this Gateway; the
	// count was written earlier in the same status pass.
	if accepted.Status == metav1.ConditionFalse && !hasAttachedListenerSets(gateway) {
		programmed.Status = metav1.ConditionFalse
		programmed.Reason = string(gatewayv1.GatewayReasonInvalid)
		programmed.Message = accepted.Message
	}

	applyGatewayConditions(&gateway.Status.Conditions, []metav1.Condition{
		accepted,
		programmed,
	}, buildClientCertResolvedRefsCondition(gateway.Generation, now, clientCertErr))

	return transientClientCertError(clientCertErr)
}

func hasAttachedListenerSets(gateway *gatewayv1.Gateway) bool {
	return gateway.Status.AttachedListenerSets != nil && *gateway.Status.AttachedListenerSets > 0
}

// unusableHostnameAddress returns the first Hostname value in spec.addresses
// other than the tunnel hostname. An empty value asks the implementation to
// assign one, and the tunnel hostname is what it assigns.
func unusableHostnameAddress(gateway *gatewayv1.Gateway, tunnelHostname string) (string, bool) {
	for _, address := range gateway.Spec.Addresses {
		if address.Value != "" && address.Value != tunnelHostname {
			return address.Value, true
		}
	}

	return "", false
}

// transientClientCertError returns err when it is a client certificate read
// failure rather than a verdict on the reference, and nil otherwise.
func transientClientCertError(err error) error {
	if errors.Is(err, errGatewayClientCertTransientError) {
		return err
	}

	return nil
}

// applyAttachedListenerSets writes the Gateway's attachedListenerSets count.
// A count that could not be finished keeps the count already written and is
// returned as an error.
func applyAttachedListenerSets(
	ctx context.Context,
	cli client.Client,
	gateway *gatewayv1.Gateway,
	views *listenerViewCache,
) error {
	count, err := summariseAttachedListenerSets(ctx, cli, gateway, views)
	if err != nil {
		return err
	}

	gateway.Status.AttachedListenerSets = clampedInt32Pointer(count)

	return nil
}

// applyListenerStatuses writes the Gateway's listener statuses. The first
// error reports an attached-route count that could not be finished, which
// keeps the counts already written; the second reports a status that could not
// be built, and then nothing is written.
func (r *GatewayReconciler) applyListenerStatuses(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	gwView *gatewayListenerView,
	now metav1.Time,
) (error, error) {
	attachedRoutes, countErr := r.attachedRouteCounts(ctx, gateway)

	listenerStatuses, err := r.buildListenerStatuses(ctx, gateway, gwView, attachedRoutes, now)
	if err != nil {
		return countErr, err
	}

	gateway.Status.Listeners = preserveGatewayListenerTransitions(gateway.Status.Listeners, listenerStatuses)

	return countErr, nil
}

// attachedRouteCounts counts the routes attached to each Gateway listener. A
// count that could not be finished keeps the counts already written, and the
// error is returned so the reconcile counts again.
func (r *GatewayReconciler) attachedRouteCounts(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) (map[gatewayv1.SectionName]int32, error) {
	attachedRoutes, err := r.countAttachedRoutes(ctx, gateway)
	if err == nil {
		return attachedRoutes, nil
	}

	attachedRoutes = make(map[gatewayv1.SectionName]int32, len(gateway.Status.Listeners))
	for i := range gateway.Status.Listeners {
		attachedRoutes[gateway.Status.Listeners[i].Name] = gateway.Status.Listeners[i].AttachedRoutes
	}

	return attachedRoutes, err
}

// buildListenerStatuses builds one ListenerStatus per spec listener:
// SupportedKinds, AttachedRoutes, and the Accepted / Programmed / ResolvedRefs
// / Conflicted condition set, plus the advisory PermissiveHostname condition.
// Shared by the happy path and the config-error path — neither the listener's
// own protocol/route-kind/TLS/conflict verdict nor its attached-route count
// depends on whether the Gateway's tunnel configuration resolved. gwView
// annotates each conflicted Gateway-owned listener. A certificate reference
// that could not be read returns an error and no statuses.
func (r *GatewayReconciler) buildListenerStatuses(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	gwView *gatewayListenerView,
	attachedRoutes map[gatewayv1.SectionName]int32,
	now metav1.Time,
) ([]gatewayv1.ListenerStatus, error) {
	listenerStatuses := make([]gatewayv1.ListenerStatus, 0, len(gateway.Spec.Listeners))

	for i := range gateway.Spec.Listeners {
		listener := &gateway.Spec.Listeners[i]

		status, err := r.buildOneListenerStatus(ctx, gateway, listener, gwView, attachedRoutes[listener.Name], now)
		if err != nil {
			return nil, err
		}

		listenerStatuses = append(listenerStatuses, status)
	}

	return listenerStatuses, nil
}

// buildListenerProgrammedCondition derives a listener's Programmed condition:
// True unless its ResolvedRefs or Accepted verdict is already False, in which
// case Programmed carries the same Invalid reason (an unresolved reference, an
// unservable protocol or an invalid allowedRoutes selector means nothing is
// programmed either).
func buildListenerProgrammedCondition(
	generation int64,
	now metav1.Time,
	resolvedRefsCondition, acceptedCondition *metav1.Condition,
) metav1.Condition {
	condition := metav1.Condition{
		Type:               string(gatewayv1.ListenerConditionProgrammed),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             string(gatewayv1.ListenerReasonProgrammed),
		Message:            listenerMsgProgrammed,
	}

	switch {
	case acceptedCondition.Status == metav1.ConditionFalse:
		condition.Status = metav1.ConditionFalse
		condition.Reason = string(gatewayv1.ListenerReasonInvalid)
		condition.Message = acceptedCondition.Message
	case resolvedRefsCondition.Status == metav1.ConditionFalse:
		condition.Status = metav1.ConditionFalse
		condition.Reason = string(gatewayv1.ListenerReasonInvalid)
		condition.Message = listenerMsgInvalidUnresolved
	}

	return condition
}

// buildOneListenerStatus builds the SupportedKinds/AttachedRoutes/Conditions
// for a single spec listener: Accepted, Programmed, ResolvedRefs, the
// Conflicted override, and the advisory PermissiveHostname condition.
func (r *GatewayReconciler) buildOneListenerStatus(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	listener *gatewayv1.Listener,
	gwView *gatewayListenerView,
	attachedRoutes int32,
	now metav1.Time,
) (gatewayv1.ListenerStatus, error) {
	// Validate route kinds - filter to only supported kinds
	supportedKinds, hasValidKind, hasInvalidKind := routebinding.FilterSupportedKinds(
		listener.AllowedRoutes,
		listener.Protocol,
	)

	// Validate TLS certificate refs (if applicable)
	tlsStatus, tlsReason, tlsMessage, err := r.validateTLSCertificateRefs(
		ctx, gateway, listener,
	)
	if err != nil {
		return gatewayv1.ListenerStatus{}, err
	}

	// Determine final ResolvedRefs condition
	resolvedRefsCondition := r.buildResolvedRefsCondition(
		gateway.Generation, now, hasValidKind, hasInvalidKind, tlsStatus, tlsReason, tlsMessage,
	)
	// Empty slice (not nil) when no valid kinds. A protocol this
	// controller cannot serve supports no route kinds either: an
	// unrecognised protocol (e.g. the conformance suite's INVALID)
	// otherwise defaults to HTTPRoute/GRPCRoute in FilterSupportedKinds and
	// would report a non-empty SupportedKinds alongside its
	// Accepted=False/UnsupportedProtocol verdict — contradictory, and the
	// spec requires an empty list. Same servability predicate as the
	// Accepted condition, so the two cannot drift.
	if !hasValidKind || !servableListenerProtocol(listener.Protocol) {
		supportedKinds = []gatewayv1.RouteGroupKind{}
	}

	acceptedCondition := buildListenerAcceptedCondition(listener.Protocol, gateway.Generation, now)
	refuseInvalidNamespaceSelector(&acceptedCondition, listener.AllowedRoutes)
	programmedCondition := buildListenerProgrammedCondition(gateway.Generation, now, &resolvedRefsCondition, &acceptedCondition)

	conditions := []metav1.Condition{acceptedCondition, programmedCondition, resolvedRefsCondition}

	// A conflicted Gateway-owned listener MUST carry Conflicted=True and is
	// neither Accepted nor Programmed (gateway_types.go:168-170).
	conflicted := conflictedGatewayListenerConditions(
		gwView, listener.Name, gateway.Generation, now, &resolvedRefsCondition,
	)
	if conflicted != nil {
		conditions = conflicted
	}

	// Advisory (does not gate Accepted/Programmed): flag the
	// hostname-capture combination — allowedRoutes.namespaces.from: All
	// with no hostname pin. Only on an Accepted (protocol-servable),
	// non-conflicted listener: hostname capture is moot on a listener
	// already rejected as unservable or conflicted, where the advisory
	// would be misleading noise. Rebuilt every reconcile, so it clears
	// when the listener is pinned or scoped (#476).
	if conflicted == nil && acceptedCondition.Status == metav1.ConditionTrue &&
		hostnameCaptureRisk(listener.Hostname, listener.AllowedRoutes) {
		conditions = append(conditions, permissiveHostnameCondition(gateway.Generation, now))
	}

	return gatewayv1.ListenerStatus{
		Name:           listener.Name,
		SupportedKinds: supportedKinds,
		AttachedRoutes: attachedRoutes,
		Conditions:     conditions,
	}, nil
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
// Only this controller's own conditions count, top-level and per-listener: a
// foreign controller's condition (e.g. special.io/...) carries an unrelated
// generation and MUST NOT be touched.
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

	return ownedListenerConditionsStale(reconciledGen, listenerConds...)
}

func (r *GatewayReconciler) setConfigErrorStatus(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	configErr error,
) error {
	gatewayKey := types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace}

	var certErr error

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

		priorStatus := freshGateway.Status.DeepCopy()
		now := metav1.Now()

		prefix := "Failed to resolve Gateway configuration: "
		if errors.Is(configErr, errTunnelClaimRefused) || errors.Is(configErr, errDataPlaneQuotaExceeded) ||
			errors.Is(configErr, errFrontendValidationRefused) || errors.Is(configErr, errUnsupportedAddress) {
			// Nothing failed to resolve; the Gateway was refused.
			prefix = refusedConditionPrefix
		}

		errMsg := truncateMessage(prefix + configErr.Error())

		// Clear addresses on config error (no valid tunnel to point to).
		//
		// A Gateway with its own data plane is the exception: its address
		// records the tunnel that plane is attached to, and a configuration
		// read failing does not detach it. Clearing it would also surrender
		// the Gateway's claim on that tunnel — tunnel ownership is decided by
		// which Gateway advertises it — so a token rotation that briefly
		// deletes the Secret would let another namespace take the tunnel over
		// and lock the owner out for good.
		//
		// A class conflict is the other exception: the class tunnel keeps
		// serving its last configuration, and external-dns publishes records
		// from this address, so clearing it would delete DNS for every
		// hostname on the shared plane.
		if !config.HasInfrastructureParametersRef(&freshGateway) && !errors.Is(configErr, errClassConfigConflict) {
			freshGateway.Status.Addresses = nil
		}

		// Every ListenerSet of a Gateway refused here reports ParentNotAccepted
		// (parentGatewayRefused), so none counts as attached.
		freshGateway.Status.AttachedListenerSets = clampedInt32Pointer(0)

		_, _, clientCertErr := loadGatewayClientCertPEM(ctx, r.Client, &freshGateway, r.checkSecretReferenceGrant)
		certErr = transientClientCertError(clientCertErr)

		reasons := configErrorReasons(configErr)

		applyGatewayConditions(&freshGateway.Status.Conditions,
			configErrorGatewayConditions(freshGateway.Generation, now, errMsg, reasons),
			buildClientCertResolvedRefsCondition(freshGateway.Generation, now, clientCertErr))

		if err := r.applyConfigErrorListenerStatuses(ctx, &freshGateway, now, errMsg, reasons.listener); err != nil {
			return err
		}

		if apiequality.Semantic.DeepEqual(priorStatus, &freshGateway.Status) {
			return nil
		}

		if err := r.Status().Update(ctx, &freshGateway); err != nil {
			return errors.Wrap(err, "failed to update gateway status")
		}

		return nil
	})

	return errors.Wrap(errors.Join(err, certErr), "failed to update gateway status after retries")
}

// applyConfigErrorListenerStatuses writes the listener statuses of a Gateway
// whose configuration did not resolve. Per-listener status still reflects
// each listener's own validity (protocol, route kinds, TLS refs, conflicts),
// none of which depends on the tunnel configuration. Only a Programmed=True
// verdict is overridden: nothing is programmed without a resolved tunnel,
// while a listener already unprogrammed for its own reason keeps that more
// specific verdict. An attachedRoutes count that cannot finish keeps the counts
// already written, and the config error's own requeue counts again. A merged
// view or certificate reference that cannot be read returns an error, so the
// status is not written without the verdicts that depend on it.
func (r *GatewayReconciler) applyConfigErrorListenerStatuses(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	now metav1.Time,
	errMsg, listenerReason string,
) error {
	gwView, err := newListenerViewCache(r.Client, r.ViewStore).forGateway(ctx, gateway)
	if err != nil {
		return errors.Wrap(err, "building the merged listener view")
	}

	attachedRoutes, _ := r.attachedRouteCounts(ctx, gateway)

	listenerStatuses, err := r.buildListenerStatuses(ctx, gateway, gwView, attachedRoutes, now)
	if err != nil {
		return err
	}

	for i := range listenerStatuses {
		overrideListenerProgrammedForConfigError(listenerStatuses[i].Conditions, gateway.Generation, now, errMsg, listenerReason)
	}

	gateway.Status.Listeners = preserveGatewayListenerTransitions(gateway.Status.Listeners, listenerStatuses)

	return nil
}

// configErrorReasons picks the Accepted, Programmed and listener-Programmed
// reasons for a config error.
//
// A namespace at its data-plane cap has nothing wrong with its parameters, and
// the Gateway is neither syntactically nor semantically invalid — so the two
// default reasons would both send the operator looking for a spec mistake that
// is not there. Programmed uses the spec's own NoResources ("the Gateway is not
// scheduled because insufficient infrastructure resources are available"),
// which is what a cap declares; Accepted has no listed reason for capacity and
// takes the implementation-specific one.
//
// The listener reason is decided here rather than at the override, because the
// two are written by one call and a listener reporting Invalid under a Gateway
// reporting NoResources is one object answering the same question twice.
// Pending is the spec's reason for a listener "not yet online and ready to
// accept client traffic", which is a refused Gateway's listener exactly.
func configErrorReasons(configErr error) configErrorReasonSet {
	if errors.Is(configErr, errDataPlaneQuotaExceeded) {
		return configErrorReasonSet{
			accepted:   reasonDataPlaneQuotaExceeded,
			programmed: string(gatewayv1.GatewayReasonNoResources),
			listener:   string(gatewayv1.ListenerReasonPending),
		}
	}

	if errors.Is(configErr, errUnsupportedAddress) {
		return configErrorReasonSet{
			accepted:   string(gatewayv1.GatewayReasonUnsupportedAddress),
			programmed: string(gatewayv1.GatewayReasonInvalid),
			listener:   string(gatewayv1.ListenerReasonInvalid),
		}
	}

	if errors.Is(configErr, errFrontendValidationRefused) {
		return configErrorReasonSet{
			accepted:   string(gatewayv1.GatewayReasonInvalid),
			programmed: string(gatewayv1.GatewayReasonInvalid),
			listener:   string(gatewayv1.ListenerReasonInvalid),
		}
	}

	return configErrorReasonSet{
		accepted:   string(gatewayv1.GatewayReasonInvalidParameters),
		programmed: string(gatewayv1.GatewayReasonInvalid),
		listener:   string(gatewayv1.ListenerReasonInvalid),
	}
}

// configErrorReasonSet carries the condition reasons for a config error. A
// struct, not three positional strings: transposing two of those compiles and
// reports the wrong reason.
type configErrorReasonSet struct {
	accepted   string
	programmed string
	listener   string
}

// configErrorGatewayConditions is the Gateway-level verdict when a Gateway
// cannot be programmed for a deterministic reason: its configuration did not
// resolve, it claimed a tunnel it does not own, its namespace is at the
// operator's data-plane cap, it sets spec.tls.frontend, or it requests an
// unsupported address type. The reasons
// differ per case and are chosen by configErrorReasons; the message is
// shared.
func configErrorGatewayConditions(
	generation int64,
	now metav1.Time,
	message string,
	reasons configErrorReasonSet,
) []metav1.Condition {
	return []metav1.Condition{
		{
			Type:               string(gatewayv1.GatewayConditionAccepted),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             reasons.accepted,
			Message:            message,
		},
		{
			Type:               string(gatewayv1.GatewayConditionProgrammed),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             reasons.programmed,
			Message:            message,
		},
	}
}

// overrideListenerProgrammedForConfigError downgrades a listener's
// Programmed=True condition with the config-error message and reason carried on
// the Gateway-level Programmed condition. A listener already Programmed=False
// keeps its own reason and message; the config error is visible one level up.
func overrideListenerProgrammedForConfigError(
	conditions []metav1.Condition,
	generation int64,
	now metav1.Time,
	message, reason string,
) {
	for i := range conditions {
		if conditions[i].Type != string(gatewayv1.ListenerConditionProgrammed) ||
			conditions[i].Status != metav1.ConditionTrue {
			continue
		}

		conditions[i].Status = metav1.ConditionFalse
		conditions[i].ObservedGeneration = generation
		conditions[i].LastTransitionTime = now
		conditions[i].Reason = reason
		conditions[i].Message = message

		return
	}
}

func (r *GatewayReconciler) countAttachedRoutes(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) (map[gatewayv1.SectionName]int32, error) {
	result := make(map[gatewayv1.SectionName]int32)

	for _, listener := range gateway.Spec.Listeners {
		result[listener.Name] = 0
	}

	validator := routebinding.NewValidator(r.Client)

	var httpRouteList gatewayv1.HTTPRouteList
	if err := r.List(ctx, &httpRouteList); err != nil {
		return nil, errors.Wrap(err, "failed to list HTTPRoutes for attached routes count")
	}

	for i := range httpRouteList.Items {
		if err := r.countRouteOnGateway(ctx, validator, gateway, HTTPRouteWrapper{&httpRouteList.Items[i]}, result); err != nil {
			return nil, err
		}
	}

	var grpcRouteList gatewayv1.GRPCRouteList
	if err := r.List(ctx, &grpcRouteList); err != nil {
		return nil, errors.Wrap(err, "failed to list GRPCRoutes for attached routes count")
	}

	for i := range grpcRouteList.Items {
		if err := r.countRouteOnGateway(ctx, validator, gateway, GRPCRouteWrapper{&grpcRouteList.Items[i]}, result); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// countRouteOnGateway adds one route to the Gateway's per-listener
// attachedRoutes. Per the vendored AttachedRoutes doc, attachment follows
// allowedRoutes and parentRefs whatever the listener's own status, and only a
// route Accepted for the Gateway counts. The Accepted verdict is read from the
// route's own status, so every rejection counts: binding, conflicted
// listeners, hostname ownership and cross-type conflicts. A route counts at
// most once per listener however many of its parentRefs match it.
func (r *GatewayReconciler) countRouteOnGateway(
	ctx context.Context,
	validator *routebinding.Validator,
	gateway *gatewayv1.Gateway,
	route Route,
	result map[gatewayv1.SectionName]int32,
) error {
	counted := make(map[gatewayv1.SectionName]bool)

	for _, ref := range route.GetParentRefs() {
		if !r.refMatchesGateway(ref, gateway, route.GetNamespace()) ||
			!parentRefAcceptedInStatus(route.GetParentStatuses(), ref, route.GetNamespace(), r.ControllerName) {
			continue
		}

		bindingResult, bindErr := validator.ValidateBinding(ctx, gateway, &routebinding.RouteInfo{
			Name:        route.GetName(),
			Namespace:   route.GetNamespace(),
			Hostnames:   route.GetHostnames(),
			Kind:        route.GetRouteKind(),
			SectionName: ref.SectionName,
			Port:        ref.Port,
		})
		if bindErr == nil && bindingResult.Incomplete {
			bindErr = errListenerNotEvaluated
		}

		if bindErr != nil {
			return errors.Wrapf(bindErr, "binding %s %s/%s", route.GetRouteKind(), route.GetNamespace(), route.GetName())
		}

		if !bindingResult.Accepted {
			continue
		}

		for _, listenerName := range bindingResult.MatchedListeners {
			if !counted[listenerName] {
				counted[listenerName] = true
				result[listenerName]++
			}
		}
	}

	return nil
}

// refMatchesGateway reports whether a route parentRef names this Gateway
// itself: the Gateway API group, kind Gateway, and its name and namespace. A
// ListenerSet ref is counted on the ListenerSet's own entries, not here.
func (r *GatewayReconciler) refMatchesGateway(
	ref gatewayv1.ParentReference,
	gateway *gatewayv1.Gateway,
	routeNamespace string,
) bool {
	if !parentref.InGatewayAPIGroup(ref) || (ref.Kind != nil && *ref.Kind != kindGateway) {
		return false
	}

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
			builder.WithPredicates(classConfigWatchPredicates()...),
		).
		// Watch sibling Gateways: the data-plane cap makes one Gateway's opt-in
		// change another's verdict, and the displaced Gateway is not the object
		// written. Generation-gated so this controller's own status writes do
		// not feed back into it.
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.namespaceDataPlaneSiblings),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		// Enqueue every managed Gateway when a Gateway event flips the
		// class-conflict verdict. Generation-gated for the same reason as the
		// watch above.
		Watches(
			&gatewayv1.Gateway{},
			r.classConflictHandler(),
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
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
		Name: parent.Name, Namespace: parent.Namespace,
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
		logging.FromContext(ctx).Warn("failed to list Gateways in getAllManagedGateways", "error", err)

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
				Name:      gw.Name,
				Namespace: gw.Namespace,
			})
		}
	}

	return requests
}

// namespaceDataPlaneSiblings enqueues every opted-in Gateway in the event
// object's namespace, so a Gateway whose verdict was changed by a SIBLING gets
// its status rewritten. The cap is the only rule with that shape: tunnel
// arbitration refuses the newcomer and leaves the holder's verdict alone, while
// the cap is ordered oldest first, so a Gateway opting in with an older
// creationTimestamp displaces the current newest holder — which nobody wrote
// and For() therefore never delivers.
func (r *GatewayReconciler) namespaceDataPlaneSiblings(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	return optedInGatewaysInNamespace(ctx, r.Client, r.ControllerName, obj.GetNamespace())
}

// classConflictHandler enqueues every managed Gateway when a Gateway event can
// flip whether the managed classes in use conflict.
//
// The conflict check counts only classes some Gateway uses, so a Gateway landing
// on or leaving an otherwise unused class flips every Gateway on every other
// class, none of which was written. Only a create, a delete or a move between
// classes changes which classes are in use; an edit that keeps the class cannot
// flip anything.
func (r *GatewayReconciler) classConflictHandler() handler.Funcs {
	enqueueOnChange := func(
		ctx context.Context,
		before, after string,
		queue workqueue.TypedRateLimitingInterface[reconcile.Request],
	) {
		for _, request := range r.classConflictRequests(ctx, before, after) {
			queue.Add(request)
		}
	}

	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueueOnChange(ctx, "", gatewayClassOf(e.Object), queue)
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueueOnChange(ctx, gatewayClassOf(e.ObjectOld), gatewayClassOf(e.ObjectNew), queue)
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueueOnChange(ctx, gatewayClassOf(e.Object), "", queue)
		},
	}
}

// gatewayClassOf returns the GatewayClass a Gateway names, empty for anything
// else.
func gatewayClassOf(obj client.Object) string {
	if gateway, ok := obj.(*gatewayv1.Gateway); ok {
		return string(gateway.Spec.GatewayClassName)
	}

	return ""
}

// classConflictRequests returns every managed Gateway when a Gateway moving
// from class before to class after, empty meaning none, happens while the
// managed classes, in use or not, disagree, and nothing otherwise.
//
// It deliberately does not narrow to the classes in use, nor work out whether
// this one event flips the verdict over them. The informer writes its cache
// before it delivers events, so two Gateways that put a class in use together
// are both cached when either handler runs, and each would see the other
// already holding the class; a delete likewise arrives after its Gateway left
// the cache.
func (r *GatewayReconciler) classConflictRequests(ctx context.Context, before, after string) []reconcile.Request {
	if before == after {
		return nil
	}

	classes, err := listGatewayClassesForController(ctx, r.Client, r.ControllerName)
	if err != nil {
		logging.FromContext(ctx).Warn("failed to list GatewayClasses for the class-conflict watch", "error", err)

		return nil
	}

	if classConfigConflict(classes, r.ControllerName) == nil {
		return nil
	}

	return r.getAllManagedGateways(ctx)
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
		log.FromContext(ctx).Error(err, "listing Gateways for a GatewayConfig event; status-refresh trigger dropped",
			"namespace", obj.GetNamespace())

		return nil
	}

	var requests []reconcile.Request

	for i := range gateways.Items {
		gateway := &gateways.Items[i]

		if !config.HasInfrastructureParametersRef(gateway) {
			continue
		}

		ref := gateway.Spec.Infrastructure.ParametersRef
		// Match Group/Kind too, not just Name: a parametersRef to a foreign CRD
		// that happens to share the GatewayConfig's name is not ours to resolve.
		if string(ref.Group) != config.ParametersRefGroup || string(ref.Kind) != config.GatewayParametersRefKind {
			continue
		}

		if ref.Name != obj.GetName() {
			continue
		}

		requests = append(requests, reconcile.Request{
			Name: gateway.Name, Namespace: gateway.Namespace,
		})
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
				if isCoreSecret(string(to.Group), string(to.Kind)) {
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
				Name:      gateway.Name,
				Namespace: gateway.Namespace,
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
		if !parentref.InGatewayAPIGroup(ref) || (ref.Kind != nil && *ref.Kind != kindGateway) {
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

	if servableListenerProtocol(protocol) {
		return condition
	}

	condition.Status = metav1.ConditionFalse
	condition.Reason = string(gatewayv1.ListenerReasonUnsupportedProtocol)
	condition.Message = "Listener protocol " + string(protocol) + " is not supported; " +
		"this controller serves only HTTP and HTTPS listeners (HTTPRoute / GRPCRoute) through Cloudflare Tunnel. " +
		"Use an HTTP or HTTPS listener."

	return condition
}

// listenerMsgInvalidNamespaceSelector says a listener's namespace selector is
// invalid without quoting it.
const listenerMsgInvalidNamespaceSelector = "allowedRoutes.namespaces.selector is invalid, so the listener admits no route"

// refuseInvalidNamespaceSelector marks an otherwise accepted listener not
// Accepted when its allowedRoutes namespace selector does not parse. The
// listener is not semantically valid and admits no route. The Gateway API
// lists no reason for an invalid field value, so it uses UnsupportedValue, the
// closest of the listed ones; Programmed follows with Invalid.
func refuseInvalidNamespaceSelector(accepted *metav1.Condition, allowedRoutes *gatewayv1.AllowedRoutes) {
	if accepted.Status != metav1.ConditionTrue || !routebinding.NamespaceSelectorInvalid(allowedRoutes) {
		return
	}

	accepted.Status = metav1.ConditionFalse
	accepted.Reason = string(gatewayv1.ListenerReasonUnsupportedValue)
	accepted.Message = listenerMsgInvalidNamespaceSelector
}

// servableListenerProtocol is the single source of truth for the per-listener
// Accepted condition, the Gateway-level ListenersNotValid aggregation below and
// the conflict exemption in listenermerge.
func servableListenerProtocol(protocol gatewayv1.ProtocolType) bool {
	return listenermerge.ServableProtocol(protocol)
}

// gatewayInvalidListeners summarises, across a Gateway's own listeners, how
// many are invalid: conflicted (named in conflicted), a protocol this
// controller cannot serve, or an allowedRoutes namespace selector that does
// not parse. Per the Gateway API spec (gateway_types.go), a Gateway holding
// any invalid listener is marked ListenersNotValid, and one holding no valid
// listener at all is Accepted=False. Returns (any invalid, all invalid, the
// message naming the causes present and the listeners still accepted).
func gatewayInvalidListeners(
	listeners []gatewayv1.Listener,
	conflicted map[gatewayv1.SectionName]bool,
) (bool, bool, string) {
	if len(listeners) == 0 {
		return false, false, ""
	}

	invalid, unsupported, badSelector := 0, 0, 0

	var conflictedNames, acceptedNames []string

	for i := range listeners {
		switch {
		case conflicted[listeners[i].Name]:
			conflictedNames = append(conflictedNames, string(listeners[i].Name))
			invalid++
		case !servableListenerProtocol(listeners[i].Protocol):
			unsupported++
			invalid++
		case routebinding.NamespaceSelectorInvalid(listeners[i].AllowedRoutes):
			badSelector++
			invalid++
		default:
			acceptedNames = append(acceptedNames, string(listeners[i].Name))
		}
	}

	var causes []string

	if len(conflictedNames) > 0 {
		causes = append(causes, "Gateway has conflicted listeners: "+strings.Join(conflictedNames, ", "))
	}

	if unsupported > 0 {
		causes = append(causes, "one or more listeners use a protocol this controller does not serve "+
			"(only HTTP and HTTPS are supported)")
	}

	if badSelector > 0 {
		causes = append(causes, "one or more listeners have an invalid allowedRoutes.namespaces.selector")
	}

	if invalid > 0 && len(acceptedNames) > 0 {
		causes = append(causes, "accepted listeners: "+strings.Join(acceptedNames, ", "))
	}

	return invalid > 0, invalid == len(listeners), strings.Join(causes, "; ")
}

// gatewayAcceptedCondition builds the Gateway-level Accepted condition. The
// default is Accepted=True/Accepted; it is downgraded to ListenersNotValid when
// the Gateway holds conflicted own listeners, listeners whose protocol this
// controller cannot serve, or listeners whose allowedRoutes namespace
// selector does not parse, and to Accepted=False when no listener is valid at
// all (gateway_types.go).
func gatewayAcceptedCondition(
	gwView *gatewayListenerView,
	gateway *gatewayv1.Gateway,
	now metav1.Time,
) metav1.Condition {
	accepted := metav1.Condition{
		Type:               string(gatewayv1.GatewayConditionAccepted),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: gateway.Generation,
		LastTransitionTime: now,
		Reason:             string(gatewayv1.GatewayReasonAccepted),
		Message:            msgGatewayAccepted,
	}

	// Scoped to the Gateway's OWN listeners by design: an invalid listener
	// contributed by an attached ListenerSet carries its verdict on the
	// ListenerSet's own status, not on the parent Gateway's Accepted condition.
	conflicted := gatewayConflictedListeners(gwView)
	if anyInvalid, allInvalid, message := gatewayInvalidListeners(gateway.Spec.Listeners, conflicted); anyInvalid {
		accepted.Reason = string(gatewayv1.GatewayReasonListenersNotValid)
		accepted.Message = message

		if allInvalid {
			accepted.Status = metav1.ConditionFalse
		}
	}

	return accepted
}

// hostnameCaptureRisk reports whether a listener (Gateway-owned or ListenerSet
// entry — both expose the same hostname + allowedRoutes fields) combines
// allowedRoutes.namespaces.from: All with no hostname pin, the hostname-capture
// vector. Only an EXPLICIT from: All triggers it (the unset default is Same, and
// a hostname pin bounds even All-namespaces to that hostname).
func hostnameCaptureRisk(hostname *gatewayv1.Hostname, allowed *gatewayv1.AllowedRoutes) bool {
	if hostname != nil && *hostname != "" {
		return false
	}

	if allowed == nil || allowed.Namespaces == nil || allowed.Namespaces.From == nil {
		return false
	}

	return *allowed.Namespaces.From == gatewayv1.NamespacesFromAll
}

// permissiveHostnameCondition builds the advisory capture-risk condition for a
// listener flagged by hostnameCaptureRisk.
func permissiveHostnameCondition(generation int64, now metav1.Time) metav1.Condition {
	return metav1.Condition{
		Type:               listenerConditionPermissiveHostname,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             listenerReasonUnpinnedHostnameAllowsAll,
		Message:            listenerMsgPermissiveHostname,
	}
}

// validateTLSCertificateRefs validates TLS certificate references for a listener.
// Returns the condition status, reason, and message for the ResolvedRefs
// condition, or an error when a ReferenceGrant or Secret could not be read.
// Per Gateway API spec, TLS certificateRefs must point to valid Secrets of type
// kubernetes.io/tls, and cross-namespace references require ReferenceGrant.
func (r *GatewayReconciler) validateTLSCertificateRefs(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	listener *gatewayv1.Listener,
) (metav1.ConditionStatus, string, string, error) {
	// No TLS config - nothing to validate
	if listener.TLS == nil || len(listener.TLS.CertificateRefs) == 0 {
		return metav1.ConditionTrue,
			string(gatewayv1.ListenerReasonResolvedRefs),
			"References resolved", nil
	}

	for _, ref := range listener.TLS.CertificateRefs {
		status, reason, msg, err := r.validateSingleCertRef(ctx, gateway, ref)
		if err != nil || status == metav1.ConditionFalse {
			return status, reason, msg, err
		}
	}

	return metav1.ConditionTrue,
		string(gatewayv1.ListenerReasonResolvedRefs),
		msgReferencesResolved, nil
}

// validateSingleCertRef validates a single certificate reference.
func (r *GatewayReconciler) validateSingleCertRef(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	ref gatewayv1.SecretObjectReference,
) (metav1.ConditionStatus, string, string, error) {
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
	if !isCoreSecret(refGroup, refKind) {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Unsupported certificate ref kind: %s/%s", refGroup, refKind), nil
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
			return "", "", "", err
		}

		if !allowed {
			return metav1.ConditionFalse,
				string(gatewayv1.ListenerReasonRefNotPermitted),
				fmt.Sprintf("Cross-namespace reference to %s/%s not permitted", refNamespace, ref.Name), nil
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
) (metav1.ConditionStatus, string, string, error) {
	secret := &corev1.Secret{}

	err := r.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      string(ref.Name),
	}, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return metav1.ConditionFalse,
				string(gatewayv1.ListenerReasonInvalidCertificateRef),
				fmt.Sprintf("Secret %s/%s not found", namespace, ref.Name), nil
		}

		return "", "", "", errors.Wrapf(err, "getting Secret %s/%s", namespace, ref.Name)
	}

	// Validate Secret type
	if secret.Type != corev1.SecretTypeTLS {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Secret %s/%s is not of type kubernetes.io/tls", namespace, ref.Name), nil
	}

	// Validate certificate data exists and is valid PEM
	certData, hasCert := secret.Data[corev1.TLSCertKey]
	if !hasCert || len(certData) == 0 {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Secret %s/%s missing tls.crt data", namespace, ref.Name), nil
	}

	keyData, hasKey := secret.Data[corev1.TLSPrivateKeyKey]
	if !hasKey || len(keyData) == 0 {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Secret %s/%s missing tls.key data", namespace, ref.Name), nil
	}

	// Validate that certificate contains valid PEM data
	block, _ := pem.Decode(certData)
	if block == nil {
		return metav1.ConditionFalse,
			string(gatewayv1.ListenerReasonInvalidCertificateRef),
			fmt.Sprintf("Secret %s/%s contains invalid certificate PEM data", namespace, ref.Name), nil
	}

	return metav1.ConditionTrue, "", "", nil
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
	return checkSecretReferenceGrantForGateway(ctx, r.Client, gateway, targetNamespace, ref)
}
