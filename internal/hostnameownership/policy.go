// Package hostnameownership implements the controller-side enforcement layer
// of the per-namespace hostname-ownership policy (issue #475).
//
// The policy binds a namespace to one allowed hostname suffix via a namespace
// label and rejects routes whose hostnames fall outside it. It ships in TWO
// independent layers — defence in depth ("make it impossible twice"):
//
//  1. A CEL ValidatingAdmissionPolicy (Helm-rendered, fail-fast at admission).
//  2. THIS package, evaluated by the controller during route binding: a
//     violating route is never programmed into the proxy config or the
//     Cloudflare ingress document, even when the admission layer is absent
//     (pre-1.30 cluster, policy deleted, object written behind the apiserver).
//
// The two layers MUST agree bit-for-bit ON THE SUFFIX DECISION — given the same
// namespace label and hostname, both reach the same allow/deny verdict. The
// shared semantic contract lives in Vectors(): the package unit tests drive it
// through Evaluate, the e2e suite drives the SAME table through the rendered
// ValidatingAdmissionPolicy. Change the semantics in one place only by changing
// the vectors first.
//
// The layers deliberately differ in WHICH routes they evaluate (the matched
// set, not the decision): the admission policy polices every HTTPRoute and
// GRPCRoute in a policed namespace regardless of parentRef, while this layer
// only rejects routes binding to THIS controller's Gateways. On a
// multi-implementation cluster a route targeting a foreign GatewayClass is
// denied at admission but never reaches Evaluate. This scope difference is
// intentional and documented in the chart values; it is not a contract drift.
package hostnameownership

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/errors"
	"k8s.io/apimachinery/pkg/labels"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// RouteReasonHostnameNotPermitted is the Accepted=False condition reason for
// a route rejected by the ownership policy. Implementation-specific reason —
// the Gateway API defines no route-to-route hostname ownership; condition
// reasons are open for implementations.
const RouteReasonHostnameNotPermitted gatewayv1.RouteConditionReason = "HostnameNotPermitted"

var errEmptyLabelKey = errors.New("hostname ownership: label key must not be empty")

// Policy is the compiled controller-side ownership rule.
type Policy struct {
	labelKey string
	// selector scopes which namespaces are policed. nil (from an empty
	// selector string) polices EVERY namespace — fail-closed everywhere.
	selector labels.Selector
}

// Verdict is the outcome of one route evaluation.
type Verdict struct {
	Allowed bool
	// Message is the operator-facing denial explanation; empty when allowed.
	Message string
}

// New compiles a Policy. namespaceSelector uses kubectl label-selector syntax
// ("" = police all namespaces); a malformed selector errors at construction so
// a typo fails the controller loudly instead of silently policing nothing.
func New(labelKey, namespaceSelector string) (*Policy, error) {
	if strings.TrimSpace(labelKey) == "" {
		return nil, errEmptyLabelKey
	}

	policy := &Policy{labelKey: labelKey}

	if strings.TrimSpace(namespaceSelector) != "" {
		selector, err := labels.Parse(namespaceSelector)
		if err != nil {
			return nil, errors.Wrap(err, "hostname ownership: parse namespace selector")
		}

		policy.selector = selector
	}

	return policy, nil
}

// Evaluate applies the policy to a route's namespace labels and declared
// hostnames. Semantics mirror the CEL ValidatingAdmissionPolicy exactly
// (see Vectors for the shared contract):
//
//   - A namespace outside the selector scope is not policed → allowed.
//   - A policed namespace MUST carry the ownership label → fail closed.
//   - The route MUST declare explicit hostnames (an empty list inherits the
//     listener hostname — the exact capture vector) → fail closed.
//   - Every hostname, after stripping a leading "*.", must equal the suffix
//     or be a subdomain of it ("evil<suffix>" does not pass — the subdomain
//     check requires the "." boundary).
func (p *Policy) Evaluate(nsLabels map[string]string, hostnames []gatewayv1.Hostname) Verdict {
	if p.selector != nil && !p.selector.Matches(labels.Set(nsLabels)) {
		return Verdict{Allowed: true}
	}

	// Only lowercase — matching the CEL layer's lowerAscii() exactly. No
	// TrimSpace: the apiserver's label-value regex forbids leading/trailing
	// whitespace, so trimming would be dead code that makes the two layers'
	// normalisation diverge on paper. The normalisation is bit-for-bit identical.
	suffix := strings.ToLower(nsLabels[p.labelKey])
	if suffix == "" {
		return Verdict{Allowed: false, Message: fmt.Sprintf(
			"namespace is policed by the hostname-ownership policy but carries no %q label; "+
				"label the namespace with its allowed hostname suffix or exclude it from the policy scope", p.labelKey)}
	}

	if len(hostnames) == 0 {
		return Verdict{Allowed: false, Message: fmt.Sprintf(
			"routes in hostname-ownership-policed namespaces must declare spec.hostnames explicitly "+
				"(an empty list would capture the listener hostname); allowed suffix: %q", suffix)}
	}

	for _, hostname := range hostnames {
		if !hostnameWithinSuffix(string(hostname), suffix) {
			return Verdict{Allowed: false, Message: fmt.Sprintf(
				"hostname %q is outside the namespace's allowed suffix %q; "+
					"all route hostnames must equal the suffix or be subdomains of it", string(hostname), suffix)}
		}
	}

	return Verdict{Allowed: true}
}

// hostnameWithinSuffix reports whether hostname (with an optional "*." prefix
// stripped) equals suffix or is a subdomain of it. Comparison is lowercase —
// DNS names are case-insensitive.
//
// strings.ToLower folds Unicode (e.g. U+212A KELVIN SIGN → "k"), while the CEL
// admission layer uses lowerAscii(). That divergence is a deny↔allow axis ONLY
// if a non-ASCII rune can reach this function — and it cannot: the Gateway API
// CRD Hostname pattern and the namespace label-value regex are both
// ASCII-only (the label value may be mixed-case ASCII, which is exactly why it
// is lowercased here), so hostname and suffix are always ASCII. If that CRD
// pattern is ever relaxed to admit Unicode, this homograph axis reopens and
// both layers must switch to one shared normalization.
func hostnameWithinSuffix(hostname, suffix string) bool {
	candidate := strings.ToLower(hostname)
	candidate = strings.TrimPrefix(candidate, "*.")

	return candidate == suffix || strings.HasSuffix(candidate, "."+suffix)
}
