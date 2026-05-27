package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// withEffectiveHostnames returns copies of the given routes whose
// Spec.Hostnames is augmented with the hostnames of each parentRef listener
// the route actually binds to.
//
// Why: when an HTTPRoute attaches to a Gateway listener or ListenerSet entry
// with a non-empty hostname, and the route itself declares no hostnames in
// spec.hostnames, the route is expected (per Gateway API spec) to serve
// traffic for the parent listener's hostname. The proxy router consults the
// resulting Spec.Hostnames to decide which Host headers a rule answers; an
// empty list there would make the rule a default-route catch-all, which is
// wrong for ListenerSet-bound routes that should only serve the ListenerSet
// listener's hostname.
//
// Critically, hostnames are inherited ONLY from listeners that actually
// accept the route — the same per-listener namespace / kind / hostname /
// sectionName checks the binding validator applies. A route bound to a
// multi-listener ListenerSet where only some listeners permit the route's
// namespace must NOT inherit the hostnames of the listeners that reject it,
// otherwise it would answer on hostnames it has no business serving.
//
// The function never mutates the input routes; each output element is a
// fresh shallow copy whose Spec.Hostnames slice has been replaced.
func withEffectiveHostnames(
	ctx context.Context,
	cli client.Client,
	routes []*gatewayv1.HTTPRoute,
) []*gatewayv1.HTTPRoute {
	if len(routes) == 0 {
		return routes
	}

	validator := routebinding.NewValidator(cli)
	out := make([]*gatewayv1.HTTPRoute, len(routes))

	for i, route := range routes {
		if len(route.Spec.Hostnames) > 0 {
			out[i] = route

			continue
		}

		parentHostnames := collectAcceptedListenerHostnames(ctx, cli, validator, route)
		if len(parentHostnames) == 0 {
			out[i] = route

			continue
		}

		clone := *route
		clone.Spec.Hostnames = parentHostnames
		out[i] = &clone
	}

	return out
}

// collectAcceptedListenerHostnames walks the route's parentRefs and, for each
// one that resolves to a managed Gateway (directly or via a ListenerSet),
// collects the hostnames of the listeners the route is ACCEPTED on per the
// binding validator. Rejected listeners (wrong namespace, kind, hostname, or
// — for ListenerSet — conflicted) contribute nothing.
func collectAcceptedListenerHostnames(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	route *gatewayv1.HTTPRoute,
) []gatewayv1.Hostname {
	seen := make(map[gatewayv1.Hostname]struct{})

	var out []gatewayv1.Hostname

	add := func(hostname gatewayv1.Hostname) {
		if hostname == "" {
			return
		}

		if _, ok := seen[hostname]; ok {
			return
		}

		seen[hostname] = struct{}{}
		out = append(out, hostname)
	}

	for _, ref := range route.Spec.ParentRefs {
		for _, hostname := range acceptedHostnamesForParentRef(ctx, cli, validator, route, ref) {
			add(hostname)
		}
	}

	return out
}

func acceptedHostnamesForParentRef(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	route *gatewayv1.HTTPRoute,
	ref gatewayv1.ParentReference,
) []gatewayv1.Hostname {
	if ref.Group != nil && string(*ref.Group) != "" && string(*ref.Group) != gatewayv1.GroupName {
		return nil
	}

	kind := kindGateway
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}

	namespace := route.Namespace
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}

	routeInfo := &routebinding.RouteInfo{
		Name:        route.Name,
		Namespace:   route.Namespace,
		Hostnames:   nil, // empty by definition — that's why we're inheriting
		Kind:        routebinding.KindHTTPRoute,
		SectionName: ref.SectionName,
		Port:        ref.Port,
	}

	switch kind {
	case kindGateway:
		return gatewayAcceptedHostnames(ctx, cli, validator, namespace, string(ref.Name), routeInfo)
	case kindListenerSet:
		return listenerSetAcceptedHostnames(ctx, cli, validator, namespace, string(ref.Name), routeInfo)
	}

	return nil
}

func gatewayAcceptedHostnames(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	namespace, name string,
	routeInfo *routebinding.RouteInfo,
) []gatewayv1.Hostname {
	var gateway gatewayv1.Gateway
	if err := cli.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &gateway); err != nil {
		return nil
	}

	result, err := validator.ValidateBinding(ctx, &gateway, routeInfo)
	if err != nil || !result.Accepted {
		return nil
	}

	hostByName := make(map[gatewayv1.SectionName]*gatewayv1.Hostname, len(gateway.Spec.Listeners))
	for i := range gateway.Spec.Listeners {
		hostByName[gateway.Spec.Listeners[i].Name] = gateway.Spec.Listeners[i].Hostname
	}

	return hostnamesForSections(result.MatchedListeners, hostByName)
}

func listenerSetAcceptedHostnames(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	namespace, name string,
	routeInfo *routebinding.RouteInfo,
) []gatewayv1.Hostname {
	var listenerSet gatewayv1.ListenerSet
	if err := cli.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &listenerSet); err != nil {
		return nil
	}

	result, err := validator.ValidateBindingForListenerSet(ctx, &listenerSet, routeInfo)
	if err != nil || !result.Accepted {
		return nil
	}

	matched := result.MatchedListeners

	// Drop sections whose merged-view entry is conflicted — a conflicted
	// listener is not programmed, so a route must not inherit its hostname.
	if parent, found := listenerSetParentGateway(ctx, cli, &listenerSet); found {
		if siblings, collectErr := collectAcceptedListenerSetsForGateway(ctx, cli, parent); collectErr == nil {
			merged := listenermerge.Merge(parent, siblings)
			matched = dropConflictedSections(merged, &listenerSet, matched)
		}
	}

	hostByName := make(map[gatewayv1.SectionName]*gatewayv1.Hostname, len(listenerSet.Spec.Listeners))
	for i := range listenerSet.Spec.Listeners {
		hostByName[listenerSet.Spec.Listeners[i].Name] = listenerSet.Spec.Listeners[i].Hostname
	}

	return hostnamesForSections(matched, hostByName)
}

func dropConflictedSections(
	merged *listenermerge.MergeResult,
	listenerSet *gatewayv1.ListenerSet,
	sections []gatewayv1.SectionName,
) []gatewayv1.SectionName {
	kept := make([]gatewayv1.SectionName, 0, len(sections))

	for _, section := range sections {
		if entry := findMergedEntry(merged, listenerSet, section); entry != nil && entry.ConflictReason != "" {
			continue
		}

		kept = append(kept, section)
	}

	return kept
}

func hostnamesForSections(
	sections []gatewayv1.SectionName,
	hostByName map[gatewayv1.SectionName]*gatewayv1.Hostname,
) []gatewayv1.Hostname {
	var out []gatewayv1.Hostname

	for _, section := range sections {
		if hostname, ok := hostByName[section]; ok && hostname != nil && *hostname != "" {
			out = append(out, *hostname)
		}
	}

	return out
}
