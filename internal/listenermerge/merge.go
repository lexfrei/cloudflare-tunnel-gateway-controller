// Package listenermerge produces the precedence-ordered, conflict-annotated
// merged view of a Gateway's own listeners plus the ListenerSets that have
// been authorised to attach to it.
//
// The view is the central data structure that the controller uses to:
//
//   - Set per-listener status conditions on Gateway.status.listeners and
//     ListenerSet.status.listeners (Accepted / Programmed / Conflicted).
//   - Compute Gateway.status.attachedListenerSets.
//   - Decide route-binding eligibility (a route bound to a conflicted listener
//     is rejected with NoMatchingListener).
//
// Precedence (per Gateway API spec §ListenerSet):
//
//  1. Gateway's own listeners.
//  2. ListenerSets ordered by creationTimestamp ascending (oldest first).
//  3. ListenerSets ordered alphabetically by "{namespace}/{name}" when the
//     timestamps tie.
//
// Conflict detection within the merged view:
//
//   - Hostname conflict: two listeners share the same (port, hostname) tuple.
//   - Protocol conflict: two listeners share the same port but disagree on
//     protocol.
//
// A ListenerSet entry that conflicts loses to the higher-precedence listener
// and is annotated with ConflictReason. Two of the Gateway's own listeners have
// no precedence between them: the spec says an implementation MUST NOT pick
// one conflicting Listener as the winner, so both are annotated.
package listenermerge

import (
	"cmp"
	"slices"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ParentKind tags a merged listener with the kind of resource it originated
// from.
type ParentKind string

const (
	// ParentKindGateway means the listener is declared on the parent Gateway.
	ParentKindGateway ParentKind = "Gateway"
	// ParentKindListenerSet means the listener is declared on a ListenerSet
	// attached to the parent Gateway.
	ParentKindListenerSet ParentKind = "ListenerSet"
)

// MergedListener is one entry in the merged view.
type MergedListener struct {
	// ParentKind indicates whether this entry came from the Gateway or a
	// ListenerSet.
	ParentKind ParentKind

	// ListenerSet is non-nil when ParentKind == ParentKindListenerSet.
	ListenerSet *gatewayv1.ListenerSet

	// Index is the position of this listener within its parent resource's
	// spec.listeners slice.
	Index int

	// Name, Port, Protocol, Hostname, AllowedRoutes are copied from the source
	// listener for convenient read access without re-indexing the parent.
	Name          gatewayv1.SectionName
	Port          gatewayv1.PortNumber
	Protocol      gatewayv1.ProtocolType
	Hostname      *gatewayv1.Hostname
	AllowedRoutes *gatewayv1.AllowedRoutes

	// ConflictReason is empty when the listener is conflict-free. When set, it
	// carries the Gateway API condition reason (HostnameConflict /
	// ProtocolConflict) that explains why this listener is rejected.
	ConflictReason gatewayv1.ListenerConditionReason

	// ConflictMessage is a human-readable description, suitable for the
	// "message" field of the matching status condition. Empty when
	// ConflictReason is empty.
	ConflictMessage string
}

// MergeResult is the output of Merge.
type MergeResult struct {
	// Listeners is the precedence-ordered, conflict-annotated merged view.
	Listeners []MergedListener
}

// Merge produces the merged listener view. listenerSets MAY be passed in any
// order; Merge sorts them per Gateway API precedence rules before walking
// them.
//
// The caller is responsible for restricting listenerSets to those that pass
// the parent Gateway's spec.allowedListeners filter — Merge does not enforce
// that filter itself. ListenerSets that fail the filter still carry status
// (Accepted=False / NotAllowed), but they don't contribute listeners to the
// merged view.
func Merge(gateway *gatewayv1.Gateway, listenerSets []*gatewayv1.ListenerSet) *MergeResult {
	sorted := sortListenerSets(listenerSets)

	merged := make([]MergedListener, 0, estimateCapacity(gateway, sorted))
	merged = appendGatewayListeners(merged, gateway)

	for _, ls := range sorted {
		merged = appendListenerSetEntries(merged, ls)
	}

	annotateConflicts(merged)

	return &MergeResult{Listeners: merged}
}

func appendGatewayListeners(out []MergedListener, gateway *gatewayv1.Gateway) []MergedListener {
	if gateway == nil {
		return out
	}

	for i := range gateway.Spec.Listeners {
		listener := &gateway.Spec.Listeners[i]
		out = append(out, MergedListener{
			ParentKind:    ParentKindGateway,
			Index:         i,
			Name:          listener.Name,
			Port:          listener.Port,
			Protocol:      listener.Protocol,
			Hostname:      listener.Hostname,
			AllowedRoutes: listener.AllowedRoutes,
		})
	}

	return out
}

func appendListenerSetEntries(out []MergedListener, ls *gatewayv1.ListenerSet) []MergedListener {
	for i := range ls.Spec.Listeners {
		entry := &ls.Spec.Listeners[i]
		out = append(out, MergedListener{
			ParentKind:    ParentKindListenerSet,
			ListenerSet:   ls,
			Index:         i,
			Name:          entry.Name,
			Port:          entry.Port,
			Protocol:      entry.Protocol,
			Hostname:      entry.Hostname,
			AllowedRoutes: entry.AllowedRoutes,
		})
	}

	return out
}

func estimateCapacity(gateway *gatewayv1.Gateway, sets []*gatewayv1.ListenerSet) int {
	total := 0

	if gateway != nil {
		total += len(gateway.Spec.Listeners)
	}

	for _, ls := range sets {
		total += len(ls.Spec.Listeners)
	}

	return total
}

func sortListenerSets(in []*gatewayv1.ListenerSet) []*gatewayv1.ListenerSet {
	out := make([]*gatewayv1.ListenerSet, len(in))
	copy(out, in)

	slices.SortStableFunc(out, func(left, right *gatewayv1.ListenerSet) int {
		if c := left.CreationTimestamp.Compare(right.CreationTimestamp.Time); c != 0 {
			return c
		}

		if c := cmp.Compare(left.Namespace, right.Namespace); c != 0 {
			return c
		}

		return cmp.Compare(left.Name, right.Name)
	})

	return out
}

// annotateConflicts walks the merged view in precedence order and marks each
// listener with a conflict reason if an earlier listener has already claimed
// the same (port, hostname) tuple (HostnameConflict) or used a different
// protocol on the same port (ProtocolConflict).
func annotateConflicts(merged []MergedListener) {
	// First claimant (index into merged) of each port's protocol — used to
	// detect ProtocolConflict.
	protoOwner := make(map[gatewayv1.PortNumber]int)
	// First claimant of each (port, hostname) — used to detect HostnameConflict.
	hostnameOwner := make(map[hostnameKey]int)

	for i := range merged {
		entry := &merged[i]

		// Protocol-conflict has precedence over hostname-conflict per spec.
		if owner, ok := protoOwner[entry.Port]; ok && merged[owner].Protocol != entry.Protocol {
			markConflict(merged, owner, i, gatewayv1.ListenerReasonProtocolConflict, "protocol")

			continue
		}

		key := hostnameKey{port: entry.Port, hostname: hostnameValue(entry.Hostname)}

		if owner, taken := hostnameOwner[key]; taken {
			markConflict(merged, owner, i, gatewayv1.ListenerReasonHostnameConflict, "hostname")

			continue
		}

		// Claim the port's protocol and the (port,hostname) slot.
		if _, ok := protoOwner[entry.Port]; !ok {
			protoOwner[entry.Port] = i
		}

		hostnameOwner[key] = i
	}

	markGatewayProtocolConflicts(merged)
}

// markGatewayProtocolConflicts refuses every Gateway-owned listener on a port
// where the Gateway's own listeners mix protocols. The walk above compares a
// listener only with the port's first claimant, so a later listener matching
// that claimant's protocol would otherwise be left as the winner.
func markGatewayProtocolConflicts(merged []MergedListener) {
	protocols := make(map[gatewayv1.PortNumber]map[gatewayv1.ProtocolType]bool)

	for i := range merged {
		if merged[i].ParentKind != ParentKindGateway {
			continue
		}

		if protocols[merged[i].Port] == nil {
			protocols[merged[i].Port] = make(map[gatewayv1.ProtocolType]bool)
		}

		protocols[merged[i].Port][merged[i].Protocol] = true
	}

	for i := range merged {
		if merged[i].ParentKind == ParentKindGateway && len(protocols[merged[i].Port]) > 1 {
			merged[i].ConflictReason = gatewayv1.ListenerReasonProtocolConflict
			merged[i].ConflictMessage = "Listener conflicts on protocol with another listener of this Gateway on the same port"
		}
	}
}

// markConflict annotates merged[loser], which clashes with the earlier
// claimant merged[owner]. When both are the Gateway's own listeners the owner
// is annotated too, because neither may win.
func markConflict(merged []MergedListener, owner, loser int, reason gatewayv1.ListenerConditionReason, field string) {
	if merged[owner].ParentKind == ParentKindGateway && merged[loser].ParentKind == ParentKindGateway {
		message := "Listener conflicts on " + field + " with another listener of this Gateway on the same port"

		merged[loser].ConflictReason, merged[loser].ConflictMessage = reason, message
		if merged[owner].ConflictReason == "" {
			merged[owner].ConflictReason, merged[owner].ConflictMessage = reason, message
		}

		return
	}

	merged[loser].ConflictReason = reason
	merged[loser].ConflictMessage = "Listener conflicts on " + field + " with a higher-precedence listener on the same port"
}

type hostnameKey struct {
	port     gatewayv1.PortNumber
	hostname string
}

func hostnameValue(h *gatewayv1.Hostname) string {
	if h == nil {
		return ""
	}

	return string(*h)
}
