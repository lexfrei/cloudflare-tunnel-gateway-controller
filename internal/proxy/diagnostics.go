package proxy

// DiagnosticTarget identifies which status surface a RouteDiagnostic maps to.
// The proxy converter classifies each silently-unsupported/dropped config path
// per the Gateway API spec; the controller turns the classification into the
// matching condition (or a Kubernetes Event for benign overrides).
type DiagnosticTarget string

const (
	// DiagnosticAccepted means a rule cannot be served as written (e.g. it
	// carries an unsupported filter). The controller aggregates these per route:
	// when every rule of the route is affected it sets Accepted=False, otherwise
	// it sets PartiallyInvalid=True with a "Dropped Rule" message.
	DiagnosticAccepted DiagnosticTarget = "Accepted"
	// DiagnosticResolvedRefs means a backend/object reference could not be
	// resolved or declares an app protocol this implementation does not support.
	// The controller sets ResolvedRefs=False.
	DiagnosticResolvedRefs DiagnosticTarget = "ResolvedRefs"
	// DiagnosticShadowed means a rule's (hostname, match) pair is exactly
	// claimed by a higher-precedence rule from another route, so it receives
	// zero traffic. Status/observability only: the route stays Accepted (the
	// Gateway API treats same-hostname routes as legal merging); the controller
	// surfaces a dedicated condition plus a Warning Event on the losing route.
	DiagnosticShadowed DiagnosticTarget = "Shadowed"
	// DiagnosticProxyConfigPush means the controller could not push this route's
	// generated config to its data plane (a SUSTAINED proxy push failure, not a
	// one-off blip). The route stays Accepted — its spec is valid and the tunnel
	// document was written — but the in-cluster proxy never received the config,
	// so requests 502 until the push recovers. The controller surfaces a
	// dedicated condition plus a Warning Event on the affected route.
	DiagnosticProxyConfigPush DiagnosticTarget = "ProxyConfigPush"
	// DiagnosticTunnelShared means this route's per-Gateway data plane shares one
	// Cloudflare Tunnel with another tenant's Gateway in a different namespace
	// (the connector tokens parse to the same tunnel). Sharing a tunnel is a
	// supported configuration but it is NOT isolation — the edge load-balances
	// the tunnel's requests across both planes. Status/observability only: the
	// route stays Accepted; the controller surfaces a dedicated condition plus a
	// Warning Event so the collapsed isolation is visible, not just logged.
	DiagnosticTunnelShared DiagnosticTarget = "TunnelShared"
	// DiagnosticEvent means the config was applied successfully but a redundant
	// or conflicting hint was overridden (a benign override, e.g. an appProtocol
	// cleartext hint superseded by a BackendTLSPolicy, or a ResponseHeaderModifier
	// that strips a WebSocket handshake header). No standard Gateway API condition
	// fits, so the controller emits a Kubernetes Event instead of a condition.
	DiagnosticEvent DiagnosticTarget = "Event"
)

// Kubernetes Event types for a RouteDiagnostic whose Target is DiagnosticEvent.
const (
	// EventTypeNormal marks a benign override the proxy handled correctly (e.g.
	// "TLS wins" over a cleartext appProtocol hint).
	EventTypeNormal = "Normal"
	// EventTypeWarning marks an operator-authored conflict honored as written but
	// with a consequence worth flagging (e.g. stripping a WS handshake header).
	EventTypeWarning = "Warning"
)

// RouteDiagnostic records a piece of a route's spec the controller will not
// serve exactly as written. It is produced by the proxy converter and consumed
// by the controller's status writer. It never crosses the proxy wire (Config
// carries the slice with a json:"-" tag), so it stays a controller-side concern.
//
// Message must be explicit and actionable: it names the problem AND the fix in
// plain words the operator can act on without reading the controller source.
type RouteDiagnostic struct {
	Namespace string
	Name      string
	RuleIndex int
	Target    DiagnosticTarget
	Reason    string // gateway-api RouteConditionReason, e.g. "UnsupportedValue"
	Message   string
	// WholeRule is true when the entire rule is unservable (e.g. a rule-level
	// unsupported filter makes every matched request fail closed), false when
	// only a backend traffic fraction is affected (e.g. a backend-level
	// unsupported filter). The status writer uses it to decide Accepted=False
	// (every rule wholly unservable) versus PartiallyInvalid=True (some rules or
	// backends degraded while the route still serves).
	WholeRule bool
	// EventType is "Normal" or "Warning"; only meaningful when Target is
	// DiagnosticEvent. The controller passes it to EventRecorder.Event.
	EventType string
}

// diagSink accumulates RouteDiagnostics during a single conversion pass. It is
// stamped with the current route identity and rule index so the deep tree of
// leaf converter functions can emit diagnostics without threading identity and
// rule index through every signature.
//
// It is nil-safe: every method is a no-op on a nil receiver, so converter
// helpers can be reached from call paths that do not collect diagnostics.
type diagSink struct {
	namespace string
	name      string
	rule      int
	items     []RouteDiagnostic
}

// route sets the identity stamped onto subsequently-added diagnostics.
func (s *diagSink) route(namespace, name string) {
	if s == nil {
		return
	}

	s.namespace = namespace
	s.name = name
}

// at sets the rule index stamped onto subsequently-added diagnostics.
func (s *diagSink) at(rule int) {
	if s == nil {
		return
	}

	s.rule = rule
}

// add records a condition-bound diagnostic. wholeRule is true when the entire
// rule is unservable (rule-level cause), false when only a backend fraction is
// affected (backend-level cause).
func (s *diagSink) add(target DiagnosticTarget, reason, message string, wholeRule bool) {
	if s == nil {
		return
	}

	s.items = append(s.items, RouteDiagnostic{
		Namespace: s.namespace,
		Name:      s.name,
		RuleIndex: s.rule,
		Target:    target,
		Reason:    reason,
		Message:   message,
		WholeRule: wholeRule,
	})
}

// event records a benign-override diagnostic surfaced as a Kubernetes Event.
// eventType is EventTypeNormal or EventTypeWarning.
func (s *diagSink) event(eventType, message string) {
	if s == nil {
		return
	}

	s.items = append(s.items, RouteDiagnostic{
		Namespace: s.namespace,
		Name:      s.name,
		RuleIndex: s.rule,
		Target:    DiagnosticEvent,
		Message:   message,
		EventType: eventType,
	})
}
