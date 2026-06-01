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
// Production no longer calls this directly — every consumer goes through the
// per-Gateway merge-view cache (see buildGatewayListenerView / forGateway in
// listenerset_view.go). It is retained as the independent oracle the merge-view
// equivalence test composes against, asserting the cache yields the same
// accepted set + merge a from-scratch computation would.
func collectAcceptedListenerSetsForGateway(
	ctx context.Context,
	cli client.Client,
	gateway *gatewayv1.Gateway,
) ([]*gatewayv1.ListenerSet, error) {
	candidates, err := listTargetingListenerSets(ctx, cli, gateway)
	if err != nil {
		return nil, err
	}

	return acceptedFromCandidates(ctx, routebinding.NewValidator(cli), gateway, candidates)
}

// listTargetingListenerSets lists every ListenerSet whose spec.parentRef names
// the given Gateway, without applying the Gateway's allowedListeners filter.
// This pre-acceptance candidate set is what the merge-view cache fingerprints
// on: it captures membership and per-ListenerSet generation, which is exactly
// what the merged view depends on. The List reads the controller-runtime
// informer cache (no API round-trip).
func listTargetingListenerSets(
	ctx context.Context,
	cli client.Client,
	gateway *gatewayv1.Gateway,
) ([]*gatewayv1.ListenerSet, error) {
	var all gatewayv1.ListenerSetList
	if err := cli.List(ctx, &all); err != nil {
		return nil, errors.Wrap(err, "failed to list listenersets")
	}

	out := make([]*gatewayv1.ListenerSet, 0, len(all.Items))

	for i := range all.Items {
		if listenerSetTargetsGateway(&all.Items[i], gateway) {
			out = append(out, &all.Items[i])
		}
	}

	return out, nil
}

// acceptedFromCandidates filters targeting ListenerSets down to those the
// Gateway's spec.allowedListeners filter permits to attach.
func acceptedFromCandidates(
	ctx context.Context,
	validator *routebinding.Validator,
	gateway *gatewayv1.Gateway,
	candidates []*gatewayv1.ListenerSet,
) ([]*gatewayv1.ListenerSet, error) {
	out := make([]*gatewayv1.ListenerSet, 0, len(candidates))

	for _, listenerSet := range candidates {
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
	views *listenerViewCache,
) (int, error) {
	view, err := views.orNew(cli).forGateway(ctx, gateway)
	if err != nil {
		return 0, err
	}

	accepted := 0

	for _, listenerSet := range view.acceptedSets {
		if listenerSetEntriesAccepted(ctx, cli, listenerSet, view.merged) {
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
