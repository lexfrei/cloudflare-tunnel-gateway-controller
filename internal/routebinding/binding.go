package routebinding

import (
	"context"
	"fmt"
	"strings"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
)

const (
	defaultRejectionMessage = "Route not accepted"
	routeAcceptedMessage    = "Route accepted"
)

// RouteInfo contains information about a route for binding validation.
type RouteInfo struct {
	Name        string
	Namespace   string
	Hostnames   []gatewayv1.Hostname
	Kind        gatewayv1.Kind
	SectionName *gatewayv1.SectionName
	Port        *gatewayv1.PortNumber
}

// BindingResult represents the result of route-to-listener binding validation.
type BindingResult struct {
	Accepted         bool
	Reason           gatewayv1.RouteConditionReason
	Message          string
	MatchedListeners []gatewayv1.SectionName
}

// ValidateBinding validates whether a route can bind to a gateway's listeners.
// It returns a BindingResult indicating acceptance status, reason, and matched listeners.
// The error is nil today and kept for the namespace read failures #895 covers.
func (v *Validator) ValidateBinding(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	route *RouteInfo,
) (BindingResult, error) {
	listeners := gateway.Spec.Listeners

	matched, rejectionReason, invalid, detail := findMatchingEntries(
		len(listeners),
		func(i int) (gatewayv1.SectionName, gatewayv1.PortNumber) {
			return listeners[i].Name, listeners[i].Port
		},
		func(i int) (gatewayv1.RouteConditionReason, error) {
			return v.listenerAcceptsRoute(ctx, &listeners[i], gateway.Namespace, route)
		},
		route.SectionName,
		route.Port,
	)

	logUnevaluatedListeners(ctx, route, detail)

	return makeBindingResult(matched, rejectionReason, invalid), nil
}

// logUnevaluatedListeners logs the errors of listeners that could not be
// evaluated. They quote the Gateway's spec, which the route's authors may not
// be allowed to read, so they go to the controller log and not to route status.
func logUnevaluatedListeners(ctx context.Context, route *RouteInfo, detail string) {
	if detail == "" {
		return
	}

	logging.FromContext(ctx).Warn("listeners could not be evaluated for a route",
		"route", route.Namespace+"/"+route.Name, "detail", detail)
}

// makeBindingResult turns the tuple returned by findMatchingEntries into a
// public BindingResult, applying the standard Accepted=True/Reason=Accepted
// treatment when at least one entry matched. invalid names the entries that
// could not be evaluated; the status message names them without their errors.
func makeBindingResult(
	matched []gatewayv1.SectionName,
	rejectionReason gatewayv1.RouteConditionReason,
	invalid []string,
) BindingResult {
	if len(matched) == 0 {
		var message strings.Builder

		message.WriteString(getReasonMessage(rejectionReason))

		for _, name := range invalid {
			fmt.Fprintf(&message, "; listener %q has an invalid allowedRoutes selector", name)
		}

		return BindingResult{
			Accepted:         false,
			Reason:           rejectionReason,
			Message:          message.String(),
			MatchedListeners: nil,
		}
	}

	return BindingResult{
		Accepted:         true,
		Reason:           gatewayv1.RouteReasonAccepted,
		Message:          routeAcceptedMessage,
		MatchedListeners: matched,
	}
}

// findMatchingEntries is the shared section/port match + accept iteration used
// by both Gateway listener binding and ListenerSet entry binding. The accept
// callback returns the per-entry route condition reason; entries with reason
// == Accepted are collected into the matched-section list. The last
// observed non-accepted reason becomes the fallback rejection reason when no
// entry matches.
//
// An entry whose evaluation fails (an unparseable namespace selector) admits
// nothing. Its error is returned in detail, and, when no entry matched, its name
// in invalid. It does not stop the loop: the error belongs to that entry, and a
// sibling entry may still admit the route.
func findMatchingEntries(
	count int,
	nameAndPort func(int) (gatewayv1.SectionName, gatewayv1.PortNumber),
	accept func(int) (gatewayv1.RouteConditionReason, error),
	routeSectionName *gatewayv1.SectionName,
	routePort *gatewayv1.PortNumber,
) ([]gatewayv1.SectionName, gatewayv1.RouteConditionReason, []string, string) {
	if count == 0 {
		return nil, gatewayv1.RouteReasonNoMatchingParent, nil, ""
	}

	var (
		matched             []gatewayv1.SectionName
		lastRejectionReason gatewayv1.RouteConditionReason
		invalid             []string
		entryErrors         []string
	)

	for i := range count {
		name, port := nameAndPort(i)

		if routeSectionName != nil && *routeSectionName != name {
			continue
		}

		if routePort != nil && *routePort != port {
			continue
		}

		reason, err := accept(i)
		if err != nil {
			reason = gatewayv1.RouteReasonNotAllowedByListeners

			invalid = append(invalid, string(name))
			entryErrors = append(entryErrors, fmt.Sprintf("listener %q: %v", name, err))
		}

		if reason == gatewayv1.RouteReasonAccepted {
			matched = append(matched, name)
		} else {
			lastRejectionReason = reason
		}
	}

	detail := strings.Join(entryErrors, "; ")

	if len(matched) == 0 {
		if routeSectionName != nil || routePort != nil {
			return nil, gatewayv1.RouteReasonNoMatchingParent, invalid, detail
		}

		if lastRejectionReason == "" {
			return nil, gatewayv1.RouteReasonNoMatchingParent, invalid, detail
		}

		return nil, lastRejectionReason, invalid, detail
	}

	return matched, "", nil, detail
}

// listenerAcceptsRoute checks if a single listener accepts the route.
// Returns RouteReasonAccepted if accepted, or rejection reason otherwise.
func (v *Validator) listenerAcceptsRoute(
	ctx context.Context,
	listener *gatewayv1.Listener,
	gatewayNamespace string,
	route *RouteInfo,
) (gatewayv1.RouteConditionReason, error) {
	return v.evaluateListenerBinding(
		ctx, listener.Hostname, listener.AllowedRoutes, listener.Protocol,
		gatewayNamespace, route,
	)
}

// evaluateListenerBinding is the shared listener-vs-route check used by both
// Gateway listeners and ListenerSet entries. The two carry different concrete
// types (Listener vs ListenerEntry) but only the same set of fields matter
// for binding: hostname, allowedRoutes, and protocol.
func (v *Validator) evaluateListenerBinding(
	ctx context.Context,
	listenerHostname *gatewayv1.Hostname,
	allowedRoutes *gatewayv1.AllowedRoutes,
	protocol gatewayv1.ProtocolType,
	parentNamespace string,
	route *RouteInfo,
) (gatewayv1.RouteConditionReason, error) {
	if !HostnamesIntersect(listenerHostname, route.Hostnames) {
		return gatewayv1.RouteReasonNoMatchingListenerHostname, nil
	}

	allowed, err := v.IsNamespaceAllowed(ctx, allowedRoutes, parentNamespace, route.Namespace)
	if err != nil {
		return "", err
	}

	if !allowed {
		return gatewayv1.RouteReasonNotAllowedByListeners, nil
	}

	if !IsRouteKindAllowed(allowedRoutes, protocol, route.Kind) {
		return gatewayv1.RouteReasonNotAllowedByListeners, nil
	}

	return gatewayv1.RouteReasonAccepted, nil
}

// getReasonMessage returns a human-readable message for a route condition reason.
func getReasonMessage(reason gatewayv1.RouteConditionReason) string {
	switch reason {
	case gatewayv1.RouteReasonNoMatchingListenerHostname:
		return "No listener hostname matches route hostnames"
	case gatewayv1.RouteReasonNotAllowedByListeners:
		return "Route not allowed by listener allowedRoutes policy"
	case gatewayv1.RouteReasonNoMatchingParent:
		return "No matching listener found"
	case gatewayv1.RouteReasonAccepted,
		gatewayv1.RouteReasonPending,
		gatewayv1.RouteReasonUnsupportedValue,
		gatewayv1.RouteReasonIncompatibleFilters,
		gatewayv1.RouteReasonResolvedRefs,
		gatewayv1.RouteReasonRefNotPermitted,
		gatewayv1.RouteReasonInvalidKind,
		gatewayv1.RouteReasonBackendNotFound,
		gatewayv1.RouteReasonUnsupportedProtocol:
		return defaultRejectionMessage
	}

	return defaultRejectionMessage
}
