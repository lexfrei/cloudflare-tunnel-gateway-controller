package controller

import (
	"context"

	"github.com/cockroachdb/errors"
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

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

const (
	listenerSetMsgAccepted     = "ListenerSet accepted by cloudflare-tunnel controller"
	listenerSetMsgProgrammed   = "ListenerSet programmed against parent Gateway"
	listenerSetMsgNotAllowed   = "Parent Gateway does not allow ListenerSet attachment"
	listenerSetMsgListenersBad = "No listener in this ListenerSet is conflict-free"
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

// reconcileStatus computes and writes the ListenerSet status (top-level
// Accepted/Programmed plus per-entry listener conditions). It uses
// retry.RetryOnConflict so a parallel reconcile cannot lose updates.
func (r *ListenerSetReconciler) reconcileStatus(
	ctx context.Context,
	listenerSet *gatewayv1.ListenerSet,
	gateway *gatewayv1.Gateway,
) error {
	key := types.NamespacedName{Name: listenerSet.Name, Namespace: listenerSet.Namespace}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh gatewayv1.ListenerSet
		if err := r.Get(ctx, key, &fresh); err != nil {
			return errors.Wrap(err, "failed to get fresh listenerset")
		}

		now := metav1.Now()

		acceptance := r.computeAcceptance(ctx, gateway, &fresh)

		conditions := buildListenerSetAggregateConditions(fresh.Generation, now, acceptance)
		for _, cond := range conditions {
			meta.SetStatusCondition(&fresh.Status.Conditions, cond)
		}

		if acceptance.Accepted || acceptance.Reason == gatewayv1.ListenerSetReasonListenersNotValid {
			// Either the ListenerSet is fully accepted, or it's been
			// rejected only because individual entries failed (conflict or
			// bad refs). Either way, the per-entry status is what users
			// need — surface it from the merge view + refChecks.
			fresh.Status.Listeners = buildListenerSetEntryStatuses(&fresh, acceptance, fresh.Generation, now)
		} else {
			// Resource-level rejection (NotAllowed / Pending / Invalid) —
			// stamp the same reason on every entry so kubectl describe
			// shows a coherent story.
			fresh.Status.Listeners = buildListenerSetRejectedEntryStatuses(&fresh, acceptance, fresh.Generation, now)
		}

		if err := r.Status().Update(ctx, &fresh); err != nil {
			return errors.Wrap(err, "failed to update listenerset status")
		}

		return nil
	})

	return errors.Wrap(err, "failed to update listenerset status after retries")
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

//nolint:funlen // single sequential pipeline; splitting hurts readability
func (r *ListenerSetReconciler) computeAcceptance(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	listenerSet *gatewayv1.ListenerSet,
) listenerSetAcceptanceResult {
	validator := routebinding.NewValidator(r.Client)

	allowed, err := validator.EvaluateListenerSetAcceptance(ctx, gateway, listenerSet)
	if err != nil || !allowed.Accepted {
		reason := gatewayv1.ListenerSetReasonNotAllowed

		message := listenerSetMsgNotAllowed
		if err != nil {
			message = "Failed to evaluate parent Gateway allowedListeners: " + err.Error()
		}

		return listenerSetAcceptanceResult{
			Accepted: false,
			Reason:   reason,
			Message:  message,
		}
	}

	siblings, err := r.collectAcceptedSiblings(ctx, gateway, listenerSet)
	if err != nil {
		return listenerSetAcceptanceResult{
			Accepted: false,
			Reason:   gatewayv1.ListenerSetReasonPending,
			Message:  "Failed to enumerate sibling ListenerSets: " + err.Error(),
		}
	}

	merged := listenermerge.Merge(gateway, siblings)

	refChecks, refErr := r.collectListenerEntryRefChecks(ctx, listenerSet)
	if refErr != nil {
		return listenerSetAcceptanceResult{
			Accepted: false,
			Reason:   gatewayv1.ListenerSetReasonPending,
			Message:  "Failed to evaluate ListenerSet TLS references: " + refErr.Error(),
		}
	}

	accepted, summaryReason, summaryMessage := summariseListenerSet(merged, listenerSet, refChecks)

	attached, attachErr := r.countAttachedRoutesPerEntry(ctx, listenerSet)
	if attachErr != nil {
		return listenerSetAcceptanceResult{
			Accepted: false,
			Reason:   gatewayv1.ListenerSetReasonPending,
			Message:  "Failed to count attached routes: " + attachErr.Error(),
		}
	}

	return listenerSetAcceptanceResult{
		Accepted:       accepted,
		Reason:         summaryReason,
		Message:        summaryMessage,
		MergeResult:    merged,
		RefChecks:      refChecks,
		AttachedRoutes: attached,
	}
}

// countAttachedRoutesPerEntry returns the number of accepted HTTPRoutes (and
// GRPCRoutes) that bind to each entry of the ListenerSet via parentRef.
// "Accepted" mirrors RouteSyncer's binding contract: hostname intersects,
// allowedRoutes.namespaces permits the route, route kind is allowed.
func (r *ListenerSetReconciler) countAttachedRoutesPerEntry(
	ctx context.Context,
	listenerSet *gatewayv1.ListenerSet,
) (map[gatewayv1.SectionName]int32, error) {
	out := make(map[gatewayv1.SectionName]int32, len(listenerSet.Spec.Listeners))
	for i := range listenerSet.Spec.Listeners {
		out[listenerSet.Spec.Listeners[i].Name] = 0
	}

	validator := routebinding.NewValidator(r.Client)

	if err := r.countAttachedHTTPRoutes(ctx, listenerSet, validator, out); err != nil {
		return nil, err
	}

	if err := r.countAttachedGRPCRoutes(ctx, listenerSet, validator, out); err != nil {
		return nil, err
	}

	return out, nil
}

//nolint:dupl // mirrored on purpose against countAttachedGRPCRoutes — different list type prevents a generic
func (r *ListenerSetReconciler) countAttachedHTTPRoutes(
	ctx context.Context,
	listenerSet *gatewayv1.ListenerSet,
	validator *routebinding.Validator,
	counts map[gatewayv1.SectionName]int32,
) error {
	var routes gatewayv1.HTTPRouteList
	if err := r.List(ctx, &routes); err != nil {
		return errors.Wrap(err, "failed to list httproutes")
	}

	for i := range routes.Items {
		route := &routes.Items[i]
		incrementListenerSetAttachedRoutes(
			ctx, validator, listenerSet,
			route.Namespace, route.Name, route.Spec.Hostnames,
			routebinding.KindHTTPRoute, route.Spec.ParentRefs, counts,
		)
	}

	return nil
}

//nolint:dupl // mirrored on purpose against countAttachedHTTPRoutes — different list type prevents a generic
func (r *ListenerSetReconciler) countAttachedGRPCRoutes(
	ctx context.Context,
	listenerSet *gatewayv1.ListenerSet,
	validator *routebinding.Validator,
	counts map[gatewayv1.SectionName]int32,
) error {
	var routes gatewayv1.GRPCRouteList
	if err := r.List(ctx, &routes); err != nil {
		return errors.Wrap(err, "failed to list grpcroutes")
	}

	for i := range routes.Items {
		route := &routes.Items[i]
		incrementListenerSetAttachedRoutes(
			ctx, validator, listenerSet,
			route.Namespace, route.Name, route.Spec.Hostnames,
			routebinding.KindGRPCRoute, route.Spec.ParentRefs, counts,
		)
	}

	return nil
}

func incrementListenerSetAttachedRoutes(
	ctx context.Context,
	validator *routebinding.Validator,
	listenerSet *gatewayv1.ListenerSet,
	routeNamespace, routeName string,
	hostnames []gatewayv1.Hostname,
	kind gatewayv1.Kind,
	parentRefs []gatewayv1.ParentReference,
	counts map[gatewayv1.SectionName]int32,
) {
	for _, ref := range parentRefs {
		if !parentRefSelectsListenerSet(ref, routeNamespace, listenerSet) {
			continue
		}

		routeInfo := &routebinding.RouteInfo{
			Name:        routeName,
			Namespace:   routeNamespace,
			Hostnames:   hostnames,
			Kind:        kind,
			SectionName: ref.SectionName,
			Port:        ref.Port,
		}

		result, err := validator.ValidateBindingForListenerSet(ctx, listenerSet, routeInfo)
		if err != nil || !result.Accepted {
			continue
		}

		for _, section := range result.MatchedListeners {
			counts[section]++
		}
	}
}

// parentRefSelectsListenerSet returns true when a route parentRef targets
// the given ListenerSet — Kind=ListenerSet and name/namespace match.
func parentRefSelectsListenerSet(
	ref gatewayv1.ParentReference,
	routeNamespace string,
	listenerSet *gatewayv1.ListenerSet,
) bool {
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
// Programmed conditions. The contract:
//
//   - At least one entry must be conflict-free AND ResolvedRefs-True →
//     Accepted=True / Reason=Accepted.
//   - Otherwise → Accepted=False / Reason=ListenersNotValid.
func summariseListenerSet(
	merged *listenermerge.MergeResult,
	listenerSet *gatewayv1.ListenerSet,
	refChecks map[gatewayv1.SectionName]listenerEntryRefsCheck,
) (bool, gatewayv1.ListenerSetConditionReason, string) {
	for i := range listenerSet.Spec.Listeners {
		entry := &listenerSet.Spec.Listeners[i]
		mergedEntry := findMergedEntry(merged, listenerSet, entry.Name)

		if mergedEntry != nil && mergedEntry.ConflictReason != "" {
			continue
		}

		if check, ok := refChecks[entry.Name]; ok && check.Status == metav1.ConditionFalse {
			continue
		}

		return true, gatewayv1.ListenerSetReasonAccepted, "ListenerSet attached to parent Gateway"
	}

	return false, gatewayv1.ListenerSetReasonListenersNotValid, listenerSetMsgListenersBad
}

// collectAcceptedSiblings returns the slice of ListenerSets — including the
// one being reconciled — that point at the parent Gateway and are themselves
// allowed by the Gateway's allowedListeners filter. This is the input to the
// precedence + conflict view so that hostname/protocol conflicts between
// siblings are visible.
func (r *ListenerSetReconciler) collectAcceptedSiblings(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	current *gatewayv1.ListenerSet,
) ([]*gatewayv1.ListenerSet, error) {
	var all gatewayv1.ListenerSetList
	if err := r.List(ctx, &all); err != nil {
		return nil, errors.Wrap(err, "failed to list listenersets")
	}

	validator := routebinding.NewValidator(r.Client)
	siblings := make([]*gatewayv1.ListenerSet, 0, len(all.Items))

	for i := range all.Items {
		item := &all.Items[i]

		if !listenerSetTargetsGateway(item, gateway) {
			continue
		}

		// Always include the current ListenerSet (we've already checked it).
		if item.Name == current.Name && item.Namespace == current.Namespace {
			siblings = append(siblings, item)

			continue
		}

		acceptance, err := validator.EvaluateListenerSetAcceptance(ctx, gateway, item)
		if err != nil {
			return nil, errors.Wrap(err, "failed to evaluate sibling listenerset")
		}

		if acceptance.Accepted {
			siblings = append(siblings, item)
		}
	}

	return siblings, nil
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

		out = append(out, gatewayv1.ListenerEntryStatus{
			Name:           entry.Name,
			SupportedKinds: supportedKinds,
			AttachedRoutes: attached,
			Conditions: listenerEntryConditions(
				generation, now, merge, hasValidKind, hasInvalidKind, refCheck, hasRefCheck,
			),
		})
	}

	return out
}

// buildListenerSetRejectedEntryStatuses produces a uniform per-entry status
// when the ListenerSet has been rejected at the resource level (allowedListeners
// said no, or all entries conflicted). Every entry reports the same reason
// for clarity in `kubectl describe`.
func buildListenerSetRejectedEntryStatuses(
	listenerSet *gatewayv1.ListenerSet,
	result listenerSetAcceptanceResult,
	generation int64,
	now metav1.Time,
) []gatewayv1.ListenerEntryStatus {
	out := make([]gatewayv1.ListenerEntryStatus, 0, len(listenerSet.Spec.Listeners))

	// Reason "NotAllowed" stamps the resource-level rejection on the entry
	// level only when there is no per-entry merge view (i.e. the resource was
	// disallowed). The per-entry reason for ListenersNotValid is more
	// specific and built from the merge view.
	if result.Reason == gatewayv1.ListenerSetReasonListenersNotValid && result.MergeResult != nil {
		return buildListenerSetEntryStatuses(listenerSet, result, generation, now)
	}

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
				{
					Type:               string(gatewayv1.ListenerConditionResolvedRefs),
					Status:             metav1.ConditionTrue,
					ObservedGeneration: generation,
					LastTransitionTime: now,
					Reason:             string(gatewayv1.ListenerReasonResolvedRefs),
					Message:            msgReferencesResolved,
				},
			},
		})
	}

	return out
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

// listenerEntryConditions produces the per-entry condition slice from a
// merged-view entry plus the per-entry TLS-ref verdict. Conflict reasons
// drive the Accepted/Programmed/Conflicted trio; clean entries report
// success. The TLS-ref verdict, when present, overrides the kind-derived
// ResolvedRefs condition.
func listenerEntryConditions(
	generation int64,
	now metav1.Time,
	merge *listenermerge.MergedListener,
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

	return acceptedEntryConditions(generation, now, &resolvedRefsCondition)
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
	resolvedRefs *metav1.Condition,
) []metav1.Condition {
	programmedStatus := metav1.ConditionTrue
	programmedReason := string(gatewayv1.ListenerReasonProgrammed)
	programmedMessage := listenerMsgProgrammed

	if resolvedRefs.Status == metav1.ConditionFalse {
		programmedStatus = metav1.ConditionFalse
		programmedReason = string(gatewayv1.ListenerReasonInvalid)
		programmedMessage = listenerMsgInvalidUnresolved
	}

	return []metav1.Condition{
		{
			Type:               string(gatewayv1.ListenerConditionAccepted),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: generation,
			LastTransitionTime: now,
			Reason:             string(gatewayv1.ListenerReasonAccepted),
			Message:            listenerMsgAccepted,
		},
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
			Reason:             "NoConflicts",
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
// runtime, watching ListenerSet resources directly and Gateway changes that
// might flip AllowedListeners (re-evaluating all ListenerSets pointed at the
// Gateway).
func (r *ListenerSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	//nolint:wrapcheck // controller-runtime builder pattern
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1.ListenerSet{}).
		Watches(
			&gatewayv1.Gateway{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayToListenerSets),
		).
		Complete(r)
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
				NamespacedName: types.NamespacedName{
					Name:      ls.Name,
					Namespace: ls.Namespace,
				},
			})
		}
	}

	return requests
}
