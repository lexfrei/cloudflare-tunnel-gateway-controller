package proxy

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// reasonHostnameMatchShadowed is the condition/diagnostic reason for a rule
// whose (hostname, match) pair is exactly claimed by a higher-precedence rule
// from another route. Implementation-specific reason (the Gateway API defines
// no route-to-route hostname ownership; same-hostname routes legally merge).
const reasonHostnameMatchShadowed = "HostnameMatchShadowed"

// RuleProvenance identifies the source route of one flattened Config rule.
// Config.Provenance parallels Config.Rules index-for-index; both are appended
// together by the converters and by buildProxyConfig's gRPC merge, and nothing
// downstream reorders Rules, so the parallel structure holds. Tagged json:"-"
// transitively (the whole slice is) — it never crosses the proxy wire.
type RuleProvenance struct {
	Kind              string // "HTTPRoute" | "GRPCRoute"
	Namespace         string
	Name              string
	CreationTimestamp metav1.Time
	// RuleIndex is the route-LOCAL rule index (the index an operator sees in
	// the route's spec.rules), not the flattened config index.
	RuleIndex int
}

// String renders the provenance the way condition messages reference routes.
func (p *RuleProvenance) String() string {
	return fmt.Sprintf("%s %s/%s rule %d", p.Kind, p.Namespace, p.Name, p.RuleIndex)
}

// sameRoute reports whether two provenances point at the same route object.
func (p *RuleProvenance) sameRoute(other *RuleProvenance) bool {
	return p.Kind == other.Kind && p.Namespace == other.Namespace && p.Name == other.Name
}

// shadowKey is the identity of one servable (hostname, match) pair.
type shadowKey struct {
	hostname string
	matchKey string
}

// DetectShadowedRules walks the flattened config in router order and flags
// every (hostname, match) pair that is EXACTLY claimed by an earlier rule from
// another route. Exact equality is provably the zero-traffic case in this data
// plane: identical matches compute identical router priorities, and the stable
// ruleIndex tiebreak (flattening order = spec precedence: HTTPRoutes before
// GRPCRoutes, then oldest creationTimestamp, then {namespace}/{name}) means
// the earlier claimant always wins. Cross-bucket overlaps (wildcard vs exact
// hostname, prefix vs exact path) are deliberately NOT flagged — the later
// rule still serves traffic there.
//
// Returns one DiagnosticShadowed entry per shadowed pair, stamped with the
// LOSING route's identity so the existing per-route status pipeline delivers
// it. Rules with UnavailableStatus still claim keys: they match requests and
// answer them (fail closed), so a later identical rule is just as shadowed.
//
// A config without provenance (hand-built in tests) yields nothing.
func DetectShadowedRules(cfg *Config) []RouteDiagnostic {
	if len(cfg.Provenance) != len(cfg.Rules) {
		return nil
	}

	claims := make(map[shadowKey]RuleProvenance)

	var diags []RouteDiagnostic

	for ruleIdx := range cfg.Rules {
		rule := &cfg.Rules[ruleIdx]
		provenance := cfg.Provenance[ruleIdx]

		for _, key := range ruleShadowKeys(rule) {
			winner, claimed := claims[key]
			if !claimed {
				claims[key] = provenance

				continue
			}

			if winner.sameRoute(&provenance) {
				// Within-route duplicates are the route author's own first-rule-wins
				// ordering, spec'd separately — not a cross-tenant collision.
				continue
			}

			diags = append(diags, RouteDiagnostic{
				Namespace: provenance.Namespace,
				Name:      provenance.Name,
				RuleIndex: provenance.RuleIndex,
				Target:    DiagnosticShadowed,
				Reason:    reasonHostnameMatchShadowed,
				Message:   shadowedMessage(&provenance, key, &winner),
				WholeRule: false,
			})
		}
	}

	return diags
}

// ruleShadowKeys expands a rule into its claimed (hostname, match) keys. A
// rule with no hostnames claims the default bucket (""); a rule with no
// matches claims the matches-everything key — both mirror the router's
// behaviour exactly.
func ruleShadowKeys(rule *RouteRule) []shadowKey {
	hostnames := rule.Hostnames
	if len(hostnames) == 0 {
		hostnames = []string{""}
	}

	matchKeys := make([]string, 0, max(len(rule.Matches), 1))
	if len(rule.Matches) == 0 {
		matchKeys = append(matchKeys, "catch-all")
	}

	for _, match := range rule.Matches {
		matchKeys = append(matchKeys, canonicalMatchKey(match))
	}

	keys := make([]shadowKey, 0, len(hostnames)*len(matchKeys))

	for _, hostname := range hostnames {
		for _, matchKey := range matchKeys {
			keys = append(keys, shadowKey{hostname: strings.ToLower(hostname), matchKey: matchKey})
		}
	}

	return keys
}

// canonicalMatchKey serializes a RouteMatch into a stable, order-insensitive
// identity: header and query-param lists are sorted (names lowercased — HTTP
// header names are case-insensitive), so two spec-equal matches written in
// different order collide as they should.
func canonicalMatchKey(match RouteMatch) string {
	norm := RouteMatch{
		Path:        match.Path,
		Method:      match.Method,
		Headers:     normalizeHeaderMatches(match.Headers),
		QueryParams: normalizeQueryMatches(match.QueryParams),
	}

	encoded, err := json.Marshal(norm)
	if err != nil {
		// RouteMatch is plain data; Marshal cannot realistically fail. An
		// unkeyable match must never alias another, so make the key unique.
		return fmt.Sprintf("unmarshalable-%p", &match)
	}

	return string(encoded)
}

func normalizeHeaderMatches(headers []HeaderMatch) []HeaderMatch {
	if len(headers) == 0 {
		return nil
	}

	norm := make([]HeaderMatch, len(headers))
	for i, header := range headers {
		norm[i] = HeaderMatch{Type: header.Type, Name: strings.ToLower(header.Name), Value: header.Value}
	}

	slices.SortFunc(norm, func(a, b HeaderMatch) int {
		return strings.Compare(a.Name+"\x00"+string(a.Type)+"\x00"+a.Value, b.Name+"\x00"+string(b.Type)+"\x00"+b.Value)
	})

	return norm
}

func normalizeQueryMatches(params []QueryParamMatch) []QueryParamMatch {
	if len(params) == 0 {
		return nil
	}

	norm := slices.Clone(params)

	slices.SortFunc(norm, func(a, b QueryParamMatch) int {
		return strings.Compare(a.Name+"\x00"+string(a.Type)+"\x00"+a.Value, b.Name+"\x00"+string(b.Type)+"\x00"+b.Value)
	})

	return norm
}

// shadowBasis names the precedence criterion that decided the collision, per
// the Gateway API ordering: creationTimestamp, then {namespace}/{name}. The
// only remaining tie is cross-kind, where HTTPRoute rules precede GRPCRoute
// rules in the generated configuration — reported honestly rather than
// pretending a timestamp decided it.
func shadowBasis(winner, loser *RuleProvenance) string {
	if winner.CreationTimestamp.Before(&loser.CreationTimestamp) {
		return "older creationTimestamp"
	}

	if winner.CreationTimestamp.Equal(&loser.CreationTimestamp) {
		winnerKey := winner.Namespace + "/" + winner.Name
		loserKey := loser.Namespace + "/" + loser.Name

		if winnerKey < loserKey {
			return "alphabetical {namespace}/{name} precedence"
		}
	}

	return "HTTPRoute rules precede GRPCRoute rules in the generated configuration"
}

// shadowedMessage builds the actionable operator-facing message: which pair is
// shadowed, who wins, why, and that the route stays Accepted.
func shadowedMessage(loser *RuleProvenance, key shadowKey, winner *RuleProvenance) string {
	hostname := key.hostname
	if hostname == "" {
		hostname = "<any>"
	}

	return fmt.Sprintf(
		"rule %d match (host %q, match %s) is shadowed by %s (%s); matching requests are served by that route. "+
			"This route remains Accepted. Resolve by removing the duplicate match or scoping hostnames per tenant.",
		loser.RuleIndex, hostname, key.matchKey, winner.String(), shadowBasis(winner, loser),
	)
}
