package proxy

import "net/url"

// MarkUnavailableBackends sets UnavailableStatus on every backend in cfg whose
// resolved service host:port matches (namespace, name, port), leaving the
// backend in the weighted pool. The controller calls this for each invalid
// backendRef (a nonexistent Service) so the proxy returns status — 500 per the
// Gateway API spec — for that backend's traffic fraction instead of dialing a
// dead address and surfacing a 502.
//
// Matching is on the URL host (name.namespace.svc.<clusterDomain>:port), which
// is scheme-agnostic: a port-443 backend emitted as https:// still matches. The
// host encodes the service identity, so a marked backend can never collide with
// a valid sibling — two distinct services never share a host.
//
// An already-marked backend (non-zero UnavailableStatus) is left as-is, so the
// first marking wins. Callers exploit this for precedence: an invalid-ref 500
// is applied before a zero-endpoint 503, so a backend that is both nonexistent
// and endpoint-less reports 500.
func MarkUnavailableBackends(cfg *Config, clusterDomain, namespace, name string, port int32, status int) {
	if cfg == nil {
		return
	}

	target := ServiceBackendHost(clusterDomain, namespace, name, port)
	if target == "" {
		return
	}

	for ruleIdx := range cfg.Rules {
		backends := cfg.Rules[ruleIdx].Backends
		for backendIdx := range backends {
			if backends[backendIdx].UnavailableStatus != 0 {
				continue
			}

			parsed, parseErr := url.Parse(backends[backendIdx].URL)
			if parseErr == nil && parsed.Host == target {
				backends[backendIdx].UnavailableStatus = status
			}
		}
	}
}

// ServiceBackendHost returns the URL host (authority) component —
// name.namespace.svc.<clusterDomain>:port — that the converter emits for a
// Service backend. It is the stable identity key used to match a backendRef
// against the entries in a built Config: the host encodes (namespace, name,
// port) and is scheme-agnostic, so it is unaffected by TLS/protocol URL
// rewrites. Returns "" if the URL cannot be parsed.
func ServiceBackendHost(clusterDomain, namespace, name string, port int32) string {
	u, err := url.Parse(buildServiceURL(name, namespace, port, clusterDomain))
	if err != nil {
		return ""
	}

	return u.Host
}

// UnmarkedBackendHosts returns the set of URL hosts for every backend in cfg
// that is not already marked unavailable. The converter emits a backend only
// for a ref that passed kind/port validation and (for cross-namespace refs)
// ReferenceGrant authorization, so this set is the authoritative list of
// authorized, routable, not-yet-failed backends. Callers use it to gate further
// per-backend work (e.g. the zero-endpoint 503 probe) on the converter's
// decision, keeping authorization symmetric across failure paths and avoiding
// reads for backends that were dropped or already marked.
func UnmarkedBackendHosts(cfg *Config) map[string]struct{} {
	hosts := make(map[string]struct{})
	if cfg == nil {
		return hosts
	}

	for ruleIdx := range cfg.Rules {
		backends := cfg.Rules[ruleIdx].Backends
		for backendIdx := range backends {
			if backends[backendIdx].UnavailableStatus != 0 {
				continue
			}

			parsed, err := url.Parse(backends[backendIdx].URL)
			if err != nil {
				continue
			}

			hosts[parsed.Host] = struct{}{}
		}
	}

	return hosts
}
