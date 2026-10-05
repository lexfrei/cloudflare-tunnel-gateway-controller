package routebinding

import (
	"context"

	"github.com/cockroachdb/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/logging"
)

// ListenerSetAcceptance describes whether a Gateway accepts a particular
// ListenerSet attachment based on the Gateway's spec.allowedListeners filter.
type ListenerSetAcceptance struct {
	Accepted bool
	Reason   gatewayv1.ListenerSetConditionReason
	// Message explains a refusal the generic not-allowed message would not.
	// Empty otherwise.
	Message string
	// Err is the error behind a refusal the Gateway's spec caused, for the
	// caller's log only: it quotes that spec. Nil otherwise.
	Err error
}

// invalidAllowedListenersMessage tells a ListenerSet owner why it is refused
// without quoting the Gateway's selector, which the owner may not be allowed
// to read.
const invalidAllowedListenersMessage = "The parent Gateway's allowedListeners selector is invalid"

// EvaluateListenerSetAcceptance applies the parent Gateway's
// spec.allowedListeners.namespaces filter to decide if the given ListenerSet
// is allowed to attach. The default (unset) is From=None, i.e. attachment is
// rejected unless the Gateway opts in. A selector that does not parse admits
// no ListenerSet. The error reports a namespace that could not be read, which
// leaves the acceptance undecided. A Gateway that RequestsFrontendValidation
// admits no ListenerSet.
func (v *Validator) EvaluateListenerSetAcceptance(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
	listenerSet *gatewayv1.ListenerSet,
) (ListenerSetAcceptance, error) {
	if RequestsFrontendValidation(gateway) {
		return ListenerSetAcceptance{
			Reason:  gatewayv1.ListenerSetReasonParentNotAccepted,
			Message: ParentRequestsFrontendValidationMessage,
		}, nil
	}

	from := getListenerNamespaceFrom(gateway.Spec.AllowedListeners)

	switch from {
	case gatewayv1.NamespacesFromAll:
		return acceptedListenerSet(), nil
	case gatewayv1.NamespacesFromSame:
		if gateway.Namespace == listenerSet.Namespace {
			return acceptedListenerSet(), nil
		}

		return rejectedListenerSet(), nil
	case gatewayv1.NamespacesFromSelector:
		ok, err := v.listenerSetNamespaceMatchesSelector(ctx, gateway.Spec.AllowedListeners, listenerSet.Namespace)
		if err != nil && !errors.Is(err, errInvalidSelector) {
			return ListenerSetAcceptance{}, err
		}

		if err != nil {
			// The error quotes the Gateway's spec, which the ListenerSet's and
			// the routes' authors may not be allowed to read, so the ListenerSet
			// is refused like any other that is not allowed and the error only
			// goes to logs. Every route on the ListenerSet reaches this once per
			// sync, so it logs at debug; the ListenerSet reconcile warns.
			logging.FromContext(ctx).Debug("Gateway allowedListeners selector could not be evaluated",
				"gateway", gateway.Namespace+"/"+gateway.Name,
				"listenerSet", listenerSet.Namespace+"/"+listenerSet.Name,
				"error", err)

			refused := rejectedListenerSet()
			refused.Message = invalidAllowedListenersMessage
			refused.Err = err

			return refused, nil
		}

		if ok {
			return acceptedListenerSet(), nil
		}

		return rejectedListenerSet(), nil
	case gatewayv1.NamespacesFromNone:
		return rejectedListenerSet(), nil
	}

	return rejectedListenerSet(), nil
}

func acceptedListenerSet() ListenerSetAcceptance {
	return ListenerSetAcceptance{Accepted: true, Reason: gatewayv1.ListenerSetReasonAccepted}
}

func rejectedListenerSet() ListenerSetAcceptance {
	return ListenerSetAcceptance{Accepted: false, Reason: gatewayv1.ListenerSetReasonNotAllowed}
}

// ListenerNamespaceSelectorInvalid reports whether allowedListeners admits
// ListenerSets by a namespace label selector that does not parse, which
// refuses every ListenerSet. A selector is read only when From is Selector.
func ListenerNamespaceSelectorInvalid(allowed *gatewayv1.AllowedListeners) bool {
	if getListenerNamespaceFrom(allowed) != gatewayv1.NamespacesFromSelector || allowed.Namespaces.Selector == nil {
		return false
	}

	_, err := metav1.LabelSelectorAsSelector(allowed.Namespaces.Selector)

	return err != nil
}

// getListenerNamespaceFrom extracts the From field from allowedListeners,
// defaulting to None when unset (per Gateway API spec — ListenerSets are
// rejected unless the Gateway explicitly opts in).
func getListenerNamespaceFrom(allowed *gatewayv1.AllowedListeners) gatewayv1.FromNamespaces {
	if allowed == nil || allowed.Namespaces == nil || allowed.Namespaces.From == nil {
		return gatewayv1.NamespacesFromNone
	}

	return *allowed.Namespaces.From
}

// listenerSetNamespaceMatchesSelector evaluates the namespace label selector
// for a ListenerSet's namespace against the Gateway's allowedListeners filter.
// A missing namespace does not match, as for routes.
func (v *Validator) listenerSetNamespaceMatchesSelector(
	ctx context.Context,
	allowed *gatewayv1.AllowedListeners,
	namespace string,
) (bool, error) {
	if allowed == nil || allowed.Namespaces == nil || allowed.Namespaces.Selector == nil {
		return false, nil
	}

	// Reuse the route-namespace selector code path by wrapping the selector in
	// an AllowedRoutes shell.
	wrapped := &gatewayv1.AllowedRoutes{
		Namespaces: &gatewayv1.RouteNamespaces{
			Selector: allowed.Namespaces.Selector,
		},
	}

	return v.namespaceMatchesSelector(ctx, wrapped, namespace)
}

// ValidateBindingForListenerSet validates whether a route can bind to one of
// the entries of a ListenerSet. The semantics mirror ValidateBinding for
// Gateway listeners: hostname intersection, namespace allowance, route-kind
// allowance, sectionName + port filters.
func (v *Validator) ValidateBindingForListenerSet(
	ctx context.Context,
	listenerSet *gatewayv1.ListenerSet,
	route *RouteInfo,
) (BindingResult, error) {
	entries := listenerSet.Spec.Listeners

	matched, rejectionReason, invalid, detail, err := findMatchingEntries(
		"ListenerSet entry",
		len(entries),
		func(i int) (gatewayv1.SectionName, gatewayv1.PortNumber) {
			return entries[i].Name, entries[i].Port
		},
		func(i int) (gatewayv1.RouteConditionReason, error) {
			entry := &entries[i]

			return v.evaluateListenerBinding(
				ctx, entry.Hostname, entry.AllowedRoutes, entry.Protocol,
				listenerSet.Namespace, route,
			)
		},
		route.SectionName,
		route.Port,
	)
	v.logUnevaluatedListeners(ctx, route, "ListenerSet "+listenerSet.Namespace+"/"+listenerSet.Name, detail)

	if err != nil && len(matched) == 0 {
		return BindingResult{}, err
	}

	result := makeBindingResult(matched, rejectionReason, invalid)
	result.Incomplete = err != nil

	return result, nil
}
