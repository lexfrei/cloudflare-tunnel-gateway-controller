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
// the data the router actually orders by: the match's rank (rankMatch, the
// router's own function) and the flattened index (the router's tiebreak).
type shadowClaimant struct {
	provenance RuleProvenance
	rank       matchRank
	flatIdx    int
}

// beats reports whether c is served BEFORE other by the router: higher
// rank first, then lower flattened index (sortByPrecedence).
func (c *shadowClaimant) beats(other *shadowClaimant) bool {
	if order := c.rank.compare(other.rank); order != 0 {
		return order > 0
	}

	return c.flatIdx < other.flatIdx
}

// DetectShadowedRules flags every (hostname, match) pair that is EXACTLY
// claimed by two routes, attributing winner and loser by the ROUTER's actual
// ordering: match rank descending, then flattened index. Equal pairs differ in
// rank only through RouteMatch.GRPCMethod, which the pair identity leaves out:
// a GRPCRoute match against an HTTPRoute match on the same path, or two
// GRPCRoute regex matches generating the same path. Exact pair equality is
// the zero-traffic case: the winning rule matches every request the losing
// pair would, and the router serves the winner first. Cross-bucket overlaps
// (wildcard vs exact hostname, prefix vs exact path) are deliberately NOT
// flagged — the other rule still serves traffic there.
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
	owners := compileListenerOwners(cfg.ListenerHostnames)

	var claims []shadowClaim

	for ruleIdx := range cfg.Rules {
		rule := &cfg.Rules[ruleIdx]
		isolation := &compiledRule{listeners: compileRuleListeners(rule.Listeners)}

		for _, ranked := range ruleShadowKeys(rule) {
			key := ranked.key
			if !isolation.isolationAllows(&hostOwners{owners: owners, host: representativeHost(key.hostname)}) {
				continue
			}

			claimant := shadowClaimant{provenance: cfg.Provenance[ruleIdx], rank: ranked.rank, flatIdx: ruleIdx}
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
			Kind:      claim.claimant.provenance.Kind,
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

type rankedShadowKey struct {
	key  shadowKey
	rank matchRank
}

// ruleShadowKeys expands a rule into its claimed (hostname, match) keys. A
// rule with no hostnames claims the default bucket (""); a rule with no
// matches claims the matches-everything key — both mirror the router's
// behaviour exactly.
func ruleShadowKeys(rule *RouteRule) []rankedShadowKey {
	hostnames := rule.Hostnames
	if len(hostnames) == 0 {
		hostnames = []string{""}
	}

	type rankedMatch struct {
		key  string
		rank matchRank
	}

	matches := make([]rankedMatch, 0, max(len(rule.Matches), 1))
	if len(rule.Matches) == 0 {
		matches = append(matches, rankedMatch{key: "catch-all"})
	}

	for idx := range rule.Matches {
		matches = append(matches, rankedMatch{key: canonicalMatchKey(&rule.Matches[idx]), rank: rankMatch(&rule.Matches[idx])})
	}

	keys := make([]rankedShadowKey, 0, len(hostnames)*len(matches))

	for _, hostname := range hostnames {
		for _, match := range matches {
			keys = append(keys, rankedShadowKey{
				key:  shadowKey{hostname: strings.ToLower(hostname), matchKey: match.key},
				rank: match.rank,
			})
		}
	}

	return keys
}

// representativeHost stands for the hosts a hostname key covers that no
// listener names more specifically than the key itself: the key for an exact
// hostname, an unnameable label under a wildcard, and an unnameable host for
// the default bucket. Listener isolation removes a rule's claim on the key
// when the listener owning this host is not one the rule is attached through.
func representativeHost(hostname string) string {
	const unnameableLabel = "\x01"

	switch {
	case hostname == "":
		return unnameableLabel
	case strings.HasPrefix(hostname, "*."):
		return unnameableLabel + hostname[1:]
	default:
		return hostname
	}
}

// canonicalMatchKey serializes a RouteMatch into a stable, order-insensitive
// identity: header and query-param lists are sorted (names lowercased — HTTP
// header names are case-insensitive), so two spec-equal matches written in
// different order collide as they should.
func canonicalMatchKey(match *RouteMatch) string {
	// Full struct copy, then overwrite ONLY the normalized slices: an
	// explicit field-by-field copy would silently exclude any future
	// RouteMatch field from the identity, making matches that differ only in
	// that field falsely collide.
	norm := *match
	norm.Headers = normalizeHeaderMatches(match.Headers)
	norm.QueryParams = normalizeQueryMatches(match.QueryParams)
	// Ranking data only: Path carries the condition, and dropping it keeps an
	// HTTPRoute match and a GRPCRoute match on the same path colliding.
	norm.GRPCMethod = nil

	encoded, err := json.Marshal(norm)
	if err != nil {
		// RouteMatch is plain data; Marshal cannot realistically fail. The
		// fallback must still key by CONTENT so equal matches collide.
		return unmarshalableMatchKey(&norm)
	}

	return string(encoded)
}

// unmarshalableMatchKey is the content-stable fallback for canonicalMatchKey
// when json.Marshal fails. RouteMatch.Path is a pointer, so a bare %#v would
// render its ADDRESS — making two spec-identical matches produce different keys
// and silently miss the shadow collision. Dereference Path explicitly (the
// only pointer field canonicalMatchKey leaves set) and drop the pointer from
// the struct dump so the rest of the fields (slices, strings) contribute
// their content.
func unmarshalableMatchKey(match *RouteMatch) string {
	norm := *match

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

// shadowBasis names the criterion that decided the collision. A rank gap
// means the winning match outranks the loser on Gateway API match
// specificity. Equal ranks are decided by beats() PURELY on flattened index,
// so a timestamp/name reason is reported only when it actually agrees with
// that order; otherwise the honest answer is the generated-config order
// itself (e.g. HTTPRoute rules precede GRPCRoute rules) — never a timestamp
// or name that did not decide it.
func shadowBasis(winner, loser *shadowClaimant) string {
	if winner.rank != loser.rank {
		return "higher match specificity — the winning match ranks above this one"
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
		// Compare namespace then name as SEPARATE fields, matching
		// sortRoutesByPrecedence (which decides the real flatIdx). Concatenating
		// "namespace/name" and comparing the joined strings diverges when a
		// namespace contains '-' or a name contains '.' — both sort below the
		// '/' separator, so the joined compare can disagree with the field-wise
		// order the winner was actually chosen by.
		wns, lns := winner.provenance.Namespace, loser.provenance.Namespace
		if wns < lns || (wns == lns && winner.provenance.Name < loser.provenance.Name) {
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
