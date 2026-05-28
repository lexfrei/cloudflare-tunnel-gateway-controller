package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// findRoutesAttachedToListenerSet returns reconcile requests for every route
// in the slice whose parentRef targets the given ListenerSet AND whose
// parent Gateway is managed by this controller.
//
// Used by HTTPRoute / GRPCRoute reconcilers to enqueue routes when a
// ListenerSet they depend on is created, edited, or deleted — without this
// hook a route created BEFORE its ListenerSet would never get a reconcile
// trigger once the ListenerSet appeared.
func findRoutesAttachedToListenerSet(
	ctx context.Context,
	cli client.Client,
	listenerSet *gatewayv1.ListenerSet,
	controllerName string,
	routes []Route,
) []reconcile.Request {
	if listenerSet == nil {
		return nil
	}

	parent, found := listenerSetParentGateway(ctx, cli, listenerSet)
	if !found {
		return nil
	}

	if !isGatewayManagedByController(ctx, cli, parent, controllerName) {
		return nil
	}

	requests := make([]reconcile.Request, 0)

	for _, route := range routes {
		if !routeTargetsListenerSet(route, listenerSet) {
			continue
		}

		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      route.GetName(),
				Namespace: route.GetNamespace(),
			},
		})
	}

	return requests
}

// routeTargetsListenerSet returns true when at least one of the route's
// parentRefs names the given ListenerSet (group, kind, name, namespace).
func routeTargetsListenerSet(route Route, listenerSet *gatewayv1.ListenerSet) bool {
	for _, ref := range route.GetParentRefs() {
		if ref.Group != nil && string(*ref.Group) != "" && string(*ref.Group) != gatewayv1.GroupName {
			continue
		}

		if ref.Kind == nil || string(*ref.Kind) != kindListenerSet {
			continue
		}

		if string(ref.Name) != listenerSet.Name {
			continue
		}

		refNamespace := route.GetNamespace()
		if ref.Namespace != nil {
			refNamespace = string(*ref.Namespace)
		}

		if refNamespace == listenerSet.Namespace {
			return true
		}
	}

	return false
}
