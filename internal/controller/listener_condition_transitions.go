package controller

import (
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// cfConditionDomainPrefix is the prefix of every condition type this
// controller emits outside the standard Gateway API set.
const cfConditionDomainPrefix = "cf.k8s.lex.la/"

// isControllerOwnedListenerConditionType reports whether this controller is
// responsible for a per-listener condition type. Every other type belongs to
// another controller and the spec forbids removing or reordering it
// (ListenerStatus.Conditions and ListenerEntryStatus.Conditions godoc).
func isControllerOwnedListenerConditionType(condType string) bool {
	switch condType {
	case string(gatewayv1.ListenerConditionAccepted),
		string(gatewayv1.ListenerConditionProgrammed),
		string(gatewayv1.ListenerConditionResolvedRefs),
		string(gatewayv1.ListenerConditionConflicted):
		return true
	}

	return strings.HasPrefix(condType, cfConditionDomainPrefix)
}

// preserveConditionTransitions merges the conditions the caller emits this
// reconcile over the stored per-listener slice. metav1.Condition defines
// LastTransitionTime as the last time the condition transitioned between
// statuses, so an owned condition whose status did not change keeps its stored
// timestamp instead of being restamped on every pass. Controller-owned
// conditions the caller no longer emits are dropped, so a cleared advisory or
// a condition replaced by the conflicted set does not linger. Every other
// stored condition belongs to another controller and stays where it is,
// untouched.
//
// desired must not contain duplicate condition types.
func preserveConditionTransitions(prior, desired []metav1.Condition) []metav1.Condition {
	return mergeConditionTransitions(prior, desired, isControllerOwnedListenerConditionType)
}

// preserveOwnedConditionTransitions is preserveConditionTransitions' twin for
// a scope where every condition belongs to this controller -- a route's own
// RouteParentStatus entry (keyed by controllerName) or a GatewayClassConfig's
// status, both sole-owner surfaces with no foreign condition to leave
// untouched. A prior condition type absent from desired is dropped rather
// than kept, since nothing else can be responsible for reviving it.
//
// desired must not contain duplicate condition types.
func preserveOwnedConditionTransitions(prior, desired []metav1.Condition) []metav1.Condition {
	return mergeConditionTransitions(prior, desired, func(string) bool { return true })
}

// mergeConditionTransitions seeds the merge with every prior condition isOwned
// keeps -- an owned type absent from desired is dropped, everything else
// (foreign types, when isOwned is selective) carries over untouched -- then
// applies meta.SetStatusCondition for each desired condition, which preserves
// LastTransitionTime whenever the matching prior entry's Status is unchanged.
func mergeConditionTransitions(prior, desired []metav1.Condition, isOwned func(string) bool) []metav1.Condition {
	merged := make([]metav1.Condition, 0, len(prior)+len(desired))

	for i := range prior {
		if isOwned(prior[i].Type) && meta.FindStatusCondition(desired, prior[i].Type) == nil {
			continue
		}

		merged = append(merged, prior[i])
	}

	for _, condition := range desired {
		meta.SetStatusCondition(&merged, condition)
	}

	return merged
}

// ownedListenerConditionsStale reports whether any controller-owned condition
// in the given per-listener slices carries an observedGeneration newer than
// reconciledGen. Conditions of other controllers are skipped: their generation
// is unrelated to ours and must not defer our own status write.
func ownedListenerConditionsStale(reconciledGen int64, conditionSets ...[]metav1.Condition) bool {
	for _, set := range conditionSets {
		for i := range set {
			if isControllerOwnedListenerConditionType(set[i].Type) && set[i].ObservedGeneration > reconciledGen {
				return true
			}
		}
	}

	return false
}

// preserveGatewayListenerTransitions applies preserveConditionTransitions to
// every rebuilt Gateway listener status, paired with its prior status by
// listener name. A listener with no prior status keeps its fresh timestamps.
func preserveGatewayListenerTransitions(prior, desired []gatewayv1.ListenerStatus) []gatewayv1.ListenerStatus {
	for i := range desired {
		for j := range prior {
			if prior[j].Name != desired[i].Name {
				continue
			}

			desired[i].Conditions = preserveConditionTransitions(prior[j].Conditions, desired[i].Conditions)

			break
		}
	}

	return desired
}

// preserveListenerEntryTransitions is the ListenerSet twin of
// preserveGatewayListenerTransitions: same contract, different status type.
func preserveListenerEntryTransitions(prior, desired []gatewayv1.ListenerEntryStatus) []gatewayv1.ListenerEntryStatus {
	for i := range desired {
		for j := range prior {
			if prior[j].Name != desired[i].Name {
				continue
			}

			desired[i].Conditions = preserveConditionTransitions(prior[j].Conditions, desired[i].Conditions)

			break
		}
	}

	return desired
}
