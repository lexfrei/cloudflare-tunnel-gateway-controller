package controller

import (
	"context"
	"slices"
	"strings"

	"github.com/cockroachdb/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/parentref"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

const (
	listenerSetMsgAccepted     = "ListenerSet accepted by cloudflare-tunnel controller"
	listenerSetMsgProgrammed   = "ListenerSet programmed against parent Gateway"
	listenerSetMsgNotAllowed   = "Parent Gateway does not allow ListenerSet attachment"
	listenerSetMsgListenersBad = "No listener in this ListenerSet is usable: each one conflicts, " +
		"has unresolved references, uses a protocol this controller does not serve " +
		"or has an invalid allowedRoutes.namespaces.selector"
	listenerSetMsgConflicted          = "One or more listeners in this ListenerSet conflict with another listener"
	listenerSetMsgUnresolvedRefs      = "One or more listeners in this ListenerSet have unresolved references"
	listenerSetMsgUnsupportedProtocol = "One or more listeners in this ListenerSet use a protocol " +
		"this controller does not serve (only HTTP and HTTPS are supported)"
	listenerSetMsgInvalidSelector = "One or more listeners in this ListenerSet have an invalid " +
		"allowedRoutes.namespaces.selector"
)

// ListenerSetReconciler reconciles ListenerSet resources that target Gateways
// managed by this controller. It updates ListenerSet status with the
// Accepted, Programmed, and per-entry listener conditions derived from the
// parent Gateway's allowedListeners filter and the precedence-ordered
// conflict view computed by internal/listenermerge.
type ListenerSetReconciler struct {
	client.Client

	Scheme         *runtime.Scheme
	ControllerName string

	// ViewStore caches the per-Gateway ListenerSet merge view across reconciles
	// (issue #332). Shared with the other reconcilers. May be nil.
	ViewStore *mergeViewStore
}

// Reconcile is the main entrypoint. It is safe to call against non-existent
// ListenerSets — the call returns cleanly when the object has been deleted.
func (r *ListenerSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var listenerSet gatewayv1.ListenerSet
	if err := r.Get(ctx, req.NamespacedName, &listenerSet); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, errors.Wrap(err, "failed to get listenerset")
	}

	parent, found, err := r.fetchParentGateway(ctx, &listenerSet)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !found {
		// Parent Gateway has not been created yet or has been deleted; nothing
		// to do until the parent appears.
		return ctrl.Result{}, nil
	}

	managed, err := r.isManagedGateway(ctx, parent)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !managed {
		// Different controller owns the parent Gateway; stay out of its way.
		return ctrl.Result{}, nil
	}

	logger.Info("reconciling listenerset",
		"name", listenerSet.Name, "namespace", listenerSet.Namespace,
		"gateway", parent.Name)

	if err := r.reconcileStatus(ctx, &listenerSet, parent); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// fetchParentGateway returns the Gateway referenced by spec.parentRef.
// Defaults parentRef.namespace to the ListenerSet's namespace when unset.
func (r *ListenerSetReconciler) fetchParentGateway(
	ctx context.Context,
	listenerSet *gatewayv1.ListenerSet,
) (*gatewayv1.Gateway, bool, error) {
	parentNamespace := listenerSet.Namespace
	if listenerSet.Spec.ParentRef.Namespace != nil && *listenerSet.Spec.ParentRef.Namespace != "" {
		parentNamespace = string(*listenerSet.Spec.ParentRef.Namespace)
	}

	var gateway gatewayv1.Gateway

	key := types.NamespacedName{
		Name:      string(listenerSet.Spec.ParentRef.Name),
		Namespace: parentNamespace,
	}

	if err := r.Get(ctx, key, &gateway); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}

		return nil, false, errors.Wrap(err, "failed to get parent gateway")
	}

	return &gateway, true, nil
}

func (r *ListenerSetReconciler) isManagedGateway(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) (bool, error) {
	classes, err := managedClassNames(ctx, r.Client, r.ControllerName)
	if err != nil {
		return false, errors.Wrap(err, "failed to list managed gateway classes")
	}

	return classes[string(gateway.Spec.GatewayClassName)], nil
}

// listenerSetStatusStale reports whether the freshly-fetched ListenerSet
// already carries status conditions stamped with a generation newer than
// reconciledGen, in which case this reconcile MUST NOT overwrite the status
// (observedGeneration regression guard). Only our own conditions count,
// top-level and per-entry; a foreign condition's generation is unrelated.
func listenerSetStatusStale(reconciledGen int64, listenerSet *gatewayv1.ListenerSet) bool {
	if ownedConditionsStale(listenerSet.Status.Conditions, reconciledGen,
		string(gatewayv1.ListenerSetConditionAccepted),
		string(gatewayv1.ListenerSetConditionProgrammed),
	) {
		return true
	}

	entryConds := make([][]metav1.Condition, 0, len(listenerSet.Status.Listeners))
	for i := range listenerSet.Status.Listeners {
		entryConds = append(entryConds, listenerSet.Status.Listeners[i].Conditions)
	}

	return ownedListenerConditionsStale(reconciledGen, entryConds...)
}

// reconcileStatus computes and writes the ListenerSet status (top-level
// Accepted/Programmed plus per-entry listener conditions). It uses
// retry.RetryOnConflict so a parallel reconcile cannot lose updates.
func (r *ListenerSetReconciler) reconcileStatus(
	ctx context.Context,
	listenerSet *gatewayv1.ListenerSet,
	gateway *gatewayv1.Gateway,
) error {
	key := types.NamespacedName{Name: listenerSet.Name, Namespace: listenerSet.Namespace}

	var evalErr error

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh gatewayv1.ListenerSet
		if err := r.Get(ctx, key, &fresh); err != nil {
			return errors.Wrap(err, "failed to get fresh listenerset")
		}

		// Skip if a newer reconcile already advanced this ListenerSet's status
		// past the generation we observed (observedGeneration regression guard).
		if listenerSetStatusStale(listenerSet.Generation, &fresh) {
			return nil
		}

		now := metav1.Now()

		var acceptance listenerSetAcceptanceResult

		acceptance, evalErr = r.computeAcceptance(ctx, gateway, &fresh)

		conditions := buildListenerSetAggregateConditions(fresh.Generation, now, acceptance)
		for _, cond := range conditions {
			meta.SetStatusCondition(&fresh.Status.Conditions, cond)
		}

		priorEntries := fresh.Status.Listeners

		var entries []gatewayv1.ListenerEntryStatus

		if acceptance.Accepted || acceptance.Reason == gatewayv1.ListenerSetReasonListenersNotValid {
			// ListenersNotValid is an entry-level verdict, with Accepted
			// True while some entry is usable and False when none is: the
			// entries conflict, have unresolved refs, use an unservable
			// protocol or have an invalid allowedRoutes selector. Then, as
			// when fully accepted, the per-entry status is what users need
			// — surface it from the merge view + refChecks.
			entries = buildListenerSetEntryStatuses(&fresh, acceptance, fresh.Generation, now)
		} else {
			// Resource-level rejection (NotAllowed / Pending / Invalid) —
			// stamp the same reason on every entry so kubectl describe
			// shows a coherent story.
			entries = buildListenerSetRejectedEntryStatuses(&fresh, acceptance, fresh.Generation, now)
		}

		fresh.Status.Listeners = preserveListenerEntryTransitions(priorEntries, entries)

		if err := r.Status().Update(ctx, &fresh); err != nil {
			return errors.Wrap(err, "failed to update listenerset status")
		}

		return nil
	})
	if err != nil {
		return errors.Wrap(err, "failed to update listenerset status after retries")
	}

	return errors.Wrap(evalErr, "computing listenerset status")
}

// listenerSetAcceptanceResult bundles the data the status writer needs in
// either branch (gateway-level allow + per-listener conflict view + per-
// entry TLS verdicts).
type listenerSetAcceptanceResult struct {
	Accepted    bool
	Reason      gatewayv1.ListenerSetConditionReason
	Message     string
	MergeResult *listenermerge.MergeResult
	// RefChecks maps entry name to TLS-ref resolution verdict. Used both to
	// roll up the aggregate Accepted condition (any entry with
	// ResolvedRefs=False counts as invalid) and to surface per-entry
	// ResolvedRefs condition.
	RefChecks map[gatewayv1.SectionName]listenerEntryRefsCheck
	// AttachedRoutes maps entry name to the number of routes whose
	// parentRef targets this ListenerSet AND whose binding to that entry was
	// Accepted. Populated only when the ListenerSet itself is accepted by
	// the parent Gateway.
	AttachedRoutes map[gatewayv1.SectionName]int32
}

// computeAcceptance returns, with the result the reconcile writes, the error
// that makes the reconcile retry: one that left the acceptance undecided comes
// with a Pending result; a sibling ListenerSet left out of the merged view
// because it could not be evaluated, or an attachedRoutes count that could not
// finish, comes with the acceptance as computed and the counts already
// written.
//
//nolint:funlen // single sequential pipeline; splitting hurts readability
func (r *ListenerSetReconciler) computeAcceptance(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	listenerSet *gatewayv1.ListenerSet,
) (listenerSetAcceptanceResult, error) {
	validator := routebinding.NewValidator(r.Client)

	allowed, err := validator.EvaluateListenerSetAcceptance(ctx, gateway, listenerSet)
	if err != nil {
		return listenerSetAcceptanceResult{
			Accepted: false,
			Reason:   gatewayv1.ListenerSetReasonPending,
			Message:  "The controller could not evaluate the parent Gateway's allowedListeners; the controller log names the error",
		}, errors.Wrap(err, "evaluating the parent Gateway's allowedListeners")
	}

	if allowed.Err != nil {
		// This reconcile runs once per ListenerSet, where the route passes run
		// once per route, so the warning an operator needs is logged here.
		logging.FromContext(ctx).Warn("parent Gateway allowedListeners selector could not be evaluated",
			"gateway", gateway.Namespace+"/"+gateway.Name,
			"listenerSet", listenerSet.Namespace+"/"+listenerSet.Name,
			"error", allowed.Err)
	}

	if !allowed.Accepted {
		message := listenerSetMsgNotAllowed
		if allowed.Message != "" {
			message = allowed.Message
		}

		return listenerSetAcceptanceResult{
			Accepted: false,
			Reason:   allowed.Reason,
			Message:  message,
		}, nil
	}

	if reason, refused := parentGatewayRefused(gateway); refused {
		return listenerSetAcceptanceResult{
			Accepted: false,
			Reason:   gatewayv1.ListenerSetReasonParentNotAccepted,
			Message:  "The parent Gateway is not accepted (reason " + reason + "); its own status names the cause",
		}, nil
	}

	// The merged view spans the parent Gateway plus every sibling ListenerSet
	// allowed to attach (including this one — it has just passed the
	// allowedListeners check above). Shared with the Gateway and route
	// reconcilers through the cross-reconcile store (issue #332).
	view, err := newListenerViewCache(r.Client, r.ViewStore).forGateway(ctx, gateway)
	if err != nil {
		return listenerSetAcceptanceResult{
			Accepted: false,
			Reason:   gatewayv1.ListenerSetReasonPending,
			Message:  "Failed to enumerate sibling ListenerSets; the controller log names the error",
		}, err
	}

	merged := view.merged

	refChecks, refErr := r.collectListenerEntryRefChecks(ctx, listenerSet)
	if refErr != nil {
		return listenerSetAcceptanceResult{
			Accepted: false,
			Reason:   gatewayv1.ListenerSetReasonPending,
			Message:  "Failed to evaluate ListenerSet TLS references; the controller log names the error",
		}, refErr
	}

	accepted, summaryReason, summaryMessage := summariseListenerSet(merged, listenerSet, refChecks)

	// A count that could not be finished keeps the counts already written and
	// leaves the acceptance as computed; the error makes the reconcile count again.
	attached, attachErr := r.countAttachedRoutesPerEntry(ctx, listenerSet)
	if attachErr != nil {
		attached = make(map[gatewayv1.SectionName]int32, len(listenerSet.Status.Listeners))
		for i := range listenerSet.Status.Listeners {
			attached[listenerSet.Status.Listeners[i].Name] = listenerSet.Status.Listeners[i].AttachedRoutes
		}
	}

	return listenerSetAcceptanceResult{
		Accepted:       accepted,
		Reason:         summaryReason,
		Message:        summaryMessage,
		MergeResult:    merged,
		RefChecks:      refChecks,
		AttachedRoutes: attached,
	}, errors.Wrap(errors.CombineErrors(view.undecided, attachErr), "completing listenerset status")
}

// countAttachedRoutesPerEntry returns the number of accepted HTTPRoutes (and
// GRPCRoutes) that bind to each entry of the ListenerSet via parentRef.
// "Accepted" mirrors RouteSyncer's binding contract: hostname intersects,
// allowedRoutes.namespaces permits the route, route kind is allowed.
//
// Per the Gateway API spec (ListenerEntryStatus.AttachedRoutes), attachment
// depends SOLELY on AllowedRoutes + ParentRefs plus the route's own Accepted
// state: "the AttachedRoutes field count MUST be set for Listeners, even if the
// Accepted condition of an individual Listener is set to False", and "Routes
// with any other value for the Accepted condition MUST NOT be included". A
// listener's own status never removes a route it matched from the count; the
// route's Accepted verdict for this ListenerSet is read from its own status.
func (r *ListenerSetReconciler) countAttachedRoutesPerEntry(
	ctx context.Context,
	listenerSet *gatewayv1.ListenerSet,
) (map[gatewayv1.SectionName]int32, error) {
	out := make(map[gatewayv1.SectionName]int32, len(listenerSet.Spec.Listeners))
	for i := range listenerSet.Spec.Listeners {
		out[listenerSet.Spec.Listeners[i].Name] = 0
	}

	var httpRoutes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &httpRoutes); err != nil {
		return nil, errors.Wrap(err, "failed to list httproutes")
	}

	var grpcRoutes gatewayv1.GRPCRouteList
	if err := r.List(ctx, &grpcRoutes); err != nil {
		return nil, errors.Wrap(err, "failed to list grpcroutes")
	}

	routes := make([]Route, 0, len(httpRoutes.Items)+len(grpcRoutes.Items))
	for i := range httpRoutes.Items {
		routes = append(routes, HTTPRouteWrapper{&httpRoutes.Items[i]})
	}

	for i := range grpcRoutes.Items {
		routes = append(routes, GRPCRouteWrapper{&grpcRoutes.Items[i]})
	}

	validator := routebinding.NewValidator(r.Client)

	for _, route := range routes {
		if err := incrementListenerSetAttachedRoutes(ctx, validator, r.ControllerName, listenerSet, route, out); err != nil {
			return nil, err
		}
	}

	return out, nil
}

func incrementListenerSetAttachedRoutes(
	ctx context.Context,
	validator *routebinding.Validator,
	controllerName string,
	listenerSet *gatewayv1.ListenerSet,
	route Route,
	counts map[gatewayv1.SectionName]int32,
) error {
	routeNamespace := route.GetNamespace()

	// A single route counts at most once per listener entry, even if it
	// lists the same ListenerSet in multiple parentRefs (degenerate but
	// legal). Track which sections this route already counted.
	countedThisRoute := make(map[gatewayv1.SectionName]struct{})

	for _, ref := range route.GetParentRefs() {
		if !parentRefSelectsListenerSet(ref, routeNamespace, listenerSet) ||
			!parentRefAcceptedInStatus(route.GetParentStatuses(), ref, routeNamespace, controllerName) {
			continue
		}

		routeInfo := &routebinding.RouteInfo{
			Name:        route.GetName(),
			Namespace:   routeNamespace,
			Hostnames:   route.GetHostnames(),
			Kind:        route.GetRouteKind(),
			SectionName: ref.SectionName,
			Port:        ref.Port,
		}

		// The Accepted gate above reads the route's own status; binding here
		// only names the entries the ref attaches to.
		result, err := validator.ValidateBindingForListenerSet(ctx, listenerSet, routeInfo)
		if err == nil && result.Incomplete {
			err = errListenerNotEvaluated
		}

		if err != nil {
			return errors.Wrapf(err, "binding %s %s/%s", route.GetRouteKind(), routeNamespace, route.GetName())
		}

		if !result.Accepted {
			continue
		}

		for _, section := range result.MatchedListeners {
			if _, already := countedThisRoute[section]; already {
				continue
			}

			countedThisRoute[section] = struct{}{}
			counts[section]++
		}
	}

	return nil
}

// parentRefSelectsListenerSet returns true when a route parentRef targets
// the given ListenerSet — Group=gateway.networking.k8s.io (or unset),
// Kind=ListenerSet, and name/namespace match.
func parentRefSelectsListenerSet(
	ref gatewayv1.ParentReference,
	routeNamespace string,
	listenerSet *gatewayv1.ListenerSet,
) bool {
	if !parentref.InGatewayAPIGroup(ref) {
		return false
	}

	if ref.Kind == nil || string(*ref.Kind) != kindListenerSet {
		return false
	}

	if string(ref.Name) != listenerSet.Name {
		return false
	}

	namespace := routeNamespace
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}

	return namespace == listenerSet.Namespace
}

// collectListenerEntryRefChecks runs TLS cert ref validation for every entry
// in the ListenerSet. Returns a map keyed by entry name so the status writer
// can stamp per-entry ResolvedRefs conditions.
func (r *ListenerSetReconciler) collectListenerEntryRefChecks(
	ctx context.Context,
	listenerSet *gatewayv1.ListenerSet,
) (map[gatewayv1.SectionName]listenerEntryRefsCheck, error) {
	out := make(map[gatewayv1.SectionName]listenerEntryRefsCheck, len(listenerSet.Spec.Listeners))

	for i := range listenerSet.Spec.Listeners {
		entry := &listenerSet.Spec.Listeners[i]

		check, err := resolveListenerEntryRefs(ctx, r.Client, listenerSet, entry)
		if err != nil {
			return nil, err
		}

		out[entry.Name] = check
	}

	return out, nil
}

// summariseListenerSet rolls the merged-view per-listener status plus the
// per-entry TLS verdicts into the ListenerSet's top-level Accepted /
// Programmed conditions. An entry is usable when it is conflict-free, its
// references resolve, its protocol is one this controller serves and its
// allowedRoutes namespace selector parses. The contract, per the vendored
// ListenerSetReasonListenersNotValid:
//
//   - No usable entry → Accepted=False / Reason=ListenersNotValid.
//   - A usable entry beside an unusable one → Accepted=True /
//     Reason=ListenersNotValid, naming each cause present.
//   - Otherwise → Accepted=True / Reason=Accepted.
func summariseListenerSet(
	merged *listenermerge.MergeResult,
	listenerSet *gatewayv1.ListenerSet,
	refChecks map[gatewayv1.SectionName]listenerEntryRefsCheck,
) (bool, gatewayv1.ListenerSetConditionReason, string) {
	usable := false

	var causes []string

	for i := range listenerSet.Spec.Listeners {
		problem := listenerSetEntryProblem(merged, listenerSet, &listenerSet.Spec.Listeners[i], refChecks)
		if problem == "" {
			usable = true

			continue
		}

		if !slices.Contains(causes, problem) {
			causes = append(causes, problem)
		}
	}

	if !usable {
		return false, gatewayv1.ListenerSetReasonListenersNotValid, listenerSetMsgListenersBad
	}

	if len(causes) > 0 {
		return true, gatewayv1.ListenerSetReasonListenersNotValid, strings.Join(causes, "; ")
	}

	return true, gatewayv1.ListenerSetReasonAccepted, "ListenerSet attached to parent Gateway"
}

// listenerSetEntryProblem returns the aggregate message for why an entry is
// not usable, or "" when it is.
func listenerSetEntryProblem(
	merged *listenermerge.MergeResult,
	listenerSet *gatewayv1.ListenerSet,
	entry *gatewayv1.ListenerEntry,
	refChecks map[gatewayv1.SectionName]listenerEntryRefsCheck,
) string {
	if mergedEntry := findMergedEntry(merged, listenerSet, entry.Name); mergedEntry != nil && mergedEntry.ConflictReason != "" {
		return listenerSetMsgConflicted
	}

	if check, ok := refChecks[entry.Name]; ok && check.Status == metav1.ConditionFalse {
		return listenerSetMsgUnresolvedRefs
	}

	if !servableListenerProtocol(entry.Protocol) {
		return listenerSetMsgUnsupportedProtocol
	}

	if routebinding.NamespaceSelectorInvalid(entry.AllowedRoutes) {
		return listenerSetMsgInvalidSelector
	}

	return ""
}

// listenerSetTargetsGateway returns true when the ListenerSet's spec.parentRef
// names the given Gateway. Defaults parentRef namespace to the ListenerSet's
// namespace.
func listenerSetTargetsGateway(listenerSet *gatewayv1.ListenerSet, gateway *gatewayv1.Gateway) bool {
	if string(listenerSet.Spec.ParentRef.Name) != gateway.Name {
		return false
	}

	parentNamespace := listenerSet.Namespace
	if listenerSet.Spec.ParentRef.Namespace != nil && *listenerSet.Spec.ParentRef.Namespace != "" {
		parentNamespace = string(*listenerSet.Spec.ParentRef.Namespace)
	}

	return parentNamespace == gateway.Namespace
}

func buildListenerSetAggregateConditions(
	generation int64,
	now metav1.Time,
	result listenerSetAcceptanceResult,
) []metav1.Condition {
	acceptedStatus := metav1.ConditionFalse
	programmedStatus := metav1.ConditionFalse

	if result.Accepted {
		acceptedStatus = metav1.ConditionTrue
		programmedStatus = metav1.ConditionTrue
	}

	programmedReason := result.Reason
	if result.Accepted {
		programmedReason = gatewayv1.ListenerSetReasonProgrammed
	}

	return []metav1.Condition{
		{
			Type:               string(gatewayv1.ListenerSetConditionAccepted),
			Status:             acceptedStatus,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(result.Reason),
			Message:            listenerSetMessageForAccepted(result),
		},
		{
			Type:               string(gatewayv1.ListenerSetConditionProgrammed),
			Status:             programmedStatus,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(programmedReason),
			Message:            listenerSetMessageForProgrammed(result),
		},
	}
}

func listenerSetMessageForAccepted(result listenerSetAcceptanceResult) string {
	if result.Accepted && result.Reason == gatewayv1.ListenerSetReasonListenersNotValid && result.Message != "" {
		return result.Message
	}

	if result.Accepted {
		return listenerSetMsgAccepted
	}

	if result.Message != "" {
		return result.Message
	}

	return string(result.Reason)
}

func listenerSetMessageForProgrammed(result listenerSetAcceptanceResult) string {
	if result.Accepted {
		return listenerSetMsgProgrammed
	}

	if result.Reason == gatewayv1.ListenerSetReasonListenersNotValid {
		return listenerSetMsgListenersBad
	}

	if result.Message != "" {
		return result.Message
	}

	return string(result.Reason)
}

// buildListenerSetEntryStatuses produces the per-entry listener status when
// the ListenerSet itself is allowed by the parent Gateway. The conflict
// reason on each MergedListener decides whether that entry's Accepted /
// Programmed / Conflicted conditions surface success or rejection.
func buildListenerSetEntryStatuses(
	listenerSet *gatewayv1.ListenerSet,
	acceptance listenerSetAcceptanceResult,
	generation int64,
	now metav1.Time,
) []gatewayv1.ListenerEntryStatus {
	out := make([]gatewayv1.ListenerEntryStatus, 0, len(listenerSet.Spec.Listeners))

	for i := range listenerSet.Spec.Listeners {
		entry := &listenerSet.Spec.Listeners[i]
		merge := findMergedEntry(acceptance.MergeResult, listenerSet, entry.Name)

		supportedKinds, hasValidKind, hasInvalidKind := routebinding.FilterSupportedKinds(entry.AllowedRoutes, entry.Protocol)
		if !hasValidKind {
			supportedKinds = []gatewayv1.RouteGroupKind{}
		}

		refCheck, hasRefCheck := acceptance.RefChecks[entry.Name]
		attached := int32(0)

		if counts, ok := acceptance.AttachedRoutes[entry.Name]; ok {
			attached = counts
		}

		conditions := listenerEntryConditions(
			generation, now, merge, entry.Protocol, entry.AllowedRoutes, hasValidKind, hasInvalidKind, refCheck, hasRefCheck,
		)

		// Advisory (#476): a tenant-authored ListenerSet entry with the
		// hostname-capture combination — allowedRoutes.namespaces.from: All and no
		// hostname pin — is the highest-risk case (the entry author may be
		// untrusted), so surface the same cf.k8s.lex.la/PermissiveHostname
		// advisory the Gateway-listener path raises. Only on a serving entry: a
		// conflicted entry already reports Accepted=False (conflictedEntryConditions),
		// so the Accepted check alone also excludes conflicts.
		accepted := meta.FindStatusCondition(conditions, string(gatewayv1.ListenerConditionAccepted))
		if accepted != nil && accepted.Status == metav1.ConditionTrue &&
			hostnameCaptureRisk(entry.Hostname, entry.AllowedRoutes) {
			conditions = append(conditions, permissiveHostnameCondition(generation, now))
		}

		out = append(out, gatewayv1.ListenerEntryStatus{
			Name:           entry.Name,
			SupportedKinds: supportedKinds,
			AttachedRoutes: attached,
			Conditions:     conditions,
		})
	}

	return out
}

// buildListenerSetRejectedEntryStatuses produces a uniform per-entry status
// when the ListenerSet has been rejected at the resource level (allowedListeners
// said no, or the parent state is Pending). Every entry reports the same reason
// for clarity in `kubectl describe`. A ListenersNotValid rejection is built per
// entry by buildListenerSetEntryStatuses instead.
func buildListenerSetRejectedEntryStatuses(
	listenerSet *gatewayv1.ListenerSet,
	result listenerSetAcceptanceResult,
	generation int64,
	now metav1.Time,
) []gatewayv1.ListenerEntryStatus {
	out := make([]gatewayv1.ListenerEntryStatus, 0, len(listenerSet.Spec.Listeners))

	rejectionReason := listenerEntryReasonForListenerSetRejection(result.Reason)

	for i := range listenerSet.Spec.Listeners {
		entry := &listenerSet.Spec.Listeners[i]

		supportedKinds, hasValidKind, _ := routebinding.FilterSupportedKinds(entry.AllowedRoutes, entry.Protocol)
		if !hasValidKind {
			supportedKinds = []gatewayv1.RouteGroupKind{}
		}

		out = append(out, gatewayv1.ListenerEntryStatus{
			Name:           entry.Name,
			SupportedKinds: supportedKinds,
			// Zero, not a real count: a ListenerSet rejected at resource level
			// is not a valid attachment point. A ParentNotAccepted ListenerSet
			// is still bound by spec, so a route on it may report its own
			// parent verdict; that verdict, not this count, says whether it is
			// served. A conflicted entry on an accepted ListenerSet does count.
			AttachedRoutes: 0,
			Conditions: []metav1.Condition{
				{
					Type:               string(gatewayv1.ListenerConditionAccepted),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: generation,
					LastTransitionTime: now,
					Reason:             rejectionReason,
					Message:            result.Message,
				},
				{
					Type:               string(gatewayv1.ListenerConditionProgrammed),
					Status:             metav1.ConditionFalse,
					ObservedGeneration: generation,
					LastTransitionTime: now,
					Reason:             rejectionReason,
					Message:            result.Message,
				},
				rejectedEntryResolvedRefsCondition(generation, now, result),
			},
		})
	}

	return out
}

// rejectedEntryResolvedRefsCondition produces the ResolvedRefs condition for
// per-entry status when the ListenerSet is rejected at the resource level.
// For "Pending" (e.g. transient TLS-ref evaluation error) we MUST NOT report
// a confident "True", because that would be a misleading signal next to an
// aggregate that says "we don't know yet". Other resource-level rejections
// don't depend on refs being resolved either way and report True.
func rejectedEntryResolvedRefsCondition(
	generation int64,
	now metav1.Time,
	result listenerSetAcceptanceResult,
) metav1.Condition {
	if result.Reason == gatewayv1.ListenerSetReasonPending {
		return metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             metav1.ConditionUnknown,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerSetReasonPending),
			Message:            result.Message,
		}
	}

	return metav1.Condition{
		Type:               string(gatewayv1.ListenerConditionResolvedRefs),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		LastTransitionTime: now,
		Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
		Message:            msgReferencesResolved,
	}
}

// parentGatewayRefused reports the reason of a parent Gateway's
// Accepted=False. ListenersNotValid does not count: it judges only the
// Gateway's own listeners, which a ListenerSet's entries do not depend on.
// The parent's message is not returned, since it can name objects the
// ListenerSet's owner may not be allowed to read. A verdict older than the
// Gateway's spec lasts until the Gateway's next status write, which
// requeues its ListenerSets.
func parentGatewayRefused(gateway *gatewayv1.Gateway) (string, bool) {
	accepted := meta.FindStatusCondition(gateway.Status.Conditions, string(gatewayv1.GatewayConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionFalse ||
		accepted.Reason == string(gatewayv1.GatewayReasonListenersNotValid) {
		return "", false
	}

	return accepted.Reason, true
}

func listenerEntryReasonForListenerSetRejection(reason gatewayv1.ListenerSetConditionReason) string {
	switch reason {
	case gatewayv1.ListenerSetReasonNotAllowed:
		return string(gatewayv1.ListenerSetReasonNotAllowed)
	case gatewayv1.ListenerSetReasonListenersNotValid:
		return string(gatewayv1.ListenerSetReasonListenersNotValid)
	case gatewayv1.ListenerSetReasonInvalid:
		return string(gatewayv1.ListenerSetReasonInvalid)
	case gatewayv1.ListenerSetReasonParentNotAccepted:
		return string(gatewayv1.ListenerSetReasonParentNotAccepted)
	case gatewayv1.ListenerSetReasonPending:
		return string(gatewayv1.ListenerSetReasonPending)
	case gatewayv1.ListenerSetReasonAccepted, gatewayv1.ListenerSetReasonProgrammed:
		return string(gatewayv1.ListenerReasonPending)
	}

	return string(gatewayv1.ListenerReasonPending)
}

// findMergedEntry locates the MergedListener corresponding to a particular
// (listenerSet, sectionName) pair within the merged view.
func findMergedEntry(
	merged *listenermerge.MergeResult,
	listenerSet *gatewayv1.ListenerSet,
	name gatewayv1.SectionName,
) *listenermerge.MergedListener {
	if merged == nil {
		return nil
	}

	for i := range merged.Listeners {
		entry := &merged.Listeners[i]
		if entry.ParentKind != listenermerge.ParentKindListenerSet {
			continue
		}

		if entry.ListenerSet != nil &&
			entry.ListenerSet.Namespace == listenerSet.Namespace &&
			entry.ListenerSet.Name == listenerSet.Name &&
			entry.Name == name {
			return entry
		}
	}

	return nil
}

// findMergedGatewayEntry returns the merged-view entry for the Gateway's own
// listener named name (ParentKind==Gateway), or nil when absent. The Gateway
// analogue of findMergedEntry.
func findMergedGatewayEntry(
	merged *listenermerge.MergeResult,
	name gatewayv1.SectionName,
) *listenermerge.MergedListener {
	if merged == nil {
		return nil
	}

	for i := range merged.Listeners {
		entry := &merged.Listeners[i]
		if entry.ParentKind == listenermerge.ParentKindGateway && entry.Name == name {
			return entry
		}
	}

	return nil
}

// listenerEntryConditions produces the per-entry condition slice from a
// merged-view entry plus the per-entry TLS-ref verdict. Conflict reasons
// drive the Accepted/Programmed/Conflicted trio; clean entries report
// success. The TLS-ref verdict, when present, overrides the kind-derived
// ResolvedRefs condition.
func listenerEntryConditions(
	generation int64,
	now metav1.Time,
	merge *listenermerge.MergedListener,
	protocol gatewayv1.ProtocolType,
	allowedRoutes *gatewayv1.AllowedRoutes,
	hasValidKind, hasInvalidKind bool,
	refCheck listenerEntryRefsCheck,
	hasRefCheck bool,
) []metav1.Condition {
	resolvedRefsCondition := listenerEntryResolvedRefsCondition(generation, now, hasValidKind, hasInvalidKind)

	if hasRefCheck && refCheck.Status == metav1.ConditionFalse {
		resolvedRefsCondition = metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             refCheck.Reason,
			Message:            refCheck.Message,
		}
	}

	if merge != nil && merge.ConflictReason != "" {
		return conflictedEntryConditions(generation, now, merge, &resolvedRefsCondition)
	}

	return acceptedEntryConditions(generation, now, protocol, allowedRoutes, &resolvedRefsCondition)
}

func conflictedEntryConditions(
	generation int64,
	now metav1.Time,
	merge *listenermerge.MergedListener,
	resolvedRefs *metav1.Condition,
) []metav1.Condition {
	reason := string(merge.ConflictReason)

	return []metav1.Condition{
		{
			Type:               string(gatewayv1.ListenerConditionAccepted),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             reason,
			Message:            merge.ConflictMessage,
		},
		{
			Type:               string(gatewayv1.ListenerConditionProgrammed),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             reason,
			Message:            merge.ConflictMessage,
		},
		{
			Type:               string(gatewayv1.ListenerConditionConflicted),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             reason,
			Message:            merge.ConflictMessage,
		},
		*resolvedRefs,
	}
}

func acceptedEntryConditions(
	generation int64,
	now metav1.Time,
	protocol gatewayv1.ProtocolType,
	allowedRoutes *gatewayv1.AllowedRoutes,
	resolvedRefs *metav1.Condition,
) []metav1.Condition {
	acceptedCondition := buildListenerAcceptedCondition(protocol, generation, now)
	refuseInvalidNamespaceSelector(&acceptedCondition, allowedRoutes)

	programmedStatus := metav1.ConditionTrue
	programmedReason := string(gatewayv1.ListenerReasonProgrammed)
	programmedMessage := listenerMsgProgrammed

	// An unservable protocol, an invalid allowedRoutes selector or an
	// unresolved ref each mean the entry is not programmed; the Accepted=False
	// verdict of the first two is more fundamental than the ref.
	switch {
	case acceptedCondition.Status == metav1.ConditionFalse:
		programmedStatus = metav1.ConditionFalse
		programmedReason = string(gatewayv1.ListenerReasonInvalid)
		programmedMessage = acceptedCondition.Message
	case resolvedRefs.Status == metav1.ConditionFalse:
		programmedStatus = metav1.ConditionFalse
		programmedReason = string(gatewayv1.ListenerReasonInvalid)
		programmedMessage = listenerMsgInvalidUnresolved
	}

	return []metav1.Condition{
		acceptedCondition,
		{
			Type:               string(gatewayv1.ListenerConditionProgrammed),
			Status:             programmedStatus,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             programmedReason,
			Message:            programmedMessage,
		},
		{
			Type:               string(gatewayv1.ListenerConditionConflicted),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonNoConflicts),
			Message:            "Listener does not conflict with any other listener",
		},
		*resolvedRefs,
	}
}

func listenerEntryResolvedRefsCondition(
	generation int64,
	now metav1.Time,
	hasValidKind, hasInvalidKind bool,
) metav1.Condition {
	switch {
	case !hasValidKind:
		return metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonInvalidRouteKinds),
			Message:            listenerMsgNoSupportedRouteKinds,
		}
	case hasInvalidKind:
		return metav1.Condition{
			Type:               string(gatewayv1.ListenerConditionResolvedRefs),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonInvalidRouteKinds),
			Message:            listenerMsgInvalidRouteKinds,
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

// SetupWithManager registers the ListenerSet reconciler with controller
// runtime. We watch every input the ListenerSet's status depends on so the
// status field never drifts from the live cluster state:
//
//   - ListenerSet itself (For).
//   - Gateway changes (allowedListeners flips, listeners added/removed change
//     conflict markers).
//   - HTTPRoute / GRPCRoute changes (AttachedRoutes count).
//   - Secret changes (TLS cert refs → ResolvedRefs).
//   - ReferenceGrant changes (cross-namespace cert refs).
func (r *ListenerSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	//nolint:wrapcheck // controller-runtime builder pattern
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.ListenerSet{}).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayToListenerSets),
		).
		Watches(
			&gatewayv1.HTTPRoute{},
			handler.EnqueueRequestsFromMapFunc(r.routeToListenerSets),
		).
		Watches(
			&gatewayv1.GRPCRoute{},
			handler.EnqueueRequestsFromMapFunc(r.routeToListenerSets),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToListenerSets),
		).
		Watches(
			&gatewayv1beta1.ReferenceGrant{},
			handler.EnqueueRequestsFromMapFunc(r.referenceGrantToListenerSets),
		).
		Complete(r)
}

// routeToListenerSets maps an HTTPRoute / GRPCRoute event to reconcile
// requests for every ListenerSet whose entry the route's parentRefs target.
// Without this, AttachedRoutes count would drift when routes are created /
// edited / deleted.
func (r *ListenerSetReconciler) routeToListenerSets(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	parentRefs, routeNamespace := extractRouteParentRefs(obj)
	if len(parentRefs) == 0 {
		return nil
	}

	return r.collectListenerSetsForParentRefs(ctx, parentRefs, routeNamespace)
}

// extractRouteParentRefs returns the parentRefs and namespace for an
// HTTPRoute / GRPCRoute event, or (nil, "") if the object is neither.
func extractRouteParentRefs(obj client.Object) ([]gatewayv1.ParentReference, string) {
	switch route := obj.(type) {
	case *gatewayv1.HTTPRoute:
		return route.Spec.ParentRefs, route.Namespace
	case *gatewayv1.GRPCRoute:
		return route.Spec.ParentRefs, route.Namespace
	}

	return nil, ""
}

// collectListenerSetsForParentRefs walks the route's parentRefs, finds every
// ListenerSet referenced (by Kind=ListenerSet), and returns reconcile
// requests for each so its AttachedRoutes count can update.
func (r *ListenerSetReconciler) collectListenerSetsForParentRefs(
	ctx context.Context,
	parentRefs []gatewayv1.ParentReference,
	routeNamespace string,
) []reconcile.Request {
	seen := make(map[types.NamespacedName]struct{})
	requests := make([]reconcile.Request, 0)

	for _, ref := range parentRefs {
		if !parentref.InGatewayAPIGroup(ref) {
			continue
		}

		if ref.Kind == nil || string(*ref.Kind) != kindListenerSet {
			continue
		}

		namespace := routeNamespace
		if ref.Namespace != nil {
			namespace = string(*ref.Namespace)
		}

		key := types.NamespacedName{Name: string(ref.Name), Namespace: namespace}
		if _, ok := seen[key]; ok {
			continue
		}

		var listenerSet gatewayv1.ListenerSet
		if err := r.Get(ctx, key, &listenerSet); err != nil {
			continue
		}

		seen[key] = struct{}{}
		requests = append(requests, reconcile.Request{NamespacedName: key})
	}

	return requests
}

// secretToListenerSets maps a Secret event to reconcile requests for every
// ListenerSet that references the Secret in a TLS cert ref. A new Secret
// appearing or an existing one disappearing must re-evaluate
// ResolvedRefs on every dependent ListenerSet entry.
func (r *ListenerSetReconciler) secretToListenerSets(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	var all gatewayv1.ListenerSetList
	if err := r.List(ctx, &all); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0)

	for i := range all.Items {
		listenerSet := &all.Items[i]
		if !listenerSetReferencesSecret(listenerSet, secret.Namespace, secret.Name) {
			continue
		}

		requests = append(requests, reconcile.Request{
			Name: listenerSet.Name, Namespace: listenerSet.Namespace,
		})
	}

	return requests
}

// referenceGrantToListenerSets maps a ReferenceGrant event to reconcile
// requests for every ListenerSet that needs a ReferenceGrant from
// Kind=ListenerSet to a Secret in the grant's namespace. Without this, a
// ReferenceGrant create/delete leaves dependent ListenerSets with stale
// ResolvedRefs status.
func (r *ListenerSetReconciler) referenceGrantToListenerSets(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	grant, ok := obj.(*gatewayv1beta1.ReferenceGrant)
	if !ok {
		return nil
	}

	if !grantTargetsSecret(grant) {
		return nil
	}

	var all gatewayv1.ListenerSetList
	if err := r.List(ctx, &all); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0)

	for i := range all.Items {
		listenerSet := &all.Items[i]
		if !listenerSetReferencesSecretsInNamespace(listenerSet, grant.Namespace) {
			continue
		}

		requests = append(requests, reconcile.Request{
			Name: listenerSet.Name, Namespace: listenerSet.Namespace,
		})
	}

	return requests
}

// listenerSetReferencesSecret returns true when any TLS cert ref on any
// entry of the ListenerSet points at the given Secret.
func listenerSetReferencesSecret(listenerSet *gatewayv1.ListenerSet, namespace, name string) bool {
	for i := range listenerSet.Spec.Listeners {
		entry := &listenerSet.Spec.Listeners[i]
		if entry.TLS == nil {
			continue
		}

		for _, ref := range entry.TLS.CertificateRefs {
			refNamespace := listenerSet.Namespace
			if ref.Namespace != nil {
				refNamespace = string(*ref.Namespace)
			}

			if refNamespace == namespace && string(ref.Name) == name {
				return true
			}
		}
	}

	return false
}

// listenerSetReferencesSecretsInNamespace returns true when any TLS cert ref
// on any entry of the ListenerSet points at a Secret in the given namespace
// (the grant's target namespace).
func listenerSetReferencesSecretsInNamespace(listenerSet *gatewayv1.ListenerSet, namespace string) bool {
	for i := range listenerSet.Spec.Listeners {
		entry := &listenerSet.Spec.Listeners[i]
		if entry.TLS == nil {
			continue
		}

		for _, ref := range entry.TLS.CertificateRefs {
			refNamespace := listenerSet.Namespace
			if ref.Namespace != nil {
				refNamespace = string(*ref.Namespace)
			}

			if refNamespace == namespace {
				return true
			}
		}
	}

	return false
}

// grantTargetsSecret returns true when the ReferenceGrant has any to-entry
// for a core Secret.
func grantTargetsSecret(grant *gatewayv1beta1.ReferenceGrant) bool {
	for _, target := range grant.Spec.To {
		if isCoreSecret(string(target.Group), string(target.Kind)) {
			return true
		}
	}

	return false
}

func (r *ListenerSetReconciler) gatewayToListenerSets(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	gateway, ok := obj.(*gatewayv1.Gateway)
	if !ok {
		return nil
	}

	var sets gatewayv1.ListenerSetList
	if err := r.List(ctx, &sets); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0)

	for i := range sets.Items {
		ls := &sets.Items[i]
		if listenerSetTargetsGateway(ls, gateway) {
			requests = append(requests, reconcile.Request{
				Name:      ls.Name,
				Namespace: ls.Namespace,
			})
		}
	}

	return requests
}
