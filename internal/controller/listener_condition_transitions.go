package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// preserveConditionTransitions carries each desired condition's
// LastTransitionTime over from the prior condition of the same type whenever
// the Status has not changed. metav1.Condition defines that timestamp as the
// last time the condition transitioned between statuses, so the per-listener
// paths — which rebuild their whole condition slice every reconcile — would
// otherwise restamp it on every pass and make status diffing and
// transition alerting see churn that never happened.
//
// Conditions the caller no longer emits are dropped. Running
// meta.SetStatusCondition straight over the prior slice would instead keep them
// forever, so a cleared advisory or a condition replaced by the conflicted set
// would never disappear.
//
// desired must not contain duplicate condition types: the duplicate would keep
// the appended prior copy while only the first occurrence is updated.
func preserveConditionTransitions(prior, desired []metav1.Condition) []metav1.Condition {
	merged := make([]metav1.Condition, 0, len(desired))

	for _, condition := range desired {
		if existing := meta.FindStatusCondition(prior, condition.Type); existing != nil {
			merged = append(merged, *existing)
		}

		meta.SetStatusCondition(&merged, condition)
	}

	return merged
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
