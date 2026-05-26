package controller

import (
	"context"

	"github.com/cockroachdb/errors"
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

// mergedListenersFor produces the precedence-ordered, conflict-annotated
// listener view for a Gateway. Shared by GatewayReconciler (for
// status.attachedListenerSets) and ListenerSetReconciler (for the per-set
// summary).
func mergedListenersFor(
	ctx context.Context,
	cli client.Client,
	gateway *gatewayv1.Gateway,
) (*listenermerge.MergeResult, error) {
	listenerSets, err := collectAcceptedListenerSetsForGateway(ctx, cli, gateway)
	if err != nil {
		return nil, err
	}

	return listenermerge.Merge(gateway, listenerSets), nil
}
