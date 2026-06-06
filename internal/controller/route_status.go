package controller

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// routeAccessor provides type-agnostic access to Gateway API route fields
// needed for status updates. The obj field is used for Get/Update calls,
// while the accessor functions read from the populated object.
type routeAccessor struct {
	obj         client.Object
	parentRefs  func() []gatewayv1.ParentReference
	routeStatus func() *gatewayv1.RouteStatus
	generation  func() int64
	ruleCount   func() int
}

// routeStatusUpdateParams holds the common parameters for updating route status.
type routeStatusUpdateParams struct {
	k8sClient      client.Client
	controllerName string
	// acceptedOverride, when non-nil, downgrades an otherwise-Accepted route to
	// Accepted=False with this reason/message. The GRPCRoute reconciler sets it
	// for gRPC served over an explicit quic tunnel (cloudflared drops HTTP
	// trailers over QUIC, so grpc-status is lost). A sync error or a binding
	// rejection is a more specific problem and takes precedence, so the override
	// only applies when the route would otherwise be Accepted=True.
	acceptedOverride *acceptedConditionOverride
	// diagnostics are the converter's per-route findings about config that will
	// not be served exactly as written (e.g. unsupported filters). The status
	// writer turns them into an Accepted=False/UnsupportedValue override (when
	// every rule is wholly unservable) or a PartiallyInvalid=True condition
	// (when only some rules/backends are affected and the route still serves).
	diagnostics []proxy.RouteDiagnostic
	// ruleCount is the number of rules in the route spec. It lets the status
	// writer tell "every rule is unservable" (Accepted=False) apart from "some
	// rules dropped" (PartiallyInvalid=True).
	ruleCount int
	// reconciledGeneration is the route's metadata.generation at the time this
	// reconcile read the spec and computed its conclusions. It is used to skip
	// the write when a newer reconcile has already advanced our entries past it
	// (see statusGenerationStale); the writer still stamps observedGeneration
	// from the freshly-fetched generation.
	reconciledGeneration int64
}

// acceptedConditionOverride carries the reason/message used to downgrade an
// otherwise-Accepted route condition to Accepted=False.
type acceptedConditionOverride struct {
	reason  string
	message string
}

// routeParentStatusMaxCount is the CRD cap on RouteStatus.Parents
// (+kubebuilder:validation:MaxItems=32 in shared_types.go). Exceeding it makes
// the Status().Update fail validation, so on overflow we truncate only our own
// entries and never the ones owned by other controllers.
const routeParentStatusMaxCount = 32

// updateRouteStatusGeneric updates the status of a route with per-parent binding conditions.
// It fetches a fresh copy, builds parent status entries, and writes the update with retry.
func updateRouteStatusGeneric(
	ctx context.Context,
	params *routeStatusUpdateParams,
	routeKey types.NamespacedName,
	newAccessor func() routeAccessor,
	bindingInfo routeBindingInfo,
	failedRefs []ingress.BackendRefError,
	syncErr error,
) error {
	// Compute managed class names once outside the retry loop.
	// The set does not change between retries (conflict retries happen in milliseconds).
	classNames, classErr := managedClassNames(ctx, params.k8sClient, params.controllerName)
	if classErr != nil {
		logging.FromContext(ctx).Warn("failed to get managed class names for route status update",
			"error", classErr)

		classNames = nil
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return updateRouteParentStatuses(
			ctx, params, routeKey, newAccessor(), classNames, bindingInfo, failedRefs, syncErr,
		)
	})

	return errors.Wrap(err, "failed to update route status after retries")
}

// updateRouteParentStatuses fetches a fresh route, builds parent statuses, and writes the update.
func updateRouteParentStatuses(
	ctx context.Context,
	params *routeStatusUpdateParams,
	routeKey types.NamespacedName,
	accessor routeAccessor,
	classNames map[string]bool,
	bindingInfo routeBindingInfo,
	failedRefs []ingress.BackendRefError,
	syncErr error,
) error {
	if err := params.k8sClient.Get(ctx, routeKey, accessor.obj); err != nil {
		return errors.Wrap(err, "failed to get fresh route")
	}

	now := metav1.Now()
	routeStatus := accessor.routeStatus()

	// Gateway API requires updating only RouteParentStatus entries whose
	// controllerName matches ours, and MUST NOT touch entries owned by other
	// controllers co-managing this Route. Keep foreign entries verbatim and
	// rebuild only our own below. Mirrors the BackendTLSPolicy ancestor-status
	// writer (backendtlspolicy_controller.go updateStatus).
	foreignEntries := make([]gatewayv1.RouteParentStatus, 0, len(routeStatus.Parents))

	for _, parent := range routeStatus.Parents {
		if string(parent.ControllerName) != params.controllerName {
			foreignEntries = append(foreignEntries, parent)

			continue
		}

		// A newer reconcile already advanced our entry past the generation we
		// reconciled; skip the write and let that reconcile own the status
		// (Gateway API MUST NOT overwrite a condition stamped with a newer
		// observedGeneration).
		if statusGenerationStale(params.reconciledGeneration, parent.Conditions) {
			return nil
		}
	}

	// ruleCount comes from the freshly-fetched spec so the diagnostic
	// aggregation can tell "every rule unservable" from "some rules dropped".
	params.ruleCount = accessor.ruleCount()

	ours := make([]gatewayv1.RouteParentStatus, 0, len(accessor.parentRefs()))

	for refIdx, ref := range accessor.parentRefs() {
		parentStatus := resolveParentRefStatus(
			ctx, params, accessor, ref, refIdx, classNames, now, bindingInfo, failedRefs, syncErr,
		)
		if parentStatus != nil {
			ours = append(ours, *parentStatus)
		}
	}

	// Reserve the foreign controllers' slots first and truncate only our own
	// entries on overflow — other controllers' status MUST NOT be dropped.
	available := max(routeParentStatusMaxCount-len(foreignEntries), 0)
	if len(ours) > available {
		ours = ours[:available]
	}

	combined := ours
	combined = append(combined, foreignEntries...)
	routeStatus.Parents = combined

	if err := params.k8sClient.Status().Update(ctx, accessor.obj); err != nil {
		return errors.Wrap(err, "failed to update route status")
	}

	return nil
}

// resolveParentRefStatus builds a RouteParentStatus for a single parentRef,
// or returns nil if the ref doesn't belong to a managed Gateway (directly or
// via a ListenerSet attached to one).
func resolveParentRefStatus(
	ctx context.Context,
	params *routeStatusUpdateParams,
	accessor routeAccessor,
	ref gatewayv1.ParentReference,
	refIdx int,
	classNames map[string]bool,
	now metav1.Time,
	bindingInfo routeBindingInfo,
	failedRefs []ingress.BackendRefError,
	syncErr error,
) *gatewayv1.RouteParentStatus {
	if !parentRefSelectsManagedGateway(ctx, params.k8sClient, ref, accessor.obj.GetNamespace(), classNames) {
		return nil
	}

	namespace := accessor.obj.GetNamespace()
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}

	parentStatus := buildParentStatus(
		ref, namespace, params.controllerName,
		accessor.generation(), now,
		bindingInfo, refIdx,
		failedRefs, syncErr,
		params.acceptedOverride,
		params.diagnostics, params.ruleCount,
	)

	return &parentStatus
}

// parentRefSelectsManagedGateway returns true when a route parentRef
// ultimately targets a Gateway managed by this controller — either directly
// (Kind=Gateway) or via a ListenerSet whose parent Gateway is managed. A ref
// with an unrecognised Group (anything other than the Gateway API group or
// empty/default) returns false so a foreign-group ListenerSet name collision
// cannot poison the route's status.parents entries.
func parentRefSelectsManagedGateway(
	ctx context.Context,
	cli client.Client,
	ref gatewayv1.ParentReference,
	routeNamespace string,
	classNames map[string]bool,
) bool {
	gateway, found := resolveParentGatewayFromRef(ctx, cli, ref, routeNamespace)
	if !found {
		return false
	}

	return classNames[string(gateway.Spec.GatewayClassName)]
}

// buildParentStatus constructs a RouteParentStatus entry for a single parent ref.
func buildParentStatus(
	ref gatewayv1.ParentReference,
	namespace string,
	controllerName string,
	generation int64,
	now metav1.Time,
	bindingInfo routeBindingInfo,
	refIdx int,
	failedRefs []ingress.BackendRefError,
	syncErr error,
	acceptedOverride *acceptedConditionOverride,
	diagnostics []proxy.RouteDiagnostic,
	ruleCount int,
) gatewayv1.RouteParentStatus {
	parentNS := gatewayv1.Namespace(namespace)

	// Derive the Accepted override and the optional PartiallyInvalid condition
	// from the converter diagnostics. A caller-supplied override (e.g. the
	// GRPCRoute reconciler's gRPC-over-quic case) is more specific, so it wins
	// over a diagnostic-derived whole-route override.
	diagOverride, partiallyInvalid := diagnosticConditions(diagnostics, ruleCount, generation, now)
	if acceptedOverride == nil {
		acceptedOverride = diagOverride
	}

	accepted := buildAcceptedCondition(generation, now, bindingInfo, refIdx, syncErr, acceptedOverride)

	conditions := []metav1.Condition{
		accepted,
		buildResolvedRefsCondition(generation, now, failedRefs, diagnostics),
	}

	// PartiallyInvalid is only meaningful when the route is otherwise accepted —
	// the spec mandates it be set only to True, alongside Accepted=True. If the
	// whole route was rejected, the rejection already tells the full story.
	if partiallyInvalid != nil && accepted.Status == metav1.ConditionTrue {
		conditions = append(conditions, *partiallyInvalid)
	}

	return gatewayv1.RouteParentStatus{
		ParentRef: gatewayv1.ParentReference{
			Group:       ref.Group,
			Kind:        ref.Kind,
			Namespace:   &parentNS,
			Name:        ref.Name,
			Port:        ref.Port,
			SectionName: ref.SectionName,
		},
		ControllerName: gatewayv1.GatewayController(controllerName),
		Conditions:     conditions,
	}
}

// diagnosticConditions derives, from the converter's per-route Accepted-target
// diagnostics, either an Accepted=False/UnsupportedValue override (when every
// rule of the route is wholly unservable) or a PartiallyInvalid=True condition
// (when only some rules or backend fractions are affected and the route still
// serves the rest). Returns (nil, nil) when there are no Accepted-target
// diagnostics. At most one of the two results is non-nil.
func diagnosticConditions(
	diagnostics []proxy.RouteDiagnostic,
	ruleCount int,
	generation int64,
	now metav1.Time,
) (*acceptedConditionOverride, *metav1.Condition) {
	var accepted []proxy.RouteDiagnostic

	wholeRuleIdx := make(map[int]struct{})

	for _, diag := range diagnostics {
		if diag.Target != proxy.DiagnosticAccepted {
			continue
		}

		accepted = append(accepted, diag)

		if diag.WholeRule {
			wholeRuleIdx[diag.RuleIndex] = struct{}{}
		}
	}

	if len(accepted) == 0 {
		return nil, nil
	}

	// Every rule wholly unservable → the route cannot be served at all.
	if ruleCount > 0 && len(wholeRuleIdx) >= ruleCount {
		return &acceptedConditionOverride{
			reason:  string(gatewayv1.RouteReasonUnsupportedValue),
			message: droppedConfigMessage(accepted, false),
		}, nil
	}

	// Otherwise some rules/backends are dropped but the route still serves.
	return nil, &metav1.Condition{
		Type:               string(gatewayv1.RouteConditionPartiallyInvalid),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             string(gatewayv1.RouteReasonUnsupportedValue),
		Message:            droppedConfigMessage(accepted, true),
	}
}

// droppedConfigMessage builds the human-facing condition message for a set of
// Accepted-target diagnostics. When partial is true the message starts with the
// "Dropped Rule" prefix the Gateway API spec mandates for the drop-rule
// PartiallyInvalid approach and lists the affected rule indices; the per-cause
// detail follows so the operator sees both which rules and why.
func droppedConfigMessage(diagnostics []proxy.RouteDiagnostic, partial bool) string {
	ruleIdx := make([]int, 0, len(diagnostics))
	seenIdx := make(map[int]struct{})

	details := make([]string, 0, len(diagnostics))
	seenMsg := make(map[string]struct{})

	for _, diag := range diagnostics {
		if _, ok := seenIdx[diag.RuleIndex]; !ok {
			seenIdx[diag.RuleIndex] = struct{}{}

			ruleIdx = append(ruleIdx, diag.RuleIndex)
		}

		if _, ok := seenMsg[diag.Message]; !ok {
			seenMsg[diag.Message] = struct{}{}

			details = append(details, diag.Message)
		}
	}

	slices.Sort(ruleIdx)

	idxStrs := make([]string, 0, len(ruleIdx))
	for _, idx := range ruleIdx {
		idxStrs = append(idxStrs, strconv.Itoa(idx))
	}

	detail := strings.Join(details, " ")
	if !partial {
		return detail
	}

	return "Dropped Rule " + strings.Join(idxStrs, ", ") + ": " + detail
}

func buildAcceptedCondition(
	generation int64,
	now metav1.Time,
	bindingInfo routeBindingInfo,
	refIdx int,
	syncErr error,
	override *acceptedConditionOverride,
) metav1.Condition {
	status := metav1.ConditionTrue
	reason := string(gatewayv1.RouteReasonAccepted)
	message := routeAcceptedMessage

	if syncErr != nil {
		status = metav1.ConditionFalse
		reason = string(gatewayv1.RouteReasonPending)
		message = syncErr.Error()
	} else if bindingResult, hasBinding := bindingInfo.bindingResults[refIdx]; hasBinding && !bindingResult.Accepted {
		status = metav1.ConditionFalse
		reason = string(bindingResult.Reason)
		message = bindingResult.Message
	} else if override != nil {
		// The route binds fine but cannot be served as written (e.g. gRPC over an
		// explicit quic tunnel). Lowest precedence: a sync error or binding
		// rejection above is the more specific problem.
		status = metav1.ConditionFalse
		reason = override.reason
		message = override.message
	}

	return metav1.Condition{
		Type:               string(gatewayv1.RouteConditionAccepted),
		Status:             status,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	}
}

func buildResolvedRefsCondition(
	generation int64,
	now metav1.Time,
	failedRefs []ingress.BackendRefError,
	diagnostics []proxy.RouteDiagnostic,
) metav1.Condition {
	status := metav1.ConditionTrue
	reason := string(gatewayv1.RouteReasonResolvedRefs)
	message := resolvedRefsMessage

	switch {
	case len(failedRefs) > 0:
		// A hard unresolved backend reference (missing Service, bad kind/port,
		// unauthorized cross-namespace) is the most fundamental problem and
		// outranks softer converter diagnostics. Gateway API spec requires
		// specific reasons like InvalidKind, RefNotPermitted, BackendNotFound.
		status = metav1.ConditionFalse
		reason = failedRefs[0].Reason

		if reason == "" {
			reason = string(gatewayv1.RouteReasonRefNotPermitted)
		}

		message = buildFailedRefsMessage(failedRefs)
	default:
		// Otherwise fold in any ResolvedRefs-target converter diagnostic (e.g. a
		// backend declaring a TLS appProtocol with no BackendTLSPolicy, or an
		// unrecognised appProtocol). These reach the status only via the
		// converter — the ingress builder does not see them.
		if diag, ok := firstResolvedRefsDiagnostic(diagnostics); ok {
			status = metav1.ConditionFalse
			reason = diag.Reason

			if reason == "" {
				reason = string(gatewayv1.RouteReasonUnsupportedProtocol)
			}

			message = diag.Message
		}
	}

	return metav1.Condition{
		Type:               string(gatewayv1.RouteConditionResolvedRefs),
		Status:             status,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	}
}

// firstResolvedRefsDiagnostic returns the first ResolvedRefs-target diagnostic,
// if any. The status writer surfaces it on the ResolvedRefs condition. A single
// condition carries one reason/message, so when a route has several ResolvedRefs
// problems (e.g. a dropped mirror and an unpoliced TLS appProtocol) only the
// first is surfaced on the condition — the rest remain in the converter logs.
// This mirrors the existing failedRefs[0] behaviour for the ingress-builder
// reasons; the Accepted/PartiallyInvalid path, by contrast, does aggregate all
// of its messages.
func firstResolvedRefsDiagnostic(diagnostics []proxy.RouteDiagnostic) (proxy.RouteDiagnostic, bool) {
	for _, diag := range diagnostics {
		if diag.Target == proxy.DiagnosticResolvedRefs {
			return diag, true
		}
	}

	return proxy.RouteDiagnostic{}, false
}

// Event reason / action tokens for a benign config override. Kubernetes Events
// require CamelCase machine-readable reason and action tokens distinct from the
// human-readable note.
const (
	eventReasonConfigOverridden = "ConfigOverridden"
	eventActionConvert          = "Convert"

	// Event reason / action tokens for the GRPCRoute edge-toggle breadcrumb (see
	// emitGRPCEdgeHint). The Cloudflare zone gRPC toggle is dashboard-only with no
	// API to read, so the controller cannot validate it; this Normal Event is the
	// only in-cluster signal an operator gets when the edge 403s application/grpc.
	eventReasonGRPCEdgeProxyingRequired = "GRPCEdgeProxyingRequired"
	eventActionVerifyEdgeConfig         = "VerifyEdgeConfig"
)

// emitDiagnosticEvents emits a Kubernetes Event for each Event-target
// diagnostic — benign overrides that applied successfully but ignored or
// superseded a hint (e.g. an appProtocol cleartext hint overridden by a
// BackendTLSPolicy, or a ResponseHeaderModifier that strips a WebSocket
// handshake header). These have no standard Gateway API condition, so an Event
// is the surface. Non-Event diagnostics are skipped — they drive conditions.
// A nil recorder is a no-op so reconcilers constructed without one (e.g. in
// unit tests) do not panic.
func emitDiagnosticEvents(recorder events.EventRecorder, route runtime.Object, diagnostics []proxy.RouteDiagnostic) {
	if recorder == nil {
		return
	}

	for _, diag := range diagnostics {
		if diag.Target != proxy.DiagnosticEvent {
			continue
		}

		eventType := diag.EventType
		if eventType != corev1.EventTypeWarning {
			eventType = corev1.EventTypeNormal
		}

		// Eventf treats the note as a format string. diag.Message is already
		// fully formatted (and may contain a literal %), so pass it as a "%s"
		// argument rather than as the format itself.
		recorder.Eventf(route, nil, eventType, eventReasonConfigOverridden, eventActionConvert, "%s", diag.Message)
	}
}

func buildFailedRefsMessage(failedRefs []ingress.BackendRefError) string {
	var msgBuilder strings.Builder

	msgBuilder.WriteString("Backend references not permitted: ")

	for i, failedRef := range failedRefs {
		if i > 0 {
			msgBuilder.WriteString(", ")
		}

		msgBuilder.WriteString(failedRef.BackendNS + "/" + failedRef.BackendName)
	}

	return msgBuilder.String()
}

// routeStatusEntry represents a single route that needs status update.
type routeStatusEntry struct {
	name        string
	namespace   string
	bindingInfo routeBindingInfo
	failedRefs  []ingress.BackendRefError
	diagnostics []proxy.RouteDiagnostic
	update      func(ctx context.Context, bindingInfo routeBindingInfo, failedRefs []ingress.BackendRefError, diagnostics []proxy.RouteDiagnostic, syncErr error) error
}

// updateRoutesStatus iterates over route entries and updates status for each.
// Returns the first error encountered (for requeue with backoff).
func updateRoutesStatus(
	ctx context.Context,
	logger interface{ Error(msg string, args ...any) },
	entries []routeStatusEntry,
	syncErr error,
) error {
	var firstErr error

	for _, entry := range entries {
		if err := entry.update(ctx, entry.bindingInfo, entry.failedRefs, entry.diagnostics, syncErr); err != nil {
			logger.Error("failed to update route status", "error", err, "route", entry.namespace+"/"+entry.name)

			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

// filterFailedRefs returns failed backend refs that belong to the specified route.
func filterFailedRefs(allFailedRefs []ingress.BackendRefError, routeNamespace, routeName string) []ingress.BackendRefError {
	var result []ingress.BackendRefError

	for _, failedRef := range allFailedRefs {
		if failedRef.RouteNamespace == routeNamespace && failedRef.RouteName == routeName {
			result = append(result, failedRef)
		}
	}

	return result
}

// filterDiagnostics returns converter diagnostics that belong to the specified route.
func filterDiagnostics(all []proxy.RouteDiagnostic, routeNamespace, routeName string) []proxy.RouteDiagnostic {
	var result []proxy.RouteDiagnostic

	for _, diag := range all {
		if diag.Namespace == routeNamespace && diag.Name == routeName {
			result = append(result, diag)
		}
	}

	return result
}

func newHTTPRouteAccessor() routeAccessor {
	route := &gatewayv1.HTTPRoute{}

	return routeAccessor{
		obj:         route,
		parentRefs:  func() []gatewayv1.ParentReference { return route.Spec.ParentRefs },
		routeStatus: func() *gatewayv1.RouteStatus { return &route.Status.RouteStatus },
		generation:  func() int64 { return route.Generation },
		ruleCount:   func() int { return len(route.Spec.Rules) },
	}
}

func newGRPCRouteAccessor() routeAccessor {
	route := &gatewayv1.GRPCRoute{}

	return routeAccessor{
		obj:         route,
		parentRefs:  func() []gatewayv1.ParentReference { return route.Spec.ParentRefs },
		routeStatus: func() *gatewayv1.RouteStatus { return &route.Status.RouteStatus },
		generation:  func() int64 { return route.Generation },
		ruleCount:   func() int { return len(route.Spec.Rules) },
	}
}
