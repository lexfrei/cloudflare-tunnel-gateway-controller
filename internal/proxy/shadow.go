package proxy

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReasonHostnameMatchShadowed is the condition/diagnostic reason for a rule
// whose (hostname, match) pair is exactly claimed by a higher-precedence rule
// from another route. Implementation-specific reason (the Gateway API defines
// no route-to-route hostname ownership; same-hostname routes legally merge).
const ReasonHostnameMatchShadowed = "HostnameMatchShadowed"

// Route kinds stamped on RuleProvenance.Kind by the converters.
const (
	kindHTTPRoute = "HTTPRoute"
	kindGRPCRoute = "GRPCRoute"
)

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

// shadowClaimant is one rule's claim on a (hostname, match) pair, carrying
// the data the router actually orders by: rule priority (max across the
// rule's ORed matches — computePriority, the router's own function) and the
// flattened index (the router's stable tiebreak).
type shadowClaimant struct {
	provenance RuleProvenance
	priority   int
	flatIdx    int
}

// beats reports whether c is served BEFORE other by the router: higher
// priority first, then lower flattened index (sortRulesByPrecedence).
func (c *shadowClaimant) beats(other *shadowClaimant) bool {
	if c.priority != other.priority {
		return c.priority > other.priority
	}

	return c.flatIdx < other.flatIdx
}

// DetectShadowedRules flags every (hostname, match) pair that is EXACTLY
// claimed by two routes, attributing winner and loser by the ROUTER's actual
// ordering: rule priority descending (a rule's priority is the max across its
// ORed matches), then flattened index — NOT flattening order alone. The
// distinction matters: a newer route [prefix /, exact /admin] outranks an
// older route [prefix /] because its exact arm lifts the whole rule, and its
// "prefix /" arm then swallows the older route's traffic — the OLDER route is
// the starved one. Exact pair equality is the zero-traffic case: the winning
// rule matches every request the losing pair would, and the router serves the
// winner first. Cross-bucket overlaps (wildcard vs exact hostname, prefix vs
// exact path) are deliberately NOT flagged — the other rule still serves
// traffic there.
//
// Returns one DiagnosticShadowed entry per shadowed pair, stamped with the
// LOSING route's identity so the existing per-route status pipeline delivers
// it. Rules with UnavailableStatus still claim keys: they match requests and
// answer them (fail closed), so an outranked identical rule is just as
// shadowed.
//
// A config without provenance (hand-built in tests) yields nothing.
func DetectShadowedRules(cfg *Config) []RouteDiagnostic {
	if len(cfg.Provenance) != len(cfg.Rules) {
		return nil
	}

	// Pass 1: collect every claim and reduce each key to the rule the router
	// ACTUALLY serves. Two passes on purpose: the message promises "matching
	// requests are served by that route", and with three or more claimants on
	// one key the true winner is only known after all of them are seen —
	// emitting against the running incumbent would name an intermediate
	// claimant that itself serves zero traffic on the pair.
	winners := make(map[shadowKey]shadowClaimant)

	var claims []shadowClaim

	for ruleIdx := range cfg.Rules {
		rule := &cfg.Rules[ruleIdx]
		claimant := shadowClaimant{
			provenance: cfg.Provenance[ruleIdx],
			priority:   computePriority(rule),
			flatIdx:    ruleIdx,
		}

		for _, key := range ruleShadowKeys(rule) {
			claims = append(claims, shadowClaim{key: key, claimant: claimant})

			incumbent, claimed := winners[key]
			if !claimed || claimant.beats(&incumbent) {
				winners[key] = claimant
			}
		}
	}

	return shadowDiagnostics(claims, winners)
}

type shadowClaim struct {
	key      shadowKey
	claimant shadowClaimant
}

// shadowDiagnostics is Pass 2: every losing claim gets a diagnostic naming the
// final winner. A rule can claim the same key more than once — duplicate
// matches, which the CRD list-map-key does not dedup for path-only matches — so
// it collapses to one diagnostic per (losing rule, key) to avoid redundant
// conditions/Events.
func shadowDiagnostics(claims []shadowClaim, winners map[shadowKey]shadowClaimant) []RouteDiagnostic {
	type emittedClaim struct {
		flatIdx int
		key     shadowKey
	}

	var diags []RouteDiagnostic

	emitted := make(map[emittedClaim]struct{}, len(claims))

	for i := range claims {
		claim := &claims[i]

		winner := winners[claim.key]
		if winner.flatIdx == claim.claimant.flatIdx {
			continue // this claim IS the winner
		}

		if winner.provenance.sameRoute(&claim.claimant.provenance) {
			// Within-route duplicates are the route author's own
			// first-rule-wins ordering, spec'd separately — not a
			// cross-tenant collision.
			continue
		}

		dedupe := emittedClaim{flatIdx: claim.claimant.flatIdx, key: claim.key}
		if _, done := emitted[dedupe]; done {
			continue // a duplicate match already emitted this exact diagnostic
		}

		emitted[dedupe] = struct{}{}

		diags = append(diags, RouteDiagnostic{
			Namespace: claim.claimant.provenance.Namespace,
			Name:      claim.claimant.provenance.Name,
			RuleIndex: claim.claimant.provenance.RuleIndex,
			Target:    DiagnosticShadowed,
			Reason:    ReasonHostnameMatchShadowed,
			Message:   shadowedMessage(&claim.claimant, claim.key, &winner),
			WholeRule: false,
		})
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
	// Full struct copy, then overwrite ONLY the normalized slices: an
	// explicit field-by-field copy would silently exclude any future
	// RouteMatch field from the identity, making matches that differ only in
	// that field falsely collide.
	norm := match
	norm.Headers = normalizeHeaderMatches(match.Headers)
	norm.QueryParams = normalizeQueryMatches(match.QueryParams)

	encoded, err := json.Marshal(norm)
	if err != nil {
		// RouteMatch is plain data; Marshal cannot realistically fail. The
		// fallback must still key by CONTENT so equal matches collide.
		return unmarshalableMatchKey(norm)
	}

	return string(encoded)
}

// unmarshalableMatchKey is the content-stable fallback for canonicalMatchKey
// when json.Marshal fails. RouteMatch.Path is a pointer, so a bare %#v would
// render its ADDRESS — making two spec-identical matches produce different keys
// and silently miss the shadow collision. Dereference Path explicitly (the only
// pointer field) and drop the pointer from the struct dump so the rest of the
// fields (slices, strings) contribute their content.
func unmarshalableMatchKey(norm RouteMatch) string {
	path := "nil"
	if norm.Path != nil {
		path = fmt.Sprintf("%#v", *norm.Path)
	}

	norm.Path = nil

	return fmt.Sprintf("unmarshalable|path=%s|%#v", path, norm)
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

// shadowBasis names the criterion that decided the collision. A priority gap
// means the winning RULE outranks the loser on Gateway API match specificity
// (its most specific ORed match sets the whole rule's rank). Equal priorities
// are decided by beats() PURELY on flattened index, so a timestamp/name reason
// is reported only when it actually agrees with that order; otherwise the
// honest answer is the generated-config order itself (e.g. HTTPRoute rules
// precede GRPCRoute rules) — never a timestamp or name that did not decide it.
func shadowBasis(winner, loser *shadowClaimant) string {
	if winner.priority != loser.priority {
		return "higher match specificity — the winning rule's most specific match ranks the whole rule above this one"
	}

	// Cross-kind ties are decided by the flattening order: gRPC rules are
	// appended after HTTP, so an HTTPRoute always holds the lower flatIdx (and
	// wins) regardless of timestamp or name. Check this BEFORE timestamp/name
	// so a coincidentally-older HTTP winner is not credited to its timestamp,
	// which did not decide the win.
	if winner.provenance.Kind == kindHTTPRoute && loser.provenance.Kind == kindGRPCRoute {
		return "HTTPRoute rules precede GRPCRoute rules in the generated configuration"
	}

	// Same-kind ties: the converter flattens in spec-precedence order, so a
	// lower flatIdx means an older creationTimestamp, then alphabetical name.
	if winner.provenance.CreationTimestamp.Before(&loser.provenance.CreationTimestamp) {
		return "older creationTimestamp"
	}

	if winner.provenance.CreationTimestamp.Equal(&loser.provenance.CreationTimestamp) {
		winnerKey := winner.provenance.Namespace + "/" + winner.provenance.Name
		loserKey := loser.provenance.Namespace + "/" + loser.provenance.Name

		if winnerKey < loserKey {
			return "alphabetical {namespace}/{name} precedence"
		}
	}

	// The winner sorts later by timestamp/name yet still serves first: it holds
	// the lower flattened index with no timestamp/name/kind reason that applied.
	return "earlier position in the generated configuration order"
}

// shadowedMessage builds the actionable operator-facing message: which pair is
// shadowed, who wins, why, and that the route stays Accepted.
func shadowedMessage(loser *shadowClaimant, key shadowKey, winner *shadowClaimant) string {
	hostname := key.hostname
	if hostname == "" {
		hostname = "<any>"
	}

	return fmt.Sprintf(
		"rule %d match (host %q, match %s) is shadowed by %s (%s); matching requests are served by that route. "+
			"This route remains Accepted. Resolve by removing the duplicate match or scoping hostnames per tenant.",
		loser.provenance.RuleIndex, hostname, key.matchKey, winner.provenance.String(), shadowBasis(winner, loser),
	)
}
