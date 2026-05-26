package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// withEffectiveHostnames returns copies of the given routes whose
// Spec.Hostnames is augmented with the hostnames of each parentRef listener
// the route binds to.
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

	out := make([]*gatewayv1.HTTPRoute, len(routes))

	for i, route := range routes {
		if len(route.Spec.Hostnames) > 0 {
			out[i] = route

			continue
		}

		parentHostnames := collectParentListenerHostnames(ctx, cli, route.Namespace, route.Spec.ParentRefs)
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

// collectParentListenerHostnames returns every hostname declared on a
// parent listener (Gateway listener or ListenerSet entry) selected by the
// route's parentRefs. sectionName narrows the selection to a single listener
// when set; otherwise all listeners on the parent contribute.
func collectParentListenerHostnames(
	ctx context.Context,
	cli client.Client,
	routeNamespace string,
	parentRefs []gatewayv1.ParentReference,
) []gatewayv1.Hostname {
	seen := make(map[gatewayv1.Hostname]struct{})

	var out []gatewayv1.Hostname

	add := func(hostname gatewayv1.Hostname) {
		if _, ok := seen[hostname]; ok {
			return
		}

		seen[hostname] = struct{}{}
		out = append(out, hostname)
	}

	for _, ref := range parentRefs {
		hosts := lookupParentHostnames(ctx, cli, routeNamespace, ref)
		for _, host := range hosts {
			add(host)
		}
	}

	return out
}

func lookupParentHostnames(
	ctx context.Context,
	cli client.Client,
	routeNamespace string,
	ref gatewayv1.ParentReference,
) []gatewayv1.Hostname {
	kind := kindGateway
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}

	namespace := routeNamespace
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}

	switch kind {
	case kindGateway:
		return gatewayListenerHostnames(ctx, cli, namespace, string(ref.Name), ref.SectionName)
	case kindListenerSet:
		return listenerSetEntryHostnames(ctx, cli, namespace, string(ref.Name), ref.SectionName)
	}

	return nil
}

//nolint:dupl // mirrored on purpose against listenerSetEntryHostnames — different Get target prevents a generic
func gatewayListenerHostnames(
	ctx context.Context,
	cli client.Client,
	namespace, name string,
	sectionName *gatewayv1.SectionName,
) []gatewayv1.Hostname {
	var gateway gatewayv1.Gateway
	if err := cli.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &gateway); err != nil {
		return nil
	}

	return collectListenerHostnames(
		len(gateway.Spec.Listeners),
		func(i int) (gatewayv1.SectionName, *gatewayv1.Hostname) {
			return gateway.Spec.Listeners[i].Name, gateway.Spec.Listeners[i].Hostname
		},
		sectionName,
	)
}

//nolint:dupl // mirrored on purpose against gatewayListenerHostnames — different Get target prevents a generic
func listenerSetEntryHostnames(
	ctx context.Context,
	cli client.Client,
	namespace, name string,
	sectionName *gatewayv1.SectionName,
) []gatewayv1.Hostname {
	var listenerSet gatewayv1.ListenerSet
	if err := cli.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &listenerSet); err != nil {
		return nil
	}

	return collectListenerHostnames(
		len(listenerSet.Spec.Listeners),
		func(i int) (gatewayv1.SectionName, *gatewayv1.Hostname) {
			return listenerSet.Spec.Listeners[i].Name, listenerSet.Spec.Listeners[i].Hostname
		},
		sectionName,
	)
}

// collectListenerHostnames is the shared loop that iterates a slice of
// either Gateway listeners or ListenerSet entries (selected via the
// nameAndHostname accessor) and collects the non-empty hostnames whose
// section name matches the sectionName filter.
func collectListenerHostnames(
	count int,
	nameAndHostname func(int) (gatewayv1.SectionName, *gatewayv1.Hostname),
	sectionName *gatewayv1.SectionName,
) []gatewayv1.Hostname {
	var out []gatewayv1.Hostname

	for i := range count {
		name, hostname := nameAndHostname(i)
		if sectionName != nil && *sectionName != name {
			continue
		}

		if hostname != nil && *hostname != "" {
			out = append(out, *hostname)
		}
	}

	return out
}
