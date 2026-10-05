package proxy

import (
	"slices"
	"strings"
)

// noListenerHostname names the owner of a request no listener of a Gateway
// matches. It is not a valid hostname, so no rule is attached through it.
const noListenerHostname = "\x00"

// exactHostnameSpecificity outranks every wildcard: a DNS name has at most
// 127 labels.
const exactHostnameSpecificity = 128

// listenerOwners holds, per Gateway, its listeners ordered most specific
// hostname first, so the first one matching a request is the listener that
// owns it under Gateway API listener isolation.
type listenerOwners map[string][]Listener

func compileListenerOwners(gateways map[string][]Listener) listenerOwners {
	if len(gateways) == 0 {
		return nil
	}

	owners := make(listenerOwners, len(gateways))

	for gateway, listeners := range gateways {
		ordered := make([]Listener, 0, len(listeners))
		for _, listener := range listeners {
			ordered = append(ordered, normalizeListener(listener))
		}

		slices.SortStableFunc(ordered, func(a, b Listener) int {
			return listenerSpecificity(b.Hostname) - listenerSpecificity(a.Hostname)
		})

		owners[gateway] = ordered
	}

	return owners
}

func normalizeListener(listener Listener) Listener {
	listener.Hostname = strings.ToLower(listener.Hostname)

	return listener
}

// listenerSpecificity orders listener hostnames that match a common host: an
// exact hostname beats any wildcard, a wildcard with more labels beats one
// with fewer, and any hostname beats none.
func listenerSpecificity(hostname string) int {
	switch {
	case hostname == "":
		return 0
	case strings.HasPrefix(hostname, "*."):
		return 1 + strings.Count(hostname, ".")
	default:
		return exactHostnameSpecificity
	}
}

func listenerMatches(listener Listener, host string, port int32) bool {
	if listener.Port != 0 && listener.Port != port {
		return false
	}

	switch {
	case listener.Hostname == "":
		return true
	case strings.HasPrefix(listener.Hostname, "*."):
		return matchesWildcard(host, listener.Hostname[1:])
	default:
		return listener.Hostname == host
	}
}

// hostOwners resolves the owning listener of one request host and port per
// Gateway, remembering each answer for the rest of the lookup.
type hostOwners struct {
	owners listenerOwners
	host   string
	port   int32
	memo   map[string]Listener
}

// owner returns the listener of gateway that owns the request, and false
// when the config carries no listeners for gateway.
func (h *hostOwners) owner(gateway string) (Listener, bool) {
	if owner, ok := h.memo[gateway]; ok {
		return owner, true
	}

	listeners, ok := h.owners[gateway]
	if !ok {
		return Listener{}, false
	}

	owner := Listener{Hostname: noListenerHostname}

	for _, listener := range listeners {
		if listenerMatches(listener, h.host, h.port) {
			owner = listener

			break
		}
	}

	if h.memo == nil {
		h.memo = make(map[string]Listener, len(h.owners))
	}

	h.memo[gateway] = owner

	return owner, true
}

// isolationAllows reports whether a rule may answer the request: some Gateway
// the rule's route is attached to gives the request to one of the listeners
// it is attached through. A rule without listener data, or attached to a
// Gateway the config carries no listeners for, is not isolated.
func (c *compiledRule) isolationAllows(hosts *hostOwners) bool {
	if len(c.listeners) == 0 {
		return true
	}

	for gateway, attached := range c.listeners {
		owner, known := hosts.owner(gateway)
		if !known {
			return true
		}

		if _, ok := attached[owner]; ok {
			return true
		}
	}

	return false
}

func compileRuleListeners(listeners map[string][]Listener) map[string]map[Listener]struct{} {
	if len(listeners) == 0 {
		return nil
	}

	out := make(map[string]map[Listener]struct{}, len(listeners))

	for gateway, attached := range listeners {
		set := make(map[Listener]struct{}, len(attached))
		for _, listener := range attached {
			set[normalizeListener(listener)] = struct{}{}
		}

		out[gateway] = set
	}

	return out
}
