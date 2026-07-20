package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// catchAllHostnameSentinel marks, in the per-listener hostname stream, a
// hostname-less route accepted by a hostname-less listener: the route serves
// every hostname through that listener, so no sibling listener's hostname may
// narrow it. The empty string cannot collide with a real hostname -- listeners
// with empty hostnames never contribute a literal value.
const catchAllHostnameSentinel = gatewayv1.Hostname("")

// withEffectiveHostnames returns copies of the given routes whose
// Spec.Hostnames is narrowed to the hostname scope of the listeners the route
// actually binds to.
//
// Why: per Gateway API a route serves, through each bound listener, only the
// INTERSECTION of its own hostnames with that listener's hostname (issue #587).
// Two cases fold into one rule:
//   - A route that declares hostnames must not answer a declared hostname that
//     no bound listener's hostname covers — e.g. a route declaring
//     `non.matching.com` bound to a `very.specific.com` listener answers only
//     `very.specific.com`, and `non.matching.com` returns 404.
//   - A route that declares NO hostnames inherits each bound listener's
//     hostname, rather than becoming a default-route catch-all answering every
//     Host (which would be wrong for a listener-scoped route).
//
// A route bound to several listeners serves the UNION of the per-listener
// intersections. The proxy router consults the resulting Spec.Hostnames to
// decide which Host headers a rule answers.
//
// Critically, only listeners that actually ACCEPT the route contribute — the
// same per-listener namespace / kind / hostname / sectionName checks the
// binding validator applies. A route bound to a multi-listener ListenerSet
// where only some listeners permit the route's namespace must not answer on the
// hostnames of the listeners that reject it.
//
// When the intersection is empty (an unresolvable parent, or a hostname-less
// route bound only to hostname-less listeners), the route is left untouched:
// the narrowing never broadens the served set beyond what the route already
// declared, and never turns a hostname-less catch-all into anything else.
//
// The function never mutates the input routes; each output element is a
// fresh shallow copy whose Spec.Hostnames slice has been replaced.
//
//nolint:dupl // mirrored on purpose against withEffectiveHostnamesGRPC; the concrete HTTPRoute/GRPCRoute clone types prevent a clean generic.
func withEffectiveHostnames(
	ctx context.Context,
	cli client.Client,
	routes []*gatewayv1.HTTPRoute,
	views *listenerViewCache,
) []*gatewayv1.HTTPRoute {
	if len(routes) == 0 {
		return routes
	}

	views = views.orNew(cli)
	validator := routebinding.NewValidator(cli)
	out := make([]*gatewayv1.HTTPRoute, len(routes))

	for i, route := range routes {
		effective, catchAll := collectEffectiveListenerHostnames(ctx, cli, validator, HTTPRouteWrapper{route}, views)
		if catchAll && len(route.Spec.Hostnames) == 0 {
			// Accepted by a hostname-less listener: the route stays a
			// catch-all regardless of what pinned sibling listeners
			// contributed.
			out[i] = route

			continue
		}

		if len(effective) == 0 {
			out[i] = route

			continue
		}

		clone := *route
		clone.Spec.Hostnames = effective
		out[i] = &clone
	}

	return out
}

// withEffectiveHostnamesGRPC is the GRPCRoute counterpart of
// withEffectiveHostnames. The Gateway API applies the same listener-hostname
// intersection to GRPCRoute as to HTTPRoute: a gRPC route serves only the
// intersection of its hostnames with each bound listener's hostname, and a gRPC
// route with empty spec.hostnames inherits the listener's hostname rather than
// becoming a catch-all. The shared core (collectEffectiveListenerHostnames)
// operates on the Route interface, so only the slice type and the clone step
// differ from the HTTP path.
//
//nolint:dupl // mirrored on purpose against withEffectiveHostnames; the concrete HTTPRoute/GRPCRoute clone types prevent a clean generic.
func withEffectiveHostnamesGRPC(
	ctx context.Context,
	cli client.Client,
	routes []*gatewayv1.GRPCRoute,
	views *listenerViewCache,
) []*gatewayv1.GRPCRoute {
	if len(routes) == 0 {
		return routes
	}

	views = views.orNew(cli)
	validator := routebinding.NewValidator(cli)
	out := make([]*gatewayv1.GRPCRoute, len(routes))

	for i, route := range routes {
		effective, catchAll := collectEffectiveListenerHostnames(ctx, cli, validator, GRPCRouteWrapper{route}, views)
		if catchAll && len(route.Spec.Hostnames) == 0 {
			// Accepted by a hostname-less listener: the route stays a
			// catch-all regardless of what pinned sibling listeners
			// contributed.
			out[i] = route

			continue
		}

		if len(effective) == 0 {
			out[i] = route

			continue
		}

		clone := *route
		clone.Spec.Hostnames = effective
		out[i] = &clone
	}

	return out
}

// collectEffectiveListenerHostnames walks the route's parentRefs and, for each
// one that resolves to a managed Gateway (directly or via a ListenerSet),
// collects the intersection of the route's hostnames with the hostnames of the
// listeners the route is ACCEPTED on per the binding validator. The results are
// unioned and de-duplicated across every parentRef. Rejected listeners (wrong
// namespace, kind, hostname, or — for ListenerSet — conflicted) contribute
// nothing.
func collectEffectiveListenerHostnames(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	route Route,
	views *listenerViewCache,
) ([]gatewayv1.Hostname, bool) {
	seen := make(map[gatewayv1.Hostname]struct{})

	var out []gatewayv1.Hostname

	catchAll := false

	add := func(hostname gatewayv1.Hostname) {
		if hostname == catchAllHostnameSentinel {
			catchAll = true

			return
		}

		if _, ok := seen[hostname]; ok {
			return
		}

		seen[hostname] = struct{}{}
		out = append(out, hostname)
	}

	for _, ref := range route.GetParentRefs() {
		for _, hostname := range effectiveHostnamesForParentRef(ctx, cli, validator, route, ref, views) {
			add(hostname)
		}
	}

	return out, catchAll
}

func effectiveHostnamesForParentRef(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	route Route,
	ref gatewayv1.ParentReference,
	views *listenerViewCache,
) []gatewayv1.Hostname {
	return resolveParentRefListeners(ctx, cli, validator, route, ref, views,
		gatewayEffectiveHostnames, listenerSetEffectiveHostnames)
}

// gatewayListenerBranch and listenerSetListenerBranch are the two per-parentRef
// resolvers resolveParentRefListeners delegates to once a parentRef resolves to
// a managed Gateway or ListenerSet respectively. Each extracts the per-listener
// value the caller wants (hostname intersections, listener protocols, …) from
// an accepted binding.
type (
	gatewayListenerBranch[T any] func(
		ctx context.Context,
		cli client.Client,
		validator *routebinding.Validator,
		namespace, name string,
		routeInfo *routebinding.RouteInfo,
	) []T

	listenerSetListenerBranch[T any] func(
		ctx context.Context,
		cli client.Client,
		validator *routebinding.Validator,
		namespace, name string,
		routeInfo *routebinding.RouteInfo,
		views *listenerViewCache,
	) []T
)

// resolveParentRefListeners is the shared parentRef → managed Gateway /
// ListenerSet resolution used by both the hostname-intersection and
// redirect-scheme passes. It applies the same group / kind / namespace
// resolution and builds the RouteInfo (carrying the route's real hostnames so
// the binding validator filters listeners accurately), then delegates to the
// Gateway or ListenerSet branch. Only the per-listener value each pass extracts
// differs, so the two passes share this preamble via the T parameter instead of
// duplicating it.
func resolveParentRefListeners[T any](
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	route Route,
	ref gatewayv1.ParentReference,
	views *listenerViewCache,
	gatewayBranch gatewayListenerBranch[T],
	listenerSetBranch listenerSetListenerBranch[T],
) []T {
	if ref.Group != nil && string(*ref.Group) != "" && string(*ref.Group) != gatewayv1.GroupName {
		return nil
	}

	kind := kindGateway
	if ref.Kind != nil {
		kind = string(*ref.Kind)
	}

	namespace := route.GetNamespace()
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}

	routeInfo := &routebinding.RouteInfo{
		Name:        route.GetName(),
		Namespace:   route.GetNamespace(),
		Hostnames:   route.GetHostnames(),
		Kind:        route.GetRouteKind(),
		SectionName: ref.SectionName,
		Port:        ref.Port,
	}

	switch kind {
	case kindGateway:
		return gatewayBranch(ctx, cli, validator, namespace, string(ref.Name), routeInfo)
	case kindListenerSet:
		return listenerSetBranch(ctx, cli, validator, namespace, string(ref.Name), routeInfo, views)
	}

	return nil
}

func gatewayEffectiveHostnames(
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

	return effectiveHostnamesForSections(result.MatchedListeners, hostByName, routeInfo.Hostnames)
}

func listenerSetEffectiveHostnames(
	ctx context.Context,
	cli client.Client,
	validator *routebinding.Validator,
	namespace, name string,
	routeInfo *routebinding.RouteInfo,
	views *listenerViewCache,
) []gatewayv1.Hostname {
	var listenerSet gatewayv1.ListenerSet
	if err := cli.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &listenerSet); err != nil {
		return nil
	}

	result, err := validator.ValidateBindingForListenerSet(ctx, &listenerSet, routeInfo)
	if err != nil || !result.Accepted {
		return nil
	}

	matched := nonConflictedSections(ctx, cli, &listenerSet, result.MatchedListeners, views)

	hostByName := make(map[gatewayv1.SectionName]*gatewayv1.Hostname, len(listenerSet.Spec.Listeners))
	for i := range listenerSet.Spec.Listeners {
		hostByName[listenerSet.Spec.Listeners[i].Name] = listenerSet.Spec.Listeners[i].Hostname
	}

	return effectiveHostnamesForSections(matched, hostByName, routeInfo.Hostnames)
}

// nonConflictedSections drops, from sections, any matched listener whose
// merged-view entry (across the parent Gateway and its sibling ListenerSets) is
// conflicted and therefore not programmed. A route binds to neither the
// hostname nor the protocol of a conflicted listener, so both the
// hostname-inheritance and redirect-scheme passes route their accepted
// sections through this. When the parent Gateway cannot be resolved the input
// sections are returned unchanged (best-effort, matching the prior behaviour).
func nonConflictedSections(
	ctx context.Context,
	cli client.Client,
	listenerSet *gatewayv1.ListenerSet,
	sections []gatewayv1.SectionName,
	views *listenerViewCache,
) []gatewayv1.SectionName {
	parent, found := listenerSetParentGateway(ctx, cli, listenerSet)
	if !found {
		return sections
	}

	view, err := views.orNew(cli).forGateway(ctx, parent)
	if err != nil {
		return sections
	}

	return dropConflictedSections(view.merged, listenerSet, sections)
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

// effectiveHostnamesForSections maps each matched listener section to the
// intersection of the route's hostnames with that listener's hostname (Gateway
// API semantics via routebinding.EffectiveListenerHostnames). A hostname-less
// route inherits each listener's hostname; a listener with no hostname
// contributes the route's hostnames unchanged (it is a catch-all). Results are
// concatenated in section order; the caller de-duplicates across sections.
func effectiveHostnamesForSections(
	sections []gatewayv1.SectionName,
	hostByName map[gatewayv1.SectionName]*gatewayv1.Hostname,
	routeHostnames []gatewayv1.Hostname,
) []gatewayv1.Hostname {
	var out []gatewayv1.Hostname

	for _, section := range sections {
		listenerHostname, ok := hostByName[section]
		if !ok {
			continue
		}

		// A hostname-less route accepted by a hostname-less listener serves
		// EVERY hostname through it. Emit the catch-all sentinel so the
		// collector knows the union covers all hosts -- otherwise a pinned
		// sibling listener's hostname would silently narrow a catch-all route.
		if len(routeHostnames) == 0 && (listenerHostname == nil || *listenerHostname == "") {
			out = append(out, catchAllHostnameSentinel)

			continue
		}

		out = append(out, routebinding.EffectiveListenerHostnames(listenerHostname, routeHostnames)...)
	}

	return out
}
