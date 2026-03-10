package proxy

import (
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/cockroachdb/errors"
)

// Priority scoring constants for Gateway API rule precedence.
const (
	priorityExactPath     = 10000
	priorityPrefixPath    = 5000
	priorityRegexPath     = 1000
	priorityPathLength    = 10
	priorityMethod        = 100
	priorityPerHeader     = 50
	priorityPerQueryParam = 25
)

// compiledRule is a pre-compiled routing rule ready for request matching.
type compiledRule struct {
	matches  []*CompiledMatch
	rule     *RouteRule
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

// Router provides thread-safe HTTP request routing with atomic config updates.
type Router struct {
	table atomic.Pointer[routingTable]
}

// NewRouter creates a Router with an empty routing table.
func NewRouter() *Router {
	router := &Router{}
	router.table.Store(&routingTable{
		exactHosts: make(map[string][]*compiledRule),
	})

	return router
}

// ConfigVersion returns the version of the currently loaded configuration.
func (r *Router) ConfigVersion() int64 {
	return r.table.Load().version
}

// Route finds the best matching rule for the request and selects a backend.
// Returns the matched RouteRule and backend index, or (nil, -1) if no match.
func (r *Router) Route(req *http.Request) (*RouteRule, int) {
	table := r.table.Load()
	host := extractHost(req)

	// Try exact host match first.
	if rules, ok := table.exactHosts[host]; ok {
		if rule, idx := matchRules(rules, req); rule != nil {
			return rule, idx
		}
	}

	// Try wildcard host matches (longest suffix first).
	for _, wildcard := range table.wildcardHosts {
		if matchesWildcard(host, wildcard.suffix) {
			if rule, idx := matchRules(wildcard.rules, req); rule != nil {
				return rule, idx
			}
		}
	}

	// Try default (no hostname) rules.
	if rule, idx := matchRules(table.defaultRules, req); rule != nil {
		return rule, idx
	}

	return nil, -1
}

// UpdateConfig compiles a new routing table from the config and atomically swaps it in.
func (r *Router) UpdateConfig(cfg *Config) error {
	table, err := compileRoutingTable(cfg)
	if err != nil {
		return errors.Wrap(err, "failed to compile routing table")
	}

	r.table.Store(table)

	return nil
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
			if strings.HasPrefix(hostname, "*.") {
				suffix := hostname[1:] // e.g., "*.example.com" → ".example.com"
				wildcardMap[suffix] = append(wildcardMap[suffix], compiled)
			} else {
				table.exactHosts[hostname] = append(table.exactHosts[hostname], compiled)
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

	return &compiledRule{
		matches:  matches,
		rule:     rule,
		priority: computePriority(rule, ruleIndex),
	}, nil
}

// computePriority calculates a precedence score for Gateway API ordering.
// Higher score = higher precedence. Rules are sorted descending by priority.
//
// Gateway API precedence (from spec):
//
//  1. Longest hostname (handled by exact > wildcard > default lookup order)
//  2. Exact path > Prefix path > Regex path
//  3. Longest path value
//  4. Method match present
//  5. Most header matches
//  6. Most query param matches
func computePriority(rule *RouteRule, ruleIndex int) int {
	priority := 0

	for _, match := range rule.Matches {
		matchScore := 0

		if match.Path != nil {
			switch match.Path.Type {
			case PathMatchExact:
				matchScore += priorityExactPath
			case PathMatchPathPrefix:
				matchScore += priorityPrefixPath
			case PathMatchRegularExpression:
				matchScore += priorityRegexPath
			}

			matchScore += len(match.Path.Value) * priorityPathLength
		}

		if match.Method != "" {
			matchScore += priorityMethod
		}

		matchScore += len(match.Headers) * priorityPerHeader
		matchScore += len(match.QueryParams) * priorityPerQueryParam

		if matchScore > priority {
			priority = matchScore
		}
	}

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
func matchRules(rules []*compiledRule, req *http.Request) (*RouteRule, int) {
	for _, compiled := range rules {
		if matchesRule(compiled, req) {
			return compiled.rule, selectBackend(compiled.rule.Backends)
		}
	}

	return nil, -1
}

// matchesRule checks if a request matches any of the rule's compiled match conditions (OR logic).
// A rule with no match conditions accepts everything.
func matchesRule(compiled *compiledRule, req *http.Request) bool {
	if len(compiled.matches) == 0 {
		return true
	}

	for _, match := range compiled.matches {
		if match.Match(req) {
			return true
		}
	}

	return false
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

	totalWeight := int32(0)

	for _, backend := range backends {
		totalWeight += backend.Weight
	}

	if totalWeight <= 0 {
		//nolint:gosec // not security-sensitive, routing randomization only
		return rand.IntN(len(backends))
	}

	//nolint:gosec // not security-sensitive, routing randomization only
	pick := rand.Int32N(totalWeight)
	cumulative := int32(0)

	for idx, backend := range backends {
		cumulative += backend.Weight

		if pick < cumulative {
			return idx
		}
	}

	return len(backends) - 1
}

// extractHost strips the port from the Host header if present.
func extractHost(req *http.Request) string {
	host := req.Host

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
