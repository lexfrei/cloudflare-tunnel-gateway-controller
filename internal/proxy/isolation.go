package proxy

import (
	"slices"
	"strings"
)

// noListener is the owner of a host no listener of a Gateway matches. It is
// not a valid hostname, so no rule is attached through it.
const noListener = "\x00"

// exactHostnameSpecificity outranks every wildcard: a DNS name has at most
// 127 labels.
const exactHostnameSpecificity = 128

// listenerOwners holds, per Gateway, its listener hostnames ordered most
// specific first, so the first one matching a host is the listener that owns
// the host under Gateway API listener isolation.
type listenerOwners map[string][]string

func compileListenerOwners(gateways map[string][]string) listenerOwners {
	if len(gateways) == 0 {
		return nil
	}

	owners := make(listenerOwners, len(gateways))

	for gateway, hostnames := range gateways {
		ordered := make([]string, 0, len(hostnames))
		for _, hostname := range hostnames {
			ordered = append(ordered, strings.ToLower(hostname))
		}

		slices.SortStableFunc(ordered, func(a, b string) int {
			return listenerSpecificity(b) - listenerSpecificity(a)
		})

		owners[gateway] = ordered
	}

	return owners
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

func listenerMatches(listener, host string) bool {
	switch {
	case listener == "":
		return true
	case strings.HasPrefix(listener, "*."):
		return matchesWildcard(host, listener[1:])
	default:
		return listener == host
	}
}

// hostOwners resolves the owning listener of one request host per Gateway,
// remembering each answer for the rest of the lookup.
type hostOwners struct {
	owners listenerOwners
	host   string
	memo   map[string]string
}

// owner returns the hostname of the listener of gateway that owns the host,
// and false when the config carries no listeners for gateway.
func (h *hostOwners) owner(gateway string) (string, bool) {
	if owner, ok := h.memo[gateway]; ok {
		return owner, true
	}

	listeners, ok := h.owners[gateway]
	if !ok {
		return "", false
	}

	owner := noListener

	for _, listener := range listeners {
		if listenerMatches(listener, h.host) {
			owner = listener

			break
		}
	}

	if h.memo == nil {
		h.memo = make(map[string]string, len(h.owners))
	}

	h.memo[gateway] = owner

	return owner, true
}

// isolationAllows reports whether a rule may answer the host: some Gateway the
// rule's route is attached to gives the host to one of the listeners it is
// attached through. A rule without listener data, or attached to a Gateway the
// config carries no listeners for, is not isolated.
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

func compileRuleListeners(listeners map[string][]string) map[string]map[string]struct{} {
	if len(listeners) == 0 {
		return nil
	}

	out := make(map[string]map[string]struct{}, len(listeners))

	for gateway, hostnames := range listeners {
		set := make(map[string]struct{}, len(hostnames))
		for _, hostname := range hostnames {
			set[strings.ToLower(hostname)] = struct{}{}
		}

		out[gateway] = set
	}

	return out
}
