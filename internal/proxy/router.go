package proxy

import (
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/errors"
)

// Priority scoring constants for Gateway API rule precedence (GEP-1722).
// Strict tier ordering: path type > path length > method > headers > queries.
//
// Each tier's per-unit value strictly dominates the total possible contribution
// of all lower tiers combined. This ensures that 1 extra character of path
// length always outranks any combination of method, header, and query matches.
const (
	priorityExactPath     = 1_000_000_000
	priorityRegexPath     = 500_000_000
	priorityPrefixPath    = 100_000_000
	priorityPathLength    = 10_000
	priorityMethod        = 100
	priorityPerHeader     = 10
	priorityPerQueryParam = 1
)

// compiledRule is a pre-compiled routing rule ready for request matching.
type compiledRule struct {
	matches  []*CompiledMatch
	rule     *RouteRule
	filters  []Filter
	priority int
}

// routingTable holds the compiled routing state for lock-free reads.
type routingTable struct {
	exactHosts    map[string][]*compiledRule
	wildcardHosts []wildcardEntry
	defaultRules  []*compiledRule
	version       int64
}

// wildcardEntry maps a wildcard suffix to its compiled rules.
type wildcardEntry struct {
	suffix string
	rules  []*compiledRule
}

// transportPruner is implemented by Handler to prune stale transport entries.
type transportPruner interface {
	PruneTransports(activeHosts map[string]bool)
}

// Router provides thread-safe HTTP request routing with atomic config updates.
type Router struct {
	table    atomic.Pointer[routingTable]
	updateMu sync.Mutex
	pruner   transportPruner
}

// NewRouter creates a Router with an empty routing table.
func NewRouter() *Router {
	router := &Router{}
	router.table.Store(&routingTable{
		exactHosts: make(map[string][]*compiledRule),
	})

	return router
}

// SetHandler registers a handler whose transport pool will be pruned on config updates.
func (r *Router) SetHandler(h *Handler) {
	r.pruner = h
}

// ConfigVersion returns the version of the currently loaded configuration.
func (r *Router) ConfigVersion() int64 {
	return r.table.Load().version
}

// RouteResult contains the result of a routing decision.
type RouteResult struct {
	Rule          *RouteRule
	Filters       []Filter
	BackendIdx    int
	MatchedPrefix string
}

// Route finds the best matching rule for the request and selects a backend.
// Returns nil if no match.
func (r *Router) Route(req *http.Request) *RouteResult {
	table := r.table.Load()
	host := extractHost(req)

	// Try exact host match first.
	if rules, ok := table.exactHosts[host]; ok {
		if result := matchRules(rules, req); result != nil {
			return result
		}
	}

	// Try wildcard host matches (longest suffix first).
	for _, wildcard := range table.wildcardHosts {
		if matchesWildcard(host, wildcard.suffix) {
			if result := matchRules(wildcard.rules, req); result != nil {
				return result
			}
		}
	}

	// Try default (no hostname) rules.
	if result := matchRules(table.defaultRules, req); result != nil {
		return result
	}

	return nil
}

// UpdateConfig compiles a new routing table from the config and atomically swaps it in.
// Rejects configs with a version older than the current one to prevent out-of-order updates.
// Thread-safe: concurrent calls are serialized internally.
func (r *Router) UpdateConfig(cfg *Config) error {
	r.updateMu.Lock()
	defer r.updateMu.Unlock()

	current := r.table.Load()

	if cfg.Version == 0 && current != nil && current.version > 0 {
		slog.Warn("applying unversioned config (version 0) over versioned config",
			"current_version", current.version)
	}

	if current != nil && cfg.Version > 0 && cfg.Version < current.version {
		return errors.Wrapf(errStaleVersion, "version %d < current %d", cfg.Version, current.version)
	}

	table, err := compileRoutingTable(cfg)
	if err != nil {
		return errors.Wrap(err, "failed to compile routing table")
	}

	r.table.Store(table)

	if r.pruner != nil {
		r.pruner.PruneTransports(extractActiveHosts(cfg))
	}

	return nil
}

// extractActiveHosts collects all backend hosts from the config's rules.
func extractActiveHosts(cfg *Config) map[string]bool {
	hosts := make(map[string]bool)

	for _, rule := range cfg.Rules {
		for _, backend := range rule.Backends {
			parsed, err := url.Parse(backend.URL)
			if err != nil {
				continue
			}

			hosts[parsed.Host] = true
		}
	}

	return hosts
}

// compileRoutingTable builds a routingTable from a Config.
func compileRoutingTable(cfg *Config) (*routingTable, error) {
	table := &routingTable{
		exactHosts: make(map[string][]*compiledRule),
		version:    cfg.Version,
	}

	wildcardMap := make(map[string][]*compiledRule)

	for ruleIdx := range cfg.Rules {
		rule := &cfg.Rules[ruleIdx]

		compiled, err := compileRule(rule, ruleIdx)
		if err != nil {
			return nil, errors.Wrapf(err, "rule[%d]", ruleIdx)
		}

		if len(rule.Hostnames) == 0 {
			table.defaultRules = append(table.defaultRules, compiled)

			continue
		}

		for _, hostname := range rule.Hostnames {
			normalized := strings.ToLower(hostname)
			if strings.HasPrefix(normalized, "*.") {
				suffix := normalized[1:] // e.g., "*.example.com" → ".example.com"
				wildcardMap[suffix] = append(wildcardMap[suffix], compiled)
			} else {
				table.exactHosts[normalized] = append(table.exactHosts[normalized], compiled)
			}
		}
	}

	// Convert wildcard map to sorted slice (longest suffix first for precedence).
	for suffix, rules := range wildcardMap {
		sortRulesByPrecedence(rules)

		table.wildcardHosts = append(table.wildcardHosts, wildcardEntry{
			suffix: suffix,
			rules:  rules,
		})
	}

	sort.Slice(table.wildcardHosts, func(i, j int) bool {
		return len(table.wildcardHosts[i].suffix) > len(table.wildcardHosts[j].suffix)
	})

	// Sort exact host rules and default rules by precedence.
	for host := range table.exactHosts {
		sortRulesByPrecedence(table.exactHosts[host])
	}

	sortRulesByPrecedence(table.defaultRules)

	return table, nil
}

// compileRule compiles a single RouteRule into a compiledRule.
func compileRule(rule *RouteRule, ruleIndex int) (*compiledRule, error) {
	var matches []*CompiledMatch

	for matchIdx, match := range rule.Matches {
		compiled, err := CompileMatch(match)
		if err != nil {
			return nil, errors.Wrapf(err, "match[%d]", matchIdx)
		}

		matches = append(matches, compiled)
	}

	filters, err := CompileFilters(rule.Filters)
	if err != nil {
		return nil, errors.Wrap(err, "compile filters")
	}

	return &compiledRule{
		matches:  matches,
		rule:     rule,
		filters:  filters,
		priority: computePriority(rule, ruleIndex),
	}, nil
}

// computePriority calculates a precedence score for Gateway API ordering.
// Higher score = higher precedence. Rules are sorted descending by priority.
//
// Gateway API precedence (from spec, GEP-1722):
//
//  1. Longest hostname (handled by exact > wildcard > default lookup order)
//  2. Exact path > Prefix path > Regex path
//  3. Longest path value
//  4. Method match present
//  5. Most header matches
//  6. Most query param matches
//
// Each component is maximized independently across all ORed matches in the rule.
// This is correct because a rule with matches [PathPrefix /v2, header:x]
// should rank higher on path than a rule with [PathPrefix /, header:y],
// regardless of which match carries the header.
func computePriority(rule *RouteRule, ruleIndex int) int {
	maxPathTypeScore := 0
	maxPathLen := 0
	hasMethod := false
	maxHeaders := 0
	maxQueryParams := 0

	for _, match := range rule.Matches {
		if match.Path != nil {
			pathTypeScore := 0

			switch match.Path.Type {
			case PathMatchExact:
				pathTypeScore = priorityExactPath
			case PathMatchPathPrefix:
				pathTypeScore = priorityPrefixPath
			case PathMatchRegularExpression:
				pathTypeScore = priorityRegexPath
			}

			if pathTypeScore > maxPathTypeScore {
				maxPathTypeScore = pathTypeScore
				maxPathLen = len(match.Path.Value)
			} else if pathTypeScore == maxPathTypeScore && len(match.Path.Value) > maxPathLen {
				maxPathLen = len(match.Path.Value)
			}
		}

		if match.Method != "" {
			hasMethod = true
		}

		if len(match.Headers) > maxHeaders {
			maxHeaders = len(match.Headers)
		}

		if len(match.QueryParams) > maxQueryParams {
			maxQueryParams = len(match.QueryParams)
		}
	}

	priority := maxPathTypeScore + maxPathLen*priorityPathLength

	if hasMethod {
		priority += priorityMethod
	}

	priority += maxHeaders * priorityPerHeader
	priority += maxQueryParams * priorityPerQueryParam

	// Use negative rule index as tiebreaker (earlier rules win).
	priority -= ruleIndex

	return priority
}

// sortRulesByPrecedence sorts rules in descending priority order.
func sortRulesByPrecedence(rules []*compiledRule) {
	sort.Slice(rules, func(i, j int) bool {
		return rules[i].priority > rules[j].priority
	})
}

// matchRules iterates through sorted rules and returns the first match.
// Multiple matches within a rule are ORed.
func matchRules(rules []*compiledRule, req *http.Request) *RouteResult {
	for _, compiled := range rules {
		matchIdx := findMatchingIndex(compiled, req)
		if matchIdx < 0 {
			continue
		}

		return &RouteResult{
			Rule:          compiled.rule,
			Filters:       compiled.filters,
			BackendIdx:    selectBackend(compiled.rule.Backends),
			MatchedPrefix: getMatchedPathPrefix(compiled.rule, matchIdx),
		}
	}

	return nil
}

// getMatchedPathPrefix returns the path prefix from the match that actually fired.
// Falls back to the first prefix match if matchIdx is out of range.
func getMatchedPathPrefix(rule *RouteRule, matchIdx int) string {
	if matchIdx >= 0 && matchIdx < len(rule.Matches) {
		match := rule.Matches[matchIdx]
		if match.Path != nil && match.Path.Type == PathMatchPathPrefix {
			return match.Path.Value
		}
	}

	return ""
}

// findMatchingIndex returns the index of the first compiled match that matches the request,
// or -1 if no match is found. A rule with no match conditions returns 0 (matches everything).
func findMatchingIndex(compiled *compiledRule, req *http.Request) int {
	if len(compiled.matches) == 0 {
		return 0
	}

	for idx, match := range compiled.matches {
		if match.Match(req) {
			return idx
		}
	}

	return -1
}

// selectBackend picks a backend using weighted random selection.
// Returns -1 when the backends slice is empty (e.g., redirect-only rules).
func selectBackend(backends []BackendRef) int {
	if len(backends) == 0 {
		return -1
	}

	if len(backends) == 1 {
		return 0
	}

	// Use int64 to avoid overflow when multiple backends have large weights.
	totalWeight := int64(0)

	for _, backend := range backends {
		totalWeight += int64(backend.Weight)
	}

	if totalWeight <= 0 {
		// All backends have zero weight — per Gateway API spec, no traffic should be sent.
		return -1
	}

	//nolint:gosec // not security-sensitive, routing randomization only
	pick := rand.Int64N(totalWeight)
	cumulative := int64(0)

	for idx, backend := range backends {
		cumulative += int64(backend.Weight)

		if pick < cumulative {
			return idx
		}
	}

	return len(backends) - 1
}

// extractHost returns the hostname to use for route matching.
// Prefers X-Original-Host header (set by TunnelRoundTripper when the real Host
// must be replaced with the edge hostname to pass through Cloudflare edge).
// Falls back to the standard Host header. Port suffix is stripped.
// NOTE: Go's http.Request.Host always wraps IPv6 addresses in brackets
// (e.g., "[::1]:8080"), so bare IPv6 like "::1" should not appear in practice.
func extractHost(req *http.Request) string {
	host := req.Header.Get("X-Original-Host")
	if host == "" {
		host = req.Host
	}

	if idx := strings.LastIndex(host, ":"); idx != -1 {
		// Ensure we don't strip part of an IPv6 address.
		if !strings.Contains(host[idx:], "]") {
			host = host[:idx]
		}
	}

	return strings.ToLower(host)
}

// matchesWildcard checks if a hostname matches a wildcard suffix.
// e.g., "app.example.com" matches ".example.com" (from "*.example.com").
func matchesWildcard(host, suffix string) bool {
	if !strings.HasSuffix(host, suffix) {
		return false
	}

	// The part before the suffix must be a single label (no dots).
	prefix := host[:len(host)-len(suffix)]

	return prefix != "" && !strings.Contains(prefix, ".")
}
