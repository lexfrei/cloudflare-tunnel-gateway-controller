package proxy

import (
	"context"
	"net/http"
)

// Gateway self-probe handling: answer a config-convergence probe locally.
//
// Some ingress integrations verify that a route is live by dialing the gateway
// data plane directly and asking "have you converged on config version X?". The
// convention is generic: the prober sends a TRIGGER header marking the request
// as a probe plus an ECHO header set to a SENTINEL placeholder; the matched
// route carries a RequestHeaderModifier that rewrites the echo header from the
// sentinel to the concrete version; in a normal gateway a backend then echoes
// that header back so the prober can compare it.
//
// A convergence check is a question about the GATEWAY's own state, so the
// gateway is the authoritative responder — this proxy answers such probes
// itself instead of forwarding. Forwarding is also actively broken here: for a
// Knative DomainMapping the route's backend is the ksvc's ExternalName Service,
// which loops the request back into this proxy. On that re-entry the sentinel
// has already been rewritten to the concrete value, so the inner probe rule no
// longer matches and the request degrades into ordinary traffic — a 404, or a
// hang past the probe timeout when the revision is scaled to zero. Answering at
// the gateway is single-hop and replica-count independent.
//
// Deliberate Gateway API deviation: the proxy synthesizes a 200 for a request
// it would otherwise forward. It is scoped to recognized self-probe protocols
// arriving on the in-cluster listener (see InClusterProbeMiddleware), so the
// Cloudflare edge / tunnel path can never be probe-spoofed.

// SelfProbeProtocol describes one config-convergence probe convention. It is
// deliberately decoupled from any specific ingress integration: register a new
// protocol in selfProbeProtocols to support another system's probe, with no
// other code changes.
type SelfProbeProtocol struct {
	// Name identifies the protocol in logs and diagnostics.
	Name string
	// TriggerHeader / TriggerValue mark a request as a probe.
	TriggerHeader string
	TriggerValue  string
	// EchoHeader is the header whose concrete value the prober expects echoed
	// back. Sentinel is the placeholder the prober sends in it; the matched
	// route's RequestHeaderModifier rewrites the sentinel to the concrete
	// version, which is the value the gateway answers with.
	EchoHeader string
	Sentinel   string
}

// defaultSelfProbeProtocols is the ordered set of recognized self-probe
// protocols; first match wins. Add a SelfProbeProtocol entry to teach the
// gateway another integration's convergence probe — no other code changes
// needed. Returned fresh rather than held as a package global (no mutable
// globals); the list is tiny and built only when an in-cluster probe arrives.
//
// The Knative net-gateway-api entry hardcodes its wire strings rather than
// importing knative.dev/networking: they are stable across upstream
// header-constant renames and avoid a module dependency for four values.
func defaultSelfProbeProtocols() []SelfProbeProtocol {
	return []SelfProbeProtocol{{
		Name:          "knative-net-gateway-api",
		TriggerHeader: "K-Network-Probe",
		TriggerValue:  "probe",
		EchoHeader:    "K-Network-Hash",
		Sentinel:      "override",
	}}
}

// triggered reports whether req is a probe under this protocol. It MUST be
// evaluated before request filters run: the prober sends EchoHeader == Sentinel,
// and the matched rule's filter rewrites that to the concrete value, after which
// this would (correctly) go false. Gating on the sentinel is what keeps the ack
// from misfiring on a forwarded re-entry that already carries a concrete value.
func (p *SelfProbeProtocol) triggered(req *http.Request) bool {
	return req.Header.Get(p.TriggerHeader) == p.TriggerValue &&
		req.Header.Get(p.EchoHeader) == p.Sentinel
}

// echoValue returns the concrete value the matched rule's RequestHeaderModifier
// would set on EchoHeader, and whether one was found. The LITERAL value is
// returned (any rollout prefix intact) so the prober's version check converges.
// Last write wins, mirroring applyHeaderModifier's headers.Set; an empty value
// counts as not found.
func (p *SelfProbeProtocol) echoValue(filters []RouteFilter) (string, bool) {
	target := http.CanonicalHeaderKey(p.EchoHeader)

	value := ""
	found := false

	for _, filter := range filters {
		if filter.Type != FilterRequestHeaderModifier || filter.RequestHeaderModifier == nil {
			continue
		}

		for _, header := range filter.RequestHeaderModifier.Set {
			if http.CanonicalHeaderKey(header.Name) != target || header.Value == "" {
				continue
			}

			value = header.Value
			found = true
		}
	}

	return value, found
}

// answerSelfProbe answers an in-cluster config-convergence probe authoritatively
// and reports whether it did. It writes a body-less 200 with the protocol's echo
// header set to the matched rule's concrete value — exactly what the prober's
// verifier checks. A probe whose matched rule carries no echo-setting filter is
// NOT answered; it falls through to normal forwarding (real probe-emitting
// integrations always stamp the setter, so this only guards against an
// accidental synthetic 200 on an arbitrary route).
//
// Body-less and never hijacking keeps it safe on both the in-cluster net/http
// writer and the cloudflared HTTP/2 writer: it never reaches the writer's
// Hijack-before-WriteHeader rejection or the 101->200 translation. Setting the
// header before WriteHeader is load-bearing on both (headers serialize once at
// WriteHeader).
func answerSelfProbe(writer http.ResponseWriter, req *http.Request, rule *RouteRule) bool {
	return answerSelfProbeWith(defaultSelfProbeProtocols(), writer, req, rule)
}

// answerSelfProbeWith is answerSelfProbe against an explicit protocol list — the
// seam tests use to exercise an arbitrary (non-Knative) protocol.
func answerSelfProbeWith(protocols []SelfProbeProtocol, writer http.ResponseWriter, req *http.Request, rule *RouteRule) bool {
	if rule == nil {
		return false
	}

	for i := range protocols {
		protocol := &protocols[i]
		if !protocol.triggered(req) {
			continue
		}

		value, ok := protocol.echoValue(rule.Filters)
		if !ok {
			continue
		}

		writer.Header().Set(protocol.EchoHeader, value)
		writer.WriteHeader(http.StatusOK)

		return true
	}

	return false
}

// inClusterProbeKey marks a request as having arrived on the in-cluster
// listener. Only such requests may be answered authoritatively, so the
// cloudflared tunnel/edge path can never be probe-spoofed.
type inClusterProbeKey struct{}

// InClusterProbeMiddleware stamps requests served by the in-cluster listener so
// the self-probe ack fires only there. The tunnel path serves the bare handler
// and never carries the flag. Exported so cmd/proxy can wrap the in-cluster
// listener's handler and tests can simulate that path.
func InClusterProbeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		ctx := context.WithValue(req.Context(), inClusterProbeKey{}, true)
		next.ServeHTTP(writer, req.WithContext(ctx))
	})
}

// requestFromInClusterListener reports whether the request passed through
// InClusterProbeMiddleware.
func requestFromInClusterListener(ctx context.Context) bool {
	flag, _ := ctx.Value(inClusterProbeKey{}).(bool)

	return flag
}
