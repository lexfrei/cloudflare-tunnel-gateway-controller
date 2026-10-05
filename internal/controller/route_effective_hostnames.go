package controller

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/cockroachdb/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/listenermerge"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/parentref"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// catchAllHostnameSentinel marks, in the per-listener hostname stream, a
// hostname-less route accepted by a hostname-less listener: the route serves
// every hostname through that listener, so no sibling listener's hostname may
// narrow it. The empty string cannot collide with a real hostname -- listeners
// with empty hostnames never contribute a literal value.
const catchAllHostnameSentinel = gatewayv1.Hostname("")

// servedHostname is one hostname a route serves through one listener.
type servedHostname struct {
	hostname gatewayv1.Hostname
	// gateway is the "namespace/name" of the listener's Gateway, the parent
	// Gateway for a ListenerSet entry.
	gateway  string
	listener proxy.Listener
}

// routeListeners maps a route to the listeners it is attached through, in
// the shape of proxy.RouteRule.Listeners.
type routeListeners map[types.NamespacedName]map[string][]proxy.Listener

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
// When the intersection is empty (a parent that does not exist, or a
// hostname-less route bound only to hostname-less listeners), the route is left
// untouched: the narrowing never broadens the served set beyond what the route
// already declared, and never turns a hostname-less catch-all into anything
// else. A parent that exists but cannot be evaluated (a failed read) is
// different: nothing shows what the route may serve, so when no other parent
// contributes the route is left out of the result, and when another parent
// does, the route is narrowed to what that parent lends.
// A diagnostic reports the undecided parent on the route's status in both
// cases, unless the narrowed route still serves every hostname it declares.
//
// parents names, per route, the Gateways the data plane being built serves
// the route for; a Gateway served by another plane contributes nothing. With
// parents set, a route none of those Gateways contributes a listener to, such
// as one whose served Gateway no longer exists, is left out instead of left
// untouched, since as written it may be a catch-all. A nil parents keeps every
// Gateway.
//
// controllerName scopes which parents may contribute at all: only Gateways
// whose GatewayClass names this controller. A route may legitimately be
// attached to another implementation's Gateway as well, and that Gateway's
// listeners describe what IT serves, not what we do. Empty accepts any
// Gateway, which is the convention the Gateway client-cert resolver already
// uses for tests.
//
// The function never mutates the input routes; each output element is a
// fresh shallow copy whose Spec.Hostnames slice has been replaced.
//
//nolint:dupl // mirrored on purpose against withEffectiveHostnamesGRPC; the concrete HTTPRoute/GRPCRoute clone types prevent a clean generic.
func withEffectiveHostnames(
	ctx context.Context,
	cli client.Client,
	controllerName string,
	routes []*gatewayv1.HTTPRoute,
	views *listenerViewCache,
	parents map[string]map[string]bool,
) ([]*gatewayv1.HTTPRoute, []proxy.RouteDiagnostic, routeListeners) {
	if len(routes) == 0 {
		return routes, nil, nil
	}

	views = views.orNew(cli)
	validator := routebinding.NewValidator(cli)
	out := make([]*gatewayv1.HTTPRoute, 0, len(routes))
	attached := make(routeListeners, len(routes))

	var undecidedDiags []proxy.RouteDiagnostic

	for _, route := range routes {
		effective, catchAll, listeners, undecided := collectEffectiveListenerHostnames(ctx, cli, controllerName, validator, HTTPRouteWrapper{route}, views,
			servedBy(parents, route))
		if len(listeners) > 0 {
			attached[client.ObjectKeyFromObject(route)] = listeners
		}

		if catchAll && len(route.Spec.Hostnames) == 0 {
			// Accepted by a hostname-less listener: the route stays a
			// catch-all regardless of what pinned sibling listeners
			// contributed.
			out = append(out, route)

			continue
		}

		if len(effective) == 0 {
			if undecided != nil {
				undecidedDiags = append(undecidedDiags, reportUndecidedParent(ctx, kindHTTPRouteDiag, route, undecided, true))

				continue
			}

			if parents != nil && len(listeners) == 0 {
				continue
			}

			out = append(out, route)

			continue
		}

		if undecided != nil {
			if diag, narrowed := reportNarrowedRoute(ctx, kindHTTPRouteDiag, route, route.Spec.Hostnames, effective, undecided); narrowed {
				undecidedDiags = append(undecidedDiags, diag)
			}
		}

		clone := *route
		clone.Spec.Hostnames = effective
		out = append(out, &clone)
	}

	return out, undecidedDiags, attached
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
	controllerName string,
	routes []*gatewayv1.GRPCRoute,
	views *listenerViewCache,
	parents map[string]map[string]bool,
) ([]*gatewayv1.GRPCRoute, []proxy.RouteDiagnostic, routeListeners) {
	if len(routes) == 0 {
		return routes, nil, nil
	}

	views = views.orNew(cli)
	validator := routebinding.NewValidator(cli)
	out := make([]*gatewayv1.GRPCRoute, 0, len(routes))
	attached := make(routeListeners, len(routes))

	var undecidedDiags []proxy.RouteDiagnostic

	for _, route := range routes {
		effective, catchAll, listeners, undecided := collectEffectiveListenerHostnames(ctx, cli, controllerName, validator, GRPCRouteWrapper{route}, views,
			servedBy(parents, route))
		if len(listeners) > 0 {
			attached[client.ObjectKeyFromObject(route)] = listeners
		}

		if catchAll && len(route.Spec.Hostnames) == 0 {
			// Accepted by a hostname-less listener: the route stays a
			// catch-all regardless of what pinned sibling listeners
			// contributed.
			out = append(out, route)

			continue
		}

		if len(effective) == 0 {
			if undecided != nil {
				undecidedDiags = append(undecidedDiags, reportUndecidedParent(ctx, kindGRPCRouteDiag, route, undecided, true))

				continue
			}

			if parents != nil && len(listeners) == 0 {
				continue
			}

			out = append(out, route)

			continue
		}

		if undecided != nil {
			if diag, narrowed := reportNarrowedRoute(ctx, kindGRPCRouteDiag, route, route.Spec.Hostnames, effective, undecided); narrowed {
				undecidedDiags = append(undecidedDiags, diag)
			}
		}

		clone := *route
		clone.Spec.Hostnames = effective
		out = append(out, &clone)
	}

	return out, undecidedDiags, attached
}

// collectEffectiveListenerHostnames walks the route's parentRefs and, for each
// one that resolves to a managed Gateway (directly or via a ListenerSet),
// collects the intersection of the route's hostnames with the hostnames of the
// listeners the route is ACCEPTED on per the binding validator. The results are
// unioned and de-duplicated across every parentRef. Rejected listeners (wrong
// namespace, kind, hostname, or — for ListenerSet — conflicted) contribute
// nothing. The error is the first parentRef that could not be evaluated, which
// also contributes nothing. The map lists, per Gateway, the hostnames of the
// listeners that contributed, in the shape of proxy.RouteRule.Listeners.
// served, when not nil, drops every listener of a Gateway it does not name.
func collectEffectiveListenerHostnames(
	ctx context.Context,
	cli client.Client,
	controllerName string,
	validator *routebinding.Validator,
	route Route,
	views *listenerViewCache,
	served func(gateway string) bool,
) ([]gatewayv1.Hostname, bool, map[string][]proxy.Listener, error) {
	seen := make(map[gatewayv1.Hostname]struct{})
	listeners := make(map[string][]proxy.Listener)

	var out []gatewayv1.Hostname

	catchAll := false

	add := func(served servedHostname) {
		if !slices.Contains(listeners[served.gateway], served.listener) {
			listeners[served.gateway] = append(listeners[served.gateway], served.listener)
		}

		hostname := served.hostname
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

	var undecided error

	for _, ref := range route.GetParentRefs() {
		hostnames, err := effectiveHostnamesForParentRef(ctx, cli, controllerName, validator, route, ref, views)
		if err != nil && undecided == nil && !servedElsewhere(served, route, ref) {
			undecided = errors.Wrapf(err, "parentRef %s", ref.Name)
		}

		for _, hostname := range hostnames {
			if served == nil || served(hostname.gateway) {
				add(hostname)
			}
		}
	}

	return out, catchAll, listeners, undecided
}

// servedBy returns the filter collectEffectiveListenerHostnames applies for
// route, nil when parents is nil.
func servedBy(parents map[string]map[string]bool, route client.Object) func(gateway string) bool {
	if parents == nil {
		return nil
	}

	gateways := parents[client.ObjectKeyFromObject(route).String()]

	return func(gateway string) bool { return gateways[gateway] }
}

// servedElsewhere reports a parentRef naming a Gateway that served excludes.
// A ListenerSet's Gateway is known only once the ListenerSet is read, so such
// a ref is never excluded.
func servedElsewhere(served func(gateway string) bool, route Route, ref gatewayv1.ParentReference) bool {
	if served == nil || !parentref.InGatewayAPIGroup(ref) || (ref.Kind != nil && *ref.Kind != kindGateway) {
		return false
	}

	namespace := route.GetNamespace()
	if ref.Namespace != nil {
		namespace = string(*ref.Namespace)
	}

	return !served(namespace + "/" + string(ref.Name))
}

// reportNarrowedRoute reports a route that another parent lends hostnames to
// while one parent could not be evaluated, when the undecided parent may be
// withholding hostnames: always for a hostname-less route, which inherits what
// its parents lend, and otherwise when a declared hostname is not served. A
// route already serving every hostname it declares gains nothing from that
// parent, so it is only logged, with no diagnostic and no requeue.
func reportNarrowedRoute(
	ctx context.Context,
	kind string,
	route client.Object,
	declared, effective []gatewayv1.Hostname,
	err error,
) (proxy.RouteDiagnostic, bool) {
	if len(declared) > 0 && !slices.ContainsFunc(declared, func(hostname gatewayv1.Hostname) bool {
		return !slices.Contains(effective, hostname)
	}) {
		logUndecidedParent(ctx, slog.LevelInfo, "a parent could not be evaluated; the route already serves every hostname it declares",
			kind, route, err)

		return proxy.RouteDiagnostic{}, false
	}

	return reportUndecidedParent(ctx, kind, route, err, false), true
}

// reportUndecidedParent logs a route with a parent that could not be
// evaluated and returns the diagnostic that reports it on the route's status.
// leftOut is true when no other parent lent the route a hostname, so it was
// left out of the proxy config, and false when it still serves the hostnames
// its other parents lend. The message leaves the error to the log: a route in
// several partitions is evaluated once per partition, and a message that
// depends only on leftOut keeps the Warning Event and the condition to one
// entry per case.
func reportUndecidedParent(ctx context.Context, kind string, route client.Object, err error, leftOut bool) proxy.RouteDiagnostic {
	logMessage := "route narrowed to the hostnames its other parents lend: a parent could not be evaluated"
	message := "the controller could not evaluate a parent of this route, so it serves only the hostnames its " +
		"other parents lend and requests for the rest are not routed; the controller log names the error and the " +
		"sync is retried. This route remains Accepted."

	if leftOut {
		logMessage = "route left out of the proxy config: its parent could not be evaluated"
		message = "this route was left out of its data plane's config because the controller could not evaluate " +
			"a parent and no other parent lends it a hostname, so it serves no requests; the controller log names " +
			"the error and the sync is retried. This route remains Accepted."
	}

	logUndecidedParent(ctx, slog.LevelError, logMessage, kind, route, err)

	return proxy.RouteDiagnostic{
		Kind:      kind,
		Namespace: route.GetNamespace(),
		Name:      route.GetName(),
		Target:    proxy.DiagnosticProxyConfigPush,
		Reason:    routeReasonParentNotEvaluated,
		Message:   message,
	}
}

// logUndecidedParent logs at level the first time a route reports a parent
// failure, and at debug while the same failure repeats across syncs.
func logUndecidedParent(ctx context.Context, level slog.Level, message, kind string, route client.Object, err error) {
	name := route.GetNamespace() + "/" + route.GetName()
	level = logging.RepeatsFromContext(ctx).Level(kind+" "+name+" proxy config", message+": "+err.Error(), level)

	logging.FromContext(ctx).Log(ctx, level, message, "route", name, "error", err)
}

// effectiveHostnamesForParentRef resolves one parentRef to a managed Gateway
// or ListenerSet, builds the RouteInfo (carrying the route's real hostnames so
// the binding validator filters listeners accurately) and returns what the
// listeners accepting the route lend it. A non-nil error means the parent
// exists but could not be evaluated, as opposed to one that is absent,
// foreign or rejects the route.
func effectiveHostnamesForParentRef(
	ctx context.Context,
	cli client.Client,
	controllerName string,
	validator *routebinding.Validator,
	route Route,
	ref gatewayv1.ParentReference,
	views *listenerViewCache,
) ([]servedHostname, error) {
	if !parentref.InGatewayAPIGroup(ref) {
		return nil, nil
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
		return gatewayEffectiveHostnames(ctx, cli, controllerName, validator, namespace, string(ref.Name), routeInfo, views)
	case kindListenerSet:
		return listenerSetEffectiveHostnames(ctx, cli, controllerName, validator, namespace, string(ref.Name), routeInfo, views)
	}

	return nil, nil
}

func gatewayEffectiveHostnames(
	ctx context.Context,
	cli client.Client,
	controllerName string,
	validator *routebinding.Validator,
	namespace, name string,
	routeInfo *routebinding.RouteInfo,
	views *listenerViewCache,
) ([]servedHostname, error) {
	var gateway gatewayv1.Gateway
	if err := cli.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &gateway); err != nil {
		return nil, errors.Wrap(client.IgnoreNotFound(err), "reading Gateway")
	}

	if gatewayOwnedElsewhere(ctx, cli, &gateway, controllerName) {
		return nil, nil
	}

	result, err := bindGatewayListeners(ctx, cli, validator, &gateway, routeInfo, views)
	if err != nil {
		return nil, err
	}

	if !result.Accepted {
		return nil, incompleteBindingError(result)
	}

	byName := make(map[gatewayv1.SectionName]sectionListener, len(gateway.Spec.Listeners))
	for i := range gateway.Spec.Listeners {
		listener := &gateway.Spec.Listeners[i]
		byName[listener.Name] = sectionListener{hostname: listener.Hostname, port: listener.Port}
	}

	return effectiveHostnamesForSections(result.MatchedListeners, byName, routeInfo.Hostnames,
		client.ObjectKeyFromObject(&gateway).String()), incompleteBindingError(result)
}

// errListenerNotEvaluated reports a binding that left out a listener the
// parentRef selects because it could not be evaluated: the hostnames returned
// with it may be missing that listener's.
var errListenerNotEvaluated = errors.New("a listener the parentRef selects could not be evaluated; the binding pass logs the error")

func incompleteBindingError(result routebinding.BindingResult) error {
	if result.Incomplete {
		return errListenerNotEvaluated
	}

	return nil
}

func listenerSetEffectiveHostnames(
	ctx context.Context,
	cli client.Client,
	controllerName string,
	validator *routebinding.Validator,
	namespace, name string,
	routeInfo *routebinding.RouteInfo,
	views *listenerViewCache,
) ([]servedHostname, error) {
	var listenerSet gatewayv1.ListenerSet
	if err := cli.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &listenerSet); err != nil {
		return nil, errors.Wrap(client.IgnoreNotFound(err), "reading ListenerSet")
	}

	if excluded, err := listenerSetExcluded(ctx, cli, controllerName, validator, &listenerSet); err != nil || excluded {
		return nil, err
	}

	result, err := validator.ValidateBindingForListenerSet(ctx, &listenerSet, routeInfo)
	if err != nil || !result.Accepted {
		return nil, errors.Wrap(err, "validating binding against ListenerSet")
	}

	matched := nonConflictedSections(ctx, cli, &listenerSet, result.MatchedListeners, views)

	byName := make(map[gatewayv1.SectionName]sectionListener, len(listenerSet.Spec.Listeners))
	for i := range listenerSet.Spec.Listeners {
		entry := &listenerSet.Spec.Listeners[i]
		byName[entry.Name] = sectionListener{hostname: entry.Hostname, port: entry.Port}
	}

	return effectiveHostnamesForSections(matched, byName, routeInfo.Hostnames,
		listenerSetParentKey(&listenerSet).String()), incompleteBindingError(result)
}

// gatewayOwnedElsewhere reports whether the Gateway's GatewayClass names a
// different controller. The hostname pass drops a parent
// only on that answer. A missing or unreadable class is not evidence that the
// Gateway is someone else's, and dropping our own Gateway on it would leave a
// hostname-less route answering every Host. GatewayInfraReconciler reads the
// same three states the same way. An empty controllerName never reports
// foreign, matching gatewayManagedByController's test convention.
func gatewayOwnedElsewhere(
	ctx context.Context,
	cli client.Client,
	gateway *gatewayv1.Gateway,
	controllerName string,
) bool {
	if controllerName == "" {
		return false
	}

	state, err := classifyGatewayClass(ctx, cli, gateway, controllerName)
	if err != nil {
		logging.FromContext(ctx).Debug("GatewayClass unreadable, Gateway still treated as ours for hostnames",
			"error", err,
			"gateway", gateway.Namespace+"/"+gateway.Name,
			"gatewayClassName", string(gateway.Spec.GatewayClassName))
	}

	return state == gatewayClassForeign
}

// listenerSetExcluded reports whether a ListenerSet must contribute nothing to
// the hostname pass: it belongs to another controller,
// or its parent Gateway's spec.allowedListeners refuses it. Route acceptance
// rejects a parentRef to a refused ListenerSet (resolveListenerSetParentBinding),
// so its entries are not served here either.
//
// Whose a ListenerSet is follows from its PARENT Gateway's GatewayClass, since
// a ListenerSet names no class of its own. An absent parent is programmed by
// nobody, so it excludes the ListenerSet unless controllerName is empty. A
// parent whose read failed returns the error, since it does not answer whether
// the ListenerSet is served. An allowedListeners selector that does not parse
// refuses the ListenerSet, here and in route acceptance alike.
func listenerSetExcluded(
	ctx context.Context,
	cli client.Client,
	controllerName string,
	validator *routebinding.Validator,
	listenerSet *gatewayv1.ListenerSet,
) (bool, error) {
	parent, err := getListenerSetParentGateway(ctx, cli, listenerSet)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return controllerName != "", nil
		}

		return false, err
	}

	if gatewayOwnedElsewhere(ctx, cli, parent, controllerName) {
		return true, nil
	}

	acceptance, err := validator.EvaluateListenerSetAcceptance(ctx, parent, listenerSet)
	if err != nil {
		return false, errors.Wrap(err, "evaluating ListenerSet acceptance")
	}

	return !acceptance.Accepted, nil
}

// nonConflictedSections drops, from sections, any matched listener whose
// merged-view entry (across the parent Gateway and its sibling ListenerSets) is
// conflicted and therefore not programmed. A route binds to nothing a
// conflicted listener lends, so the hostname pass routes its accepted
// sections through this. An unresolvable parent Gateway returns the input
// sections unchanged, which is best-effort and now rarely reached: the class
// filter resolves the same parent earlier and contributes nothing when it
// cannot.
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
// gateway names the Gateway the listeners belong to.
func effectiveHostnamesForSections(
	sections []gatewayv1.SectionName,
	byName map[gatewayv1.SectionName]sectionListener,
	routeHostnames []gatewayv1.Hostname,
	gateway string,
) []servedHostname {
	var out []servedHostname

	for _, section := range sections {
		listener, ok := byName[section]
		if !ok {
			continue
		}

		listenerHostname := listener.hostname
		served := func(hostname gatewayv1.Hostname) {
			out = append(out, servedHostname{hostname: hostname, gateway: gateway, listener: proxyListener(listenerHostname, listener.port)})
		}

		// A hostname-less route accepted by a hostname-less listener serves
		// EVERY hostname through it. Emit the catch-all sentinel so the
		// collector knows the union covers all hosts -- otherwise a pinned
		// sibling listener's hostname would silently narrow a catch-all route.
		if len(routeHostnames) == 0 && (listenerHostname == nil || *listenerHostname == "") {
			served(catchAllHostnameSentinel)

			continue
		}

		for _, hostname := range routebinding.EffectiveListenerHostnames(listenerHostname, routeHostnames) {
			served(hostname)
		}
	}

	return out
}

// sectionListener is what a listener section lends the routes it accepts.
type sectionListener struct {
	hostname *gatewayv1.Hostname
	port     gatewayv1.PortNumber
}

// proxyListener renders a listener the way the proxy config carries it: a
// lowercase hostname, "" for a listener without one, and its port.
func proxyListener(hostname *gatewayv1.Hostname, port gatewayv1.PortNumber) proxy.Listener {
	listener := proxy.Listener{Port: port}
	if hostname != nil {
		listener.Hostname = strings.ToLower(string(*hostname))
	}

	return listener
}
