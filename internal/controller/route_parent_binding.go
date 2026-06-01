package controller

import (
	"context"

	"github.com/cockroachdb/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// parentRefBinding is the per-ref binding outcome surfaced to the route
// reconcilers. It is identical between Gateway and ListenerSet parents from
// the route reconciler's point of view — what the syncer needs is whether
// the ref selects a Gateway managed by this controller, whether the route
// was accepted, and which listener-section names it landed on.
type parentRefBinding struct {
	// ManagedByThisController is true when the ref ultimately resolves to a
	// Gateway whose GatewayClass names this controller's controllerName.
	// Routes referencing a non-managed Gateway are silently ignored.
	ManagedByThisController bool

	// Result is the binding result (Accepted, Reason, matched section names).
	// Zero value when ManagedByThisController is false.
	Result routebinding.BindingResult
}

// resolveRouteParentBinding looks up the resource named by ref (Gateway or
// ListenerSet — anything else is silently skipped) and runs binding
// validation against it. The caller passes a route descriptor that already
// captures the route's hostnames, kind, and sectionName/port filters.
//
// Skipping unsupported ref kinds returns ManagedByThisController=false so
// the caller's iteration can simply `continue`.
func resolveRouteParentBinding(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	controllerName string,
	ref gatewayv1.ParentReference,
	routeNamespace string,
	routeInfo *routebinding.RouteInfo,
	views *listenerViewCache,
) (parentRefBinding, error) {
	kind := kindGateway
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}

	switch kind {
	case kindGateway:
		return resolveGatewayParentBinding(ctx, cli, validator, controllerName, ref, routeNamespace, routeInfo)
	case kindListenerSet:
		return resolveListenerSetParentBinding(ctx, cli, validator, controllerName, ref, routeNamespace, routeInfo, views)
	}

	return parentRefBinding{}, nil
}

func resolveGatewayParentBinding(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	controllerName string,
	ref gatewayv1.ParentReference,
	routeNamespace string,
	routeInfo *routebinding.RouteInfo,
) (parentRefBinding, error) {
	namespace := routeNamespace
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}

	var gateway gatewayv1.Gateway
	if err := cli.Get(ctx, client.ObjectKey{Name: string(ref.Name), Namespace: namespace}, &gateway); err != nil {
		// Missing referent is normal during route creation — surface as "not
		// matched" and let the caller carry on.
		return parentRefBinding{}, nil //nolint:nilerr // missing ref is not an error
	}

	managed, err := gatewayIsManaged(ctx, cli, controllerName, &gateway)
	if err != nil || !managed {
		return parentRefBinding{}, err
	}

	bindResult, bindErr := validator.ValidateBinding(ctx, &gateway, routeInfo)
	if bindErr != nil {
		return parentRefBinding{}, errors.Wrap(bindErr, "failed to validate route binding against gateway")
	}

	return parentRefBinding{ManagedByThisController: true, Result: bindResult}, nil
}

func resolveListenerSetParentBinding(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	controllerName string,
	ref gatewayv1.ParentReference,
	routeNamespace string,
	routeInfo *routebinding.RouteInfo,
	views *listenerViewCache,
) (parentRefBinding, error) {
	namespace := routeNamespace
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}

	var listenerSet gatewayv1.ListenerSet
	if err := cli.Get(ctx, client.ObjectKey{Name: string(ref.Name), Namespace: namespace}, &listenerSet); err != nil {
		return parentRefBinding{}, nil //nolint:nilerr // missing ref is not an error
	}

	parent, found := listenerSetParentGateway(ctx, cli, &listenerSet)
	if !found {
		return parentRefBinding{}, nil
	}

	managed, err := gatewayIsManaged(ctx, cli, controllerName, parent)
	if err != nil || !managed {
		return parentRefBinding{}, err
	}

	// The parent Gateway must also allow this ListenerSet to attach; routes
	// targeting a not-allowed ListenerSet are rejected.
	allowed, err := validator.EvaluateListenerSetAcceptance(ctx, parent, &listenerSet)
	if err != nil {
		return parentRefBinding{}, errors.Wrap(err, "failed to evaluate listenerset acceptance")
	}

	if !allowed.Accepted {
		return parentRefBinding{
			ManagedByThisController: true,
			Result: routebinding.BindingResult{
				Accepted: false,
				Reason:   gatewayv1.RouteReasonNoMatchingParent,
				Message:  "Parent ListenerSet is not allowed by the Gateway",
			},
		}, nil
	}

	bindResult, bindErr := validator.ValidateBindingForListenerSet(ctx, &listenerSet, routeInfo)
	if bindErr != nil {
		return parentRefBinding{}, errors.Wrap(bindErr, "failed to validate route binding against listenerset")
	}

	// The merge view marks listener entries that conflict with a higher-
	// precedence listener (hostname or protocol). Routes attached to such an
	// entry MUST NOT be accepted, otherwise we'd program a rule the spec
	// says should not exist. Filter the matched sections through the merge
	// view; if every match is conflicted, surface NoMatchingParent.
	if bindResult.Accepted {
		bindResult = filterMatchedListenersByConflict(ctx, cli, &listenerSet, parent, bindResult, views)
	}

	return parentRefBinding{ManagedByThisController: true, Result: bindResult}, nil
}

// filterMatchedListenersByConflict drops any matched section from the
// binding result whose merged-view counterpart is conflicted. When every
// match is filtered out the binding flips to NoMatchingParent.
func filterMatchedListenersByConflict(
	ctx context.Context,
	cli client.Client,
	listenerSet *gatewayv1.ListenerSet,
	parent *gatewayv1.Gateway,
	result routebinding.BindingResult,
	views *listenerViewCache,
) routebinding.BindingResult {
	view, err := views.orNew(cli).forGateway(ctx, parent)
	if err != nil {
		// Best-effort: if we can't compute the merge view, leave the binding
		// as-is. A later reconcile will retry.
		return result
	}

	kept := make([]gatewayv1.SectionName, 0, len(result.MatchedListeners))

	for _, section := range result.MatchedListeners {
		if view.conflictReason(listenerSet, section) != "" {
			continue
		}

		kept = append(kept, section)
	}

	if len(kept) == 0 {
		return routebinding.BindingResult{
			Accepted: false,
			Reason:   gatewayv1.RouteReasonNoMatchingParent,
			Message:  "Matched listener entries are conflicted with higher-precedence listeners",
		}
	}

	result.MatchedListeners = kept

	return result
}

// listenerSetParentGateway loads the Gateway referenced by a ListenerSet's
// spec.parentRef. Returns found=false (without error) when the Gateway has
// not been created yet or has been deleted.
func listenerSetParentGateway(
	ctx context.Context,
	cli client.Client,
	listenerSet *gatewayv1.ListenerSet,
) (*gatewayv1.Gateway, bool) {
	parentNamespace := listenerSet.Namespace
	if listenerSet.Spec.ParentRef.Namespace != nil && *listenerSet.Spec.ParentRef.Namespace != "" {
		parentNamespace = string(*listenerSet.Spec.ParentRef.Namespace)
	}

	var gateway gatewayv1.Gateway

	key := client.ObjectKey{Name: string(listenerSet.Spec.ParentRef.Name), Namespace: parentNamespace}
	if err := cli.Get(ctx, key, &gateway); err != nil {
		return nil, false
	}

	return &gateway, true
}

func gatewayIsManaged(
	ctx context.Context,
	cli client.Client,
	controllerName string,
	gateway *gatewayv1.Gateway,
) (bool, error) {
	classNames, err := managedClassNames(ctx, cli, controllerName)
	if err != nil {
		return false, errors.Wrap(err, "failed to list managed gateway classes")
	}

	return classNames[string(gateway.Spec.GatewayClassName)], nil
}
