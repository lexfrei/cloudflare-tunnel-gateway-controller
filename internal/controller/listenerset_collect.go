package controller

import (
	"context"

	"github.com/cockroachdb/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// collectAcceptedListenerSetsForGateway returns every ListenerSet that names
// the given Gateway as its spec.parentRef AND is permitted to attach by the
// Gateway's spec.allowedListeners filter.
//
// Used by GatewayReconciler to compute status.attachedListenerSets and by
// ListenerSetReconciler to compute the merged view that drives status
// conditions for the ListenerSet under reconciliation.
func collectAcceptedListenerSetsForGateway(
	ctx context.Context,
	cli client.Client,
	gateway *gatewayv1.Gateway,
) ([]*gatewayv1.ListenerSet, error) {
	var all gatewayv1.ListenerSetList
	if err := cli.List(ctx, &all); err != nil {
		return nil, errors.Wrap(err, "failed to list listenersets")
	}

	validator := routebinding.NewValidator(cli)
	out := make([]*gatewayv1.ListenerSet, 0, len(all.Items))

	for i := range all.Items {
		listenerSet := &all.Items[i]

		if !listenerSetTargetsGateway(listenerSet, gateway) {
			continue
		}

		acceptance, err := validator.EvaluateListenerSetAcceptance(ctx, gateway, listenerSet)
		if err != nil {
			return nil, errors.Wrap(err, "failed to evaluate listenerset acceptance")
		}

		if acceptance.Accepted {
			out = append(out, listenerSet)
		}
	}

	return out, nil
}

// summariseAttachedListenerSets re-applies the same per-ListenerSet
// acceptance contract the ListenerSetReconciler uses — at least one entry
// must be conflict-free AND have ResolvedRefs=True — and returns the count
// of ListenerSets that pass.
//
// Used by GatewayReconciler so status.attachedListenerSets matches the
// per-resource Accepted condition: a ListenerSet with all-broken TLS refs
// reports Accepted=False/ListenersNotValid, and the count here agrees.
func summariseAttachedListenerSets(
	ctx context.Context,
	cli client.Client,
	gateway *gatewayv1.Gateway,
) (int, error) {
	listenerSets, err := collectAcceptedListenerSetsForGateway(ctx, cli, gateway)
	if err != nil {
		return 0, err
	}

	merged := listenermerge.Merge(gateway, listenerSets)
	accepted := 0

	for _, listenerSet := range listenerSets {
		if listenerSetEntriesAccepted(ctx, cli, listenerSet, merged) {
			accepted++
		}
	}

	return accepted, nil
}

// listenerSetEntriesAccepted returns true when at least one entry of the
// ListenerSet is conflict-free in the merged view AND has its TLS cert refs
// resolved (or no TLS material at all). Mirrors summariseListenerSet in
// listenerset_controller.go.
func listenerSetEntriesAccepted(
	ctx context.Context,
	cli client.Client,
	listenerSet *gatewayv1.ListenerSet,
	merged *listenermerge.MergeResult,
) bool {
	for i := range listenerSet.Spec.Listeners {
		entry := &listenerSet.Spec.Listeners[i]
		mergedEntry := findMergedEntry(merged, listenerSet, entry.Name)

		if mergedEntry != nil && mergedEntry.ConflictReason != "" {
			continue
		}

		check, err := resolveListenerEntryRefs(ctx, cli, listenerSet, entry)
		if err != nil {
			continue
		}

		if check.Status == metav1.ConditionFalse {
			continue
		}

		return true
	}

	return false
}
