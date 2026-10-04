package routebinding

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/cockroachdb/errors"
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
	Accepted bool
	// Incomplete is set when an entry the route might also bind to could not
	// be evaluated, so MatchedListeners may be missing it and the binding has
	// to be evaluated again.
	Incomplete       bool
	Reason           gatewayv1.RouteConditionReason
	Message          string
	MatchedListeners []gatewayv1.SectionName
}

// ValidateBinding validates whether a route can bind to a gateway's listeners.
// It returns a BindingResult indicating acceptance status, reason, and matched listeners.
// The error reports that no listener admits the route while one could not be
// evaluated, such as one whose namespace selector needs a namespace that cannot
// be read. When another listener admits the route, the result is Incomplete
// instead.
func (v *Validator) ValidateBinding(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	route *RouteInfo,
) (BindingResult, error) {
	listeners := gateway.Spec.Listeners

	matched, rejectionReason, invalid, detail, err := findMatchingEntries(
		"listener",
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
	v.logUnevaluatedListeners(ctx, route, "Gateway "+gateway.Namespace+"/"+gateway.Name, detail)

	if err != nil && len(matched) == 0 {
		return BindingResult{}, err
	}

	result := makeBindingResult(matched, rejectionReason, invalid)
	result.Incomplete = err != nil

	return result, nil
}

// logUnevaluatedListeners logs the errors of listeners that could not be
// evaluated. They quote the Gateway's spec, which the route's authors may not
// be allowed to read, so they go to the controller log and not to route status.
func (v *Validator) logUnevaluatedListeners(ctx context.Context, route *RouteInfo, parent, detail string) {
	if detail == "" {
		return
	}

	name := route.Namespace + "/" + route.Name
	key := fmt.Sprintf("%s %s %s", route.Kind, name, parent)

	logging.FromContext(ctx).Log(ctx, v.repeats.Level(key, detail, v.unevaluatedLevel),
		"listeners could not be evaluated for a route", "route", name, "parent", parent, "detail", detail)
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
			fmt.Fprintf(&message, "; %s has an invalid allowedRoutes selector", name)
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
// == Accepted are collected into the matched-section list. When no entry
// matches, the last observed non-accepted reason is the rejection reason, and
// NoMatchingParent is reserved for a sectionName or port that no entry has.
//
// An entry with an unparseable namespace selector admits nothing. Its error is
// returned in detail, and, when no entry matched, its name in invalid. It does
// not stop the loop: the error belongs to that entry, and a sibling entry may
// still admit the route. Any other evaluation error, such as a failed namespace
// read, leaves that entry undecided: it is added to detail, the loop goes on,
// and the first such error is returned with whatever the other entries matched.
func findMatchingEntries(
	entryKind string,
	count int,
	nameAndPort func(int) (gatewayv1.SectionName, gatewayv1.PortNumber),
	accept func(int) (gatewayv1.RouteConditionReason, error),
	routeSectionName *gatewayv1.SectionName,
	routePort *gatewayv1.PortNumber,
) ([]gatewayv1.SectionName, gatewayv1.RouteConditionReason, []string, string, error) {
	if count == 0 {
		return nil, gatewayv1.RouteReasonNoMatchingParent, nil, "", nil
	}

	var (
		matched             []gatewayv1.SectionName
		lastRejectionReason gatewayv1.RouteConditionReason
		invalid             []string
		entryErrors         []string
		readErr             error
	)

	for i := range count {
		name, port := nameAndPort(i)

		if (routeSectionName != nil && *routeSectionName != name) || (routePort != nil && *routePort != port) {
			continue
		}

		reason, err := accept(i)
		if err != nil && !errors.Is(err, errInvalidSelector) {
			err = fmt.Errorf("%s %q: %w", entryKind, name, err)
			entryErrors = append(entryErrors, err.Error())

			if readErr == nil {
				readErr = err
			}

			continue
		}

		if err != nil {
			reason = gatewayv1.RouteReasonNotAllowedByListeners

			invalid = append(invalid, fmt.Sprintf("%s %q", entryKind, name))
			entryErrors = append(entryErrors, fmt.Sprintf("%s %q: %v", entryKind, name, err))
		}

		if reason == gatewayv1.RouteReasonAccepted {
			matched = append(matched, name)
		} else {
			lastRejectionReason = reason
		}
	}

	detail := strings.Join(entryErrors, "; ")

	if len(matched) > 0 {
		return matched, "", nil, detail, readErr
	}

	return nil, cmp.Or(lastRejectionReason, gatewayv1.RouteReasonNoMatchingParent), invalid, detail, readErr
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
