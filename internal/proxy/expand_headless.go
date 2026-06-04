package proxy

import (
	"math"
	"net"
	"net/url"
	"strconv"
)

// ResolvedEndpoint is one ready endpoint of a headless Service: the dial host
// (a pod IP, or rarely an FQDN for an FQDN-type EndpointSlice) and the endpoint
// port — the resolved targetPort that the pod actually listens on, taken from the
// EndpointSlice rather than the Service port.
type ResolvedEndpoint struct {
	Host string
	Port int32
}

// ExpandHeadlessBackend replaces every unmarked, weight>0 backend in cfg whose
// service host:port matches (namespace, name, servicePort) with one backend per
// resolved endpoint, dialing the endpoint host:port instead of the Service FQDN
// at the Service port. It is how the controller routes to a headless Service
// (clusterIP: None), which has no VIP: the FQDN resolves straight to the pod IPs,
// so dialing the Service port (8080) reaches a pod that listens on the targetPort
// (3000) and fails. Each endpoint dials the resolved targetPort instead.
//
// Matching is on the URL host via ServiceBackendHost — the same scheme-agnostic
// identity key as MarkUnavailableBackends — so an https (port-443 or TLS-policy)
// backend still matches. Already-marked backends (500/503) and weight-0 refs are
// left untouched: a marking carries that backend's traffic fraction and must
// survive, and a weight-0 ref takes no traffic so expanding it would only perturb
// sibling weights.
//
// Weight handling preserves the Gateway API traffic proportion. Within a rule,
// the matched backend (weight W) becomes N endpoints each of weight W, and every
// other backend's weight is scaled by N. The matched ref's aggregate is then N*W
// against siblings scaled by N, so the inter-backendRef ratio is unchanged while
// the N endpoints split that fraction equally. Applied once per Service, multiple
// headless services in one rule compose to the correct aggregate ratio. After
// scaling, the rule's weights are divided by their GCD to keep magnitudes small;
// the per-backend scale is computed in int64 and clamped to guard int32 overflow.
//
// Per-endpoint backends inherit the matched backend's Protocol, TLS (the SNI/
// ServerName lives in BackendTLSConfig, not the URL host, so it stays correct
// when the URL becomes an IP), Filters and WebSocket flag — only the dial URL
// changes. An empty endpoint set is a no-op, leaving the FQDN backend so the
// zero-endpoint pass can mark it 503.
func ExpandHeadlessBackend(cfg *Config, clusterDomain, namespace, name string, servicePort int32, endpoints []ResolvedEndpoint) {
	if cfg == nil || len(endpoints) == 0 {
		return
	}

	target := ServiceBackendHost(clusterDomain, namespace, name, servicePort)
	if target == "" {
		return
	}

	//nolint:gosec // endpoint count is bounded by EndpointSlice size, never overflows int32
	scale := int32(len(endpoints))

	for ruleIdx := range cfg.Rules {
		expandRuleBackends(&cfg.Rules[ruleIdx], target, scale, endpoints)
	}
}

// expandRuleBackends rewrites a single rule's backend list when it contains an
// expandable backend matching target, scaling siblings and reducing by GCD.
func expandRuleBackends(rule *RouteRule, target string, scale int32, endpoints []ResolvedEndpoint) {
	if !ruleHasExpandableBackend(rule, target) {
		return
	}

	expanded := make([]BackendRef, 0, len(rule.Backends)+len(endpoints))

	for i := range rule.Backends {
		backend := rule.Backends[i]
		if backendMatchesHeadless(&backend, target) {
			expanded = append(expanded, endpointBackends(&backend, endpoints)...)

			continue
		}

		backend.Weight = scaleWeight(backend.Weight, scale)
		expanded = append(expanded, backend)
	}

	rule.Backends = expanded
	reduceWeightsByGCD(rule.Backends)
}

// ruleHasExpandableBackend reports whether the rule contains at least one
// unmarked, weight>0 backend matching target.
func ruleHasExpandableBackend(rule *RouteRule, target string) bool {
	for i := range rule.Backends {
		if backendMatchesHeadless(&rule.Backends[i], target) {
			return true
		}
	}

	return false
}

// backendMatchesHeadless reports whether a backend is an expandable match for the
// target service host: unmarked, carrying traffic, and on the target host.
func backendMatchesHeadless(backend *BackendRef, target string) bool {
	if backend.UnavailableStatus != 0 || backend.Weight <= 0 {
		return false
	}

	parsed, err := url.Parse(backend.URL)

	return err == nil && parsed.Host == target
}

// endpointBackends clones the matched backend once per endpoint, swapping only
// the dial URL to the endpoint host:port (IPv6 auto-bracketed via JoinHostPort).
func endpointBackends(matched *BackendRef, endpoints []ResolvedEndpoint) []BackendRef {
	scheme := backendScheme(matched.URL)
	out := make([]BackendRef, 0, len(endpoints))

	for _, ep := range endpoints {
		backend := *matched
		backend.URL = scheme + "://" + net.JoinHostPort(ep.Host, strconv.Itoa(int(ep.Port)))
		out = append(out, backend)
	}

	return out
}

// backendScheme extracts the scheme from a backend URL, defaulting to http when
// it cannot be parsed.
func backendScheme(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err == nil && parsed.Scheme != "" {
		return parsed.Scheme
	}

	return schemeHTTP
}

// scaleWeight multiplies a weight by scale in int64, clamping to MaxInt32 so a
// pathological product never wraps negative; GCD reduction keeps realistic
// magnitudes small.
func scaleWeight(weight, scale int32) int32 {
	scaled := int64(weight) * int64(scale)
	if scaled > math.MaxInt32 {
		return math.MaxInt32
	}

	//nolint:gosec // clamped to MaxInt32 above; inputs are non-negative
	return int32(scaled)
}

// reduceWeightsByGCD divides every weight in the slice by their greatest common
// divisor, preserving ratios while shrinking magnitudes. A GCD of 0 or 1 is a
// no-op.
func reduceWeightsByGCD(backends []BackendRef) {
	var divisor int32
	for i := range backends {
		divisor = gcdInt32(divisor, backends[i].Weight)
	}

	if divisor <= 1 {
		return
	}

	for i := range backends {
		backends[i].Weight /= divisor
	}
}

// gcdInt32 returns the greatest common divisor of |m| and |n|; gcd(0, x) == |x|.
func gcdInt32(m, n int32) int32 {
	if m < 0 {
		m = -m
	}

	if n < 0 {
		n = -n
	}

	for n != 0 {
		m, n = n, m%n
	}

	return m
}
