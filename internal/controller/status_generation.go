package controller

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// statusGenerationStale reports whether any stored condition already carries an
// observedGeneration newer than reconciledGen — the generation this reconcile
// observed and computed its status from.
//
// Gateway API requires that, in that case, the implementation MUST NOT perform
// the status update and must wait for a future reconciliation of the newer
// generation (shared_types.go: "If the observedGeneration of a Condition is
// greater than the value the implementation knows about, then it MUST NOT
// perform the update on that Condition, but must wait for a future
// reconciliation and status update").
//
// The check is strictly greater-than: an equal generation is the normal
// re-write case, so the next reconcile of the newer generation still corrects
// any transient stale write. Callers pass every condition set they own (e.g.
// the top-level conditions plus each nested listener/parent/ancestor slice).
func statusGenerationStale(reconciledGen int64, conditionSets ...[]metav1.Condition) bool {
	for _, set := range conditionSets {
		for i := range set {
			if set[i].ObservedGeneration > reconciledGen {
				return true
			}
		}
	}

	return false
}

// ownedConditionsStale reports whether any of the listed owned condition types,
// as currently stored, carries an observedGeneration newer than reconciledGen.
//
// Use this for objects whose status.conditions is a flat list that may also
// hold entries written by OTHER controllers (e.g. a `special.io/...` condition
// on a Gateway). Those foreign entries carry a generation unrelated to ours and
// Gateway API forbids us from removing or modifying them, so the regression
// guard must look only at the types this controller manages.
func ownedConditionsStale(stored []metav1.Condition, reconciledGen int64, ownedTypes ...string) bool {
	for _, condType := range ownedTypes {
		if c := apimeta.FindStatusCondition(stored, condType); c != nil && c.ObservedGeneration > reconciledGen {
			return true
		}
	}

	return false
}
