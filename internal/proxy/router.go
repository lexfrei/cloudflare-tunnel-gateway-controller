package proxy

import (
	"cmp"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/errors"
)

// compiledRule is a pre-compiled routing rule ready for request matching.
type compiledRule struct {
	matches        []*CompiledMatch
	rule           *RouteRule
	filters        []Filter
	backendFilters [][]Filter // per-backend compiled filters (indexed by backend position)
	ruleIndex      int        // original rule index for tiebreaking (earlier rules win)
	listeners      map[string]map[string]struct{}
	// closedErr is the first filter compile error that made failClosed answer
	// HTTP 500 for all or part of the rule; nil when every filter compiled.
	closedErr error
}

// matchEntry is one of a rule's ORed matches, ranked on its own because the
// spec orders matches, not rules. A rule without matches is a single entry
// whose nil matcher matches every request.
type matchEntry struct {
	compiled *compiledRule
	matcher  *CompiledMatch
	matchIdx int
	rank     matchRank
}

// routingTable holds the compiled routing state for lock-free reads.
type routingTable struct {
	exactHosts    map[string][]matchEntry
	wildcardHosts []wildcardEntry
	defaultRules  []matchEntry
	owners        listenerOwners
	version       int64
}

// wildcardEntry maps a wildcard suffix to its match entries.
type wildcardEntry struct {
	suffix string
	rules  []matchEntry
}

// transportPruner is implemented by Handler to prune stale transport entries.
// activeKeys are the composite host|protocol|tlsFingerprint|headerTimeout
// strings produced by transportKey, derived from the active config by
// extractActiveTransportKeys. Mirroring the exact key composition ensures a
// config edit in any of those four dimensions cleanly evicts the stale entry.
type transportPruner interface {
	PruneTransports(activeKeys map[string]bool)
}

// Router provides thread-safe HTTP request routing with atomic config updates.
type Router struct {
	table            atomic.Pointer[routingTable]
	updateMu         sync.Mutex
	pruner           transportPruner
	transportFactory TransportFactory
	// mirrorLimit and metrics are copied from the Handler by SetHandler and
	// handed to every mirror filter UpdateConfig compiles.
	mirrorLimit int64
	metrics     *Metrics
	// firstConfigCh delivers the first config applied via UpdateConfig exactly
	// once (guarded by firstConfigOnce). Buffered (size 1) so the send never
	// blocks even when nothing is waiting yet. A tunnel-mode proxy waits on it
	// before dialing the edge and picks the edge transport from what it carries.
	firstConfigCh   chan *Config
	firstConfigOnce sync.Once
	// dialedProtocol is the edge transport the proxy actually dialed, recorded
	// once at startup via SetDialedProtocol. UpdateConfig reads it to warn when a
	// GRPCRoute arrives after the proxy dialed a non-http2 transport.
	dialedProtocol atomic.Pointer[string]
	// grpcRestartWarned makes the restart-needed warning fire at most once
	// rather than on every config push.
	grpcRestartWarned atomic.Bool
	// tunnelConnected latches true once the edge transport has registered a
	// tunnel connection (tunnel mode) or immediately at startup (standalone
	// mode, no tunnel to wait for). Readiness gates on it so the proxy reports
	// Ready only when it can actually receive traffic — without it the pod goes
	// Ready while cloudflared is still dialing the edge and the edge returns 530.
	// One-shot: the cloudflared connected-signal fires once on first
	// registration and the proxy is the tunnel origin (traffic arrives via the
	// tunnel, not a Service), so a later drop is not tracked here — flapping
	// readiness would not change traffic delivery and would only churn rollouts.
	tunnelConnected atomic.Bool
}

// NewRouter creates a Router with an empty routing table.
func NewRouter() *Router {
	router := &Router{
		firstConfigCh: make(chan *Config, 1),
	}
	router.table.Store(&routingTable{
		exactHosts: make(map[string][]matchEntry),
	})

	return router
}

// SetDialedProtocol records the edge transport the proxy dialed at startup so
// UpdateConfig can warn when a GRPCRoute later arrives on a non-http2 transport.
// The proxy entrypoint calls it once after resolving the transport, before
// dialing the tunnel.
func (r *Router) SetDialedProtocol(protocol string) {
	r.dialedProtocol.Store(&protocol)
}

// FirstConfigLoaded returns a channel that delivers the first config applied
// via UpdateConfig, exactly once. A tunnel-mode proxy waits on it before
// dialing the edge, so it never registers without a routing table, and decides
// from it whether to upgrade an auto/unset edge transport to http2 — gRPC needs
// http2 because cloudflared drops HTTP trailers over QUIC.
func (r *Router) FirstConfigLoaded() <-chan *Config {
	return r.firstConfigCh
}

// SetHandler registers a handler whose transport pool will be pruned on
// config updates and whose per-cert transport factory is reused by mirror
// filters so the mirror leg dials TLS the same way the main leg does. Two
// concerns flow through one wiring point because both lifecycle-bind to the
// Handler: the pool is owned by the Handler, the factory is the Handler's
// closure over getTransport. It must be called before the first UpdateConfig:
// it writes fields UpdateConfig reads under updateMu without taking the lock.
func (r *Router) SetHandler(h *Handler) {
	r.pruner = h
	r.transportFactory = h.TransportFactory()
	r.mirrorLimit = h.mirrorMaxInFlight
	r.metrics = h.metrics
}

// ConfigVersion returns the version of the currently loaded configuration.
func (r *Router) ConfigVersion() int64 {
	return r.table.Load().version
}

// SetTunnelConnected latches the tunnel-connected state to true. Tunnel mode
// calls it from the cloudflared connected-signal hook on first edge
// registration; standalone mode calls it at startup (no tunnel to wait for).
// It only ever transitions false→true (see the tunnelConnected field comment).
func (r *Router) SetTunnelConnected() {
	r.tunnelConnected.Store(true)
}

// TunnelConnected reports whether the edge transport has registered a tunnel
// connection (or, in standalone mode, whether startup latched it).
func (r *Router) TunnelConnected() bool {
	return r.tunnelConnected.Load()
}

// IsReady reports whether the proxy can serve traffic: it has a config to route
// with AND the tunnel has connected to the edge. Readiness (GET /readyz) and the
// GET /config status both derive from this single definition so they never
// disagree.
func (r *Router) IsReady() bool {
	return r.ConfigVersion() > 0 && r.TunnelConnected()
}

// RouteResult contains the result of a routing decision.
type RouteResult struct {
	Rule           *RouteRule
	Filters        []Filter
	BackendFilters []Filter
	BackendIdx     int
	MatchedPrefix  string
	// MatchedHostname is the hostname PATTERN of the routing-table bucket the
	// request matched: the exact configured hostname, the "*.suffix" wildcard
	// pattern, or "" for default-bucket matches. Always config-bounded (never
	// the raw request Host), which is what makes it safe as a metric label
	// value — clients cannot mint new series by varying the Host header.
	MatchedHostname string
}

// Route finds the best matching rule for the request and selects a backend.
// Returns nil if no match.
func (r *Router) Route(req *http.Request) *RouteResult {
	table := r.table.Load()
	host := extractHost(req)
	hosts := &hostOwners{owners: table.owners, host: host}

	// Try exact host match first. The map key only matches when the request
	// host equals a configured hostname, so it is config-bounded.
	if rules, ok := table.exactHosts[host]; ok {
		if result := matchRules(rules, req, hosts); result != nil {
			result.MatchedHostname = host

			return result
		}
	}

	// Try wildcard host matches (longest suffix first).
	for _, wildcard := range table.wildcardHosts {
		if matchesWildcard(host, wildcard.suffix) {
			if result := matchRules(wildcard.rules, req, hosts); result != nil {
				result.MatchedHostname = "*" + wildcard.suffix

				return result
			}
		}
	}

	// Try default (no hostname) rules; MatchedHostname stays "".
	if result := matchRules(table.defaultRules, req, hosts); result != nil {
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

	// Wiring guard, not a runtime safety net: this check exists to fail
	// loud at construction time when SetHandler was never called for a
	// Router that compiles a TLS-bearing mirror filter. A future
	// maintainer reordering this function MUST keep the check below the
	// stale-version short-circuit but above compileRoutingTable so a
	// production-wired Router (which always calls SetHandler before any
	// UpdateConfig is reachable) never trips it.
	if r.transportFactory == nil && configHasTLSMirror(cfg) {
		return errTLSMirrorWithoutTransportFactory
	}

	table := compileRoutingTable(cfg, filterEnv{
		factory:     r.transportFactory,
		mirrorLimit: r.mirrorLimit,
		metrics:     r.metrics,
	})

	r.table.Store(table)

	if r.pruner != nil {
		r.pruner.PruneTransports(extractActiveTransportKeys(cfg))
	}

	// Signal the first successfully-applied config exactly once: a tunnel-mode
	// proxy dials the edge only after it, and learns from it whether a
	// GRPCRoute is present. The channel is buffered (size 1) so this send never
	// blocks while holding updateMu; sync.Once keeps later pushes from
	// re-signalling.
	r.firstConfigOnce.Do(func() {
		r.firstConfigCh <- cfg
	})

	r.warnGRPCRestartIfNeeded(cfg)

	return nil
}

// warnGRPCRestartIfNeeded logs an actionable error, at most once, when a
// GRPCRoute is now served but the proxy dialed a non-http2 transport at
// startup. A live re-dial is not safe (cloudflared registers its metrics on the
// global Prometheus registry and panics on a second orchestrator build), so the
// operator must restart the proxy; it then re-dials on http2 because the
// GRPCRoute is present in the first config push.
func (r *Router) warnGRPCRestartIfNeeded(cfg *Config) {
	dialed := r.dialedProtocol.Load()
	if dialed == nil || !GRPCRestartNeeded(*dialed, cfg.HasGRPCRoute) {
		return
	}

	if !r.grpcRestartWarned.CompareAndSwap(false, true) {
		return
	}

	slog.Error("a GRPCRoute is now configured but the tunnel was dialed with the "+*dialed+
		" transport, which cannot carry gRPC (cloudflared drops HTTP trailers over QUIC, so "+
		"grpc-status is lost). The proxy must be restarted to re-dial on http2, or set "+
		"proxy.tunnel.protocol=http2.",
		"dialedProtocol", *dialed)
}

// extractActiveTransportKeys collects all backend transport-pool keys from the
// config's rules. Keys are formed by transportKey(host, protocol, tls,
// headerTimeout) so PruneTransports can evict stale entries when any of those
// change (e.g. on a Service appProtocol flip, a BackendTLSPolicy swap, or a
// per-rule timeouts edit). The header timeout is derived from the rule
// the same way getTransport's callers derive it -- see ruleHeaderTimeout
// for the shared rule.
//
// RequestMirror filters are walked too: NewRequestMirror calls the
// TransportFactory with headerTimeout=0, parking a per-cert transport in
// Handler.transports during compileRule. Without including mirror keys
// here, every UpdateConfig would prune the mirror leg's transport and
// CloseIdleConnections it, forcing a fresh TLS handshake on every push
// for any TLS-only mirror destination — exactly the cost the per-cert
// pool exists to amortize.
func extractActiveTransportKeys(cfg *Config) map[string]bool {
	keys := make(map[string]bool)

	for idx := range cfg.Rules {
		rule := &cfg.Rules[idx]
		headerTimeout := ruleHeaderTimeout(rule)

		for _, backend := range rule.Backends {
			parsed, err := url.Parse(backend.URL)
			if err != nil {
				continue
			}

			keys[transportKey(parsed.Host, backend.Protocol, backend.TLS, headerTimeout)] = true

			collectMirrorTransportKeys(keys, backend.Filters)
		}

		collectMirrorTransportKeys(keys, rule.Filters)
	}

	return keys
}

// configHasTLSMirror reports whether any rule-level or per-backend
// RequestMirror filter in cfg carries a non-nil TLS config. Router uses
// this to fail UpdateConfig loudly when a TLS-aware mirror would
// otherwise fall back to the global cleartext mirrorClient — that
// fallback is fine for tests without a Handler but a production bypass
// hazard if SetHandler was never called.
func configHasTLSMirror(cfg *Config) bool {
	for idx := range cfg.Rules {
		if rulesHaveTLSMirror(cfg.Rules[idx].Filters) {
			return true
		}

		for _, backend := range cfg.Rules[idx].Backends {
			if rulesHaveTLSMirror(backend.Filters) {
				return true
			}
		}
	}

	return false
}

// rulesHaveTLSMirror reports whether any RequestMirror filter in the
// slice carries a non-nil TLS config.
func rulesHaveTLSMirror(filters []RouteFilter) bool {
	for filterIdx := range filters {
		filter := &filters[filterIdx]
		if filter.Type == FilterRequestMirror && filter.RequestMirror != nil && filter.RequestMirror.TLS != nil {
			return true
		}
	}

	return false
}

// collectMirrorTransportKeys adds the per-cert transport key for every
// RequestMirror filter into keys. Mirror filters dial with headerTimeout=0
// (NewRequestMirror's call to TransportFactory), so the key matches the
// pool entry the filter borrows at compile time.
//
// A parse failure on BackendURL is logged at Error level rather than
// silently skipped: the URL was emitted by convertMirrorFilter through
// buildServiceURL + forceHTTPSScheme so a parse failure here means
// upstream emitted a malformed URL. Silently dropping the key would
// hand PruneTransports an incomplete active set and evict a perfectly
// good per-cert transport the next time the filter is recompiled.
func collectMirrorTransportKeys(keys map[string]bool, filters []RouteFilter) {
	for filterIdx := range filters {
		filter := &filters[filterIdx]
		if filter.Type != FilterRequestMirror || filter.RequestMirror == nil {
			continue
		}

		parsed, err := url.Parse(filter.RequestMirror.BackendURL)
		if err != nil {
			slog.Error("mirror filter BackendURL failed to parse; per-cert transport key skipped (will be evicted on next prune)",
				"url", filter.RequestMirror.BackendURL,
				"error", err)

			continue
		}

		keys[transportKey(parsed.Host, filter.RequestMirror.Protocol, filter.RequestMirror.TLS, 0)] = true
	}
}

// indexRuleByHostname files a compiled rule's match entries under each
// hostname it serves: exact hostnames on the table, wildcards in wildcardMap
// keyed by the suffix they match, and a rule with no hostnames among the
// defaults.
func indexRuleByHostname(
	table *routingTable,
	wildcardMap map[string][]matchEntry,
	hostnames []string,
	compiled *compiledRule,
) {
	entries := compiled.entries()

	if len(hostnames) == 0 {
		table.defaultRules = append(table.defaultRules, entries...)

		return
	}

	for _, hostname := range hostnames {
		normalized := strings.ToLower(hostname)
		if strings.HasPrefix(normalized, "*.") {
			suffix := normalized[1:] // e.g., "*.example.com" → ".example.com"
			wildcardMap[suffix] = append(wildcardMap[suffix], entries...)
		} else {
			table.exactHosts[normalized] = append(table.exactHosts[normalized], entries...)
		}
	}
}

func (c *compiledRule) entries() []matchEntry {
	if len(c.matches) == 0 {
		return []matchEntry{{compiled: c}}
	}

	entries := make([]matchEntry, len(c.matches))
	for idx, matcher := range c.matches {
		entries[idx] = matchEntry{
			compiled: c,
			matcher:  matcher,
			matchIdx: idx,
			rank:     rankMatch(&c.rule.Matches[idx]),
		}
	}

	return entries
}

// compileRoutingTable builds a routingTable from a Config. env is forwarded
// down to compileRule → compileFilters → compileFilter for the RequestMirror
// filter: its transport factory lets the mirror borrow a per-cert
// RoundTripper from the Handler's shared pool when a BackendTLSPolicy targets
// the mirror destination.
func compileRoutingTable(cfg *Config, env filterEnv) *routingTable {
	table := &routingTable{
		exactHosts: make(map[string][]matchEntry),
		owners:     compileListenerOwners(cfg.ListenerHostnames),
		version:    cfg.Version,
	}

	wildcardMap := make(map[string][]matchEntry)

	report := compileReport{version: cfg.Version}

	for ruleIdx := range cfg.Rules {
		rule := &cfg.Rules[ruleIdx]

		compiled, err := compileRule(rule, ruleIdx, env)
		if err != nil {
			// Skip the rule, keep the document. Match patterns are tenant
			// authored and reach the proxy unchecked, so refusing the whole
			// config over one of them would hold every other tenant on this
			// data plane at its previous routing table until the offending
			// route is withdrawn.
			report.skip(ruleIdx, rule, err)

			continue
		}

		if compiled.closedErr != nil {
			report.failClosed(ruleIdx, rule, compiled.closedErr)
		}

		indexRuleByHostname(table, wildcardMap, rule.Hostnames, compiled)
	}

	// Convert wildcard map to sorted slice (longest suffix first for precedence).
	for suffix, rules := range wildcardMap {
		sortByPrecedence(rules)

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
		sortByPrecedence(table.exactHosts[host])
	}

	sortByPrecedence(table.defaultRules)

	report.summarize(len(cfg.Rules))

	return table
}

// compileReport logs what compileRoutingTable could not compile as written:
// one line per config for each kind, not per rule, because a rule that does
// not compile does not start compiling on the next push. The summary carries
// the counts.
type compileReport struct {
	version int64
	skipped int
	closed  int
}

func (r *compileReport) skip(ruleIdx int, rule *RouteRule, err error) {
	if r.skipped == 0 {
		slog.Error("skipping rule that failed to compile; its requests fall through to the next matching rule",
			"rule", ruleIdx,
			"hostnames", rule.Hostnames,
			"version", r.version,
			"error", err,
		)
	}

	r.skipped++
}

func (r *compileReport) failClosed(ruleIdx int, rule *RouteRule, err error) {
	if r.closed == 0 {
		slog.Error("serving HTTP 500 where a filter failed to compile; the proxy image may be older than the controller",
			"rule", ruleIdx,
			"hostnames", rule.Hostnames,
			"version", r.version,
			"error", err,
		)
	}

	r.closed++
}

func (r *compileReport) summarize(rules int) {
	if r.skipped > 0 {
		slog.Warn("routing table applied with rules missing",
			"skipped", r.skipped,
			"rules", rules,
			"version", r.version,
		)
	}

	if r.closed > 0 {
		slog.Warn("routing table applied with rules answering HTTP 500 where a filter failed to compile",
			"failedClosed", r.closed,
			"rules", rules,
			"version", r.version,
		)
	}
}

// compileRule compiles a single RouteRule into a compiledRule. env is
// forwarded to compileFilters for the mirror filter.
func compileRule(rule *RouteRule, ruleIndex int, env filterEnv) (*compiledRule, error) {
	var matches []*CompiledMatch

	for matchIdx := range rule.Matches {
		compiled, err := CompileMatch(&rule.Matches[matchIdx])
		if err != nil {
			return nil, errors.Wrapf(err, "match[%d]", matchIdx)
		}

		matches = append(matches, compiled)
	}

	var closedErr error

	filters, err := compileFilters(rule.Filters, env)
	if err != nil {
		closedErr = err
		rule = failClosed(rule, noBackend)
	}

	var backendFilters [][]Filter

	for backendIdx, backend := range rule.Backends {
		if len(backend.Filters) == 0 {
			backendFilters = append(backendFilters, nil)

			continue
		}

		compiledFilters, bfErr := compileFilters(backend.Filters, env)
		if bfErr != nil {
			if closedErr == nil {
				closedErr = errors.Wrapf(bfErr, "backend[%d]", backendIdx)
			}

			rule = failClosed(rule, backendIdx)
		}

		backendFilters = append(backendFilters, compiledFilters)
	}

	return &compiledRule{
		matches:        matches,
		rule:           rule,
		filters:        filters,
		backendFilters: backendFilters,
		ruleIndex:      ruleIndex,
		listeners:      compileRuleListeners(rule.Listeners),
		closedErr:      closedErr,
	}, nil
}

// noBackend is failClosed's backendIdx for a rule-level filter.
const noBackend = -1

// failClosed returns a copy of rule that answers HTTP 500 (gRPC: UNAVAILABLE)
// instead of serving without a filter this proxy could not compile: the whole
// rule for a rule-level filter, or only the backend carrying it (its share of
// the weighted pool) for a backend-level one. The Gateway API forbids skipping
// a filter an implementation cannot honour, so the rule is neither dropped,
// which would let its requests fall through to a less specific rule, nor served
// as if the filter were absent. The copy keeps cfg itself as pushed.
func failClosed(rule *RouteRule, backendIdx int) *RouteRule {
	closed := *rule

	if backendIdx == noBackend {
		closed.UnavailableStatus = http.StatusInternalServerError

		return &closed
	}

	closed.Backends = slices.Clone(rule.Backends)
	closed.Backends[backendIdx].UnavailableStatus = http.StatusInternalServerError

	return &closed
}

// matchRank orders matches by Gateway API precedence. Fields compare in
// declaration order and each decides only on a tie of every field before it,
// so no count can outweigh a higher criterion.
type matchRank struct {
	// GRPCRouteRule.Matches: characters in the service, then in the method.
	// Both are zero for an HTTPRoute match, so where the two kinds share a
	// host bucket a GRPCRoute match naming a service or method ranks first.
	grpcService int
	grpcMethod  int
	// HTTPRouteRule.Matches: Exact, then PathPrefix by length, with
	// RegularExpression (implementation-specific) placed between them.
	pathType    int
	pathLen     int
	method      int
	headers     int
	queryParams int
}

const (
	pathRankNone = iota
	pathRankPrefix
	pathRankRegex
	pathRankExact
)

func pathTypeRank(pathType PathMatchType) int {
	switch pathType {
	case PathMatchPathPrefix:
		return pathRankPrefix
	case PathMatchRegularExpression:
		return pathRankRegex
	case PathMatchExact:
		return pathRankExact
	default:
		return pathRankNone
	}
}

func rankMatch(match *RouteMatch) matchRank {
	rank := matchRank{headers: len(match.Headers), queryParams: len(match.QueryParams)}

	if match.Method != "" {
		rank.method = 1
	}

	switch {
	case match.GRPCMethod != nil:
		rank.grpcService = len(match.GRPCMethod.Service)
		rank.grpcMethod = len(match.GRPCMethod.Method)
	case match.Path != nil:
		rank.pathType = pathTypeRank(match.Path.Type)
		rank.pathLen = len(match.Path.Value)
	default:
		// A match without a path matches as the spec's default, PathPrefix "/".
		rank.pathType = pathRankPrefix
		rank.pathLen = len("/")
	}

	return rank
}

func (r matchRank) compare(other matchRank) int {
	return cmp.Or(
		cmp.Compare(r.grpcService, other.grpcService),
		cmp.Compare(r.grpcMethod, other.grpcMethod),
		cmp.Compare(r.pathType, other.pathType),
		cmp.Compare(r.pathLen, other.pathLen),
		cmp.Compare(r.method, other.method),
		cmp.Compare(r.headers, other.headers),
		cmp.Compare(r.queryParams, other.queryParams),
	)
}

// sortByPrecedence orders entries highest rank first. Equal ranks go to the
// lower flattened rule index, which the converter assigns oldest route first,
// then by {namespace}/{name}, then in rule order.
func sortByPrecedence(entries []matchEntry) {
	slices.SortFunc(entries, func(left, right matchEntry) int {
		return cmp.Or(
			right.rank.compare(left.rank),
			cmp.Compare(left.compiled.ruleIndex, right.compiled.ruleIndex),
			cmp.Compare(left.matchIdx, right.matchIdx),
		)
	})
}

// matchRules returns the first entry, in precedence order, that the request
// matches.
func matchRules(entries []matchEntry, req *http.Request, hosts *hostOwners) *RouteResult {
	for idx := range entries {
		entry := &entries[idx]
		compiled := entry.compiled

		if !compiled.isolationAllows(hosts) {
			continue
		}

		if entry.matcher != nil && !entry.matcher.Match(req) {
			continue
		}

		backendIdx := selectBackend(compiled.rule.Backends)

		var backendFilters []Filter
		if backendIdx >= 0 && backendIdx < len(compiled.backendFilters) {
			backendFilters = compiled.backendFilters[backendIdx]
		}

		return &RouteResult{
			Rule:           compiled.rule,
			Filters:        compiled.filters,
			BackendFilters: backendFilters,
			BackendIdx:     backendIdx,
			MatchedPrefix:  getMatchedPathPrefix(compiled.rule, entry.matchIdx),
		}
	}

	return nil
}

// getMatchedPathPrefix returns the path prefix from the match that actually
// fired, or "" when that match is not a PathPrefix. A rule without matches and
// a match without a path carry the spec's default, a PathPrefix of "/".
func getMatchedPathPrefix(rule *RouteRule, matchIdx int) string {
	if matchIdx < 0 || matchIdx >= len(rule.Matches) || rule.Matches[matchIdx].Path == nil {
		return "/"
	}

	if path := rule.Matches[matchIdx].Path; path.Type == PathMatchPathPrefix {
		return path.Value
	}

	return ""
}

// selectBackend picks a backend using weighted random selection.
// Returns -1 when the backends slice is empty (e.g., redirect-only rules).
func selectBackend(backends []BackendRef) int {
	if len(backends) == 0 {
		return -1
	}

	// No single-backend short-circuit: a lone backend with weight 0 must still
	// receive no traffic per the Gateway API spec, which the totalWeight<=0
	// guard below handles. A lone backend with weight > 0 falls through to the
	// cumulative walk and selects index 0 as expected.

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
// The header only ever reaches here in a deployment that opted in via
// WithAllowXOriginalHost; Handler.ServeHTTP strips it otherwise, so the
// preference below is inert in a default install.
// NOTE: Go's http.Request.Host always wraps IPv6 addresses in brackets
// (e.g., "[::1]:8080"), so bare IPv6 like "::1" should not appear in practice.
func extractHost(req *http.Request) string {
	host := req.Header.Get(originalHostHeader)
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
// suffix is the dot-prefixed remainder of a "*." pattern, e.g. ".example.com"
// from "*.example.com".
//
// Per the Gateway API Hostname spec, "*." matches one OR MORE leading labels:
// both "app.example.com" and "deep.app.example.com" match "*.example.com". The
// apex ("example.com") never matches — the dot-prefixed suffix plus the
// non-empty-prefix length check exclude it. This mirrors the permissive
// semantics in internal/routebinding so binding and runtime routing agree.
func matchesWildcard(host, suffix string) bool {
	return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
}
