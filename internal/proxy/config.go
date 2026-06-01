// Package proxy implements a Gateway API-compliant L7 reverse proxy.
// It provides full HTTPRoute matching (path, headers, query params, method),
// filtering (header modification, redirects, rewrites, mirroring),
// and weighted backend selection.
package proxy

import (
	"encoding/json"
	"time"

	"github.com/cockroachdb/errors"
)

// Sentinel validation errors.
var (
	errURLRequired       = errors.New("url is required")
	errWeightNonNegative = errors.New("weight must be non-negative")
	errUnknownPathType   = errors.New("unknown path match type")
	errUnknownHeaderType = errors.New("unknown header match type")
	errUnknownQueryType  = errors.New("unknown query param match type")
	errNameRequired      = errors.New("name is required")
	errPathValueRequired = errors.New("path: value is required")
	errUnknownFilterType = errors.New("unknown filter type")
	errStaleVersion      = errors.New("stale config version")
	// errTLSMirrorWithoutTransportFactory is returned by Router.UpdateConfig
	// when a config carries a RequestMirror filter whose TLS is set but the
	// Router was never given a TransportFactory via SetHandler. Mirror
	// filters would otherwise fall back to the global cleartext mirrorClient
	// and silently bypass the operator's TLS expectation — the exact
	// regression the per-cert pool integration was added to prevent.
	errTLSMirrorWithoutTransportFactory = errors.New("config carries TLS-bearing RequestMirror filter but Router has no TransportFactory wired; call Router.SetHandler before UpdateConfig")
)

// Config is the top-level configuration pushed by the controller.
// It contains a version for ordering and a list of routing rules.
type Config struct {
	Version int64       `json:"version"`
	Rules   []RouteRule `json:"rules"`
	// HasGRPCRoute is true when at least one GRPCRoute contributed to this
	// config. The proxy reads it at startup to upgrade an "auto"/unset edge
	// transport to http2: gRPC requires http2 because cloudflared drops HTTP
	// trailers over QUIC (grpc-status is lost). On the wire gRPC rules are
	// indistinguishable from HTTP rules with h2c backends, so the controller
	// marks the presence of gRPC explicitly rather than having the proxy guess.
	HasGRPCRoute bool `json:"hasGrpcRoute,omitempty"`
	// Diagnostics carries the converter's per-route findings about config it
	// will not serve exactly as written (dropped filters, unsupported app
	// protocols, fail-closed rules, benign overrides). It is a controller-side
	// concern only: the json:"-" tag keeps it off the proxy wire so the pushed
	// payload is byte-identical to before, and the proxy never reads it. The
	// controller turns each entry into a status condition or a Kubernetes Event.
	Diagnostics []RouteDiagnostic `json:"-"`
}

// RouteRule represents a single routing rule derived from a Gateway API HTTPRoute.
// Each rule maps a set of hostnames + match conditions to a set of backends.
type RouteRule struct {
	Hostnames []string       `json:"hostnames,omitempty"`
	Matches   []RouteMatch   `json:"matches,omitempty"`
	Filters   []RouteFilter  `json:"filters,omitempty"`
	Backends  []BackendRef   `json:"backends"`
	Timeouts  *RouteTimeouts `json:"timeouts,omitempty"`
	// UnavailableStatus, when non-zero, makes the proxy return this HTTP status
	// for every request matching the rule, short-circuiting backend selection.
	// The controller sets it when a rule cannot be served as written — for
	// example a rule carrying an unsupported filter type — so that matched
	// requests fail closed with an HTTP error instead of being served silently
	// without the dropped config, as the Gateway API spec requires. This is the
	// rule-level analogue of BackendRef.UnavailableStatus.
	UnavailableStatus int `json:"unavailableStatus,omitempty"`
}

// RouteMatch defines conditions that must all be true for a request to match.
// Multiple matches within a rule are ORed; conditions within a match are ANDed.
type RouteMatch struct {
	Path        *PathMatch        `json:"path,omitempty"`
	Headers     []HeaderMatch     `json:"headers,omitempty"`
	QueryParams []QueryParamMatch `json:"queryParams,omitempty"`
	Method      string            `json:"method,omitempty"`
}

// PathMatchType defines how path matching is performed.
type PathMatchType string

const (
	PathMatchExact             PathMatchType = "Exact"
	PathMatchPathPrefix        PathMatchType = "PathPrefix"
	PathMatchRegularExpression PathMatchType = "RegularExpression"
)

// PathMatch specifies how to match the request path.
type PathMatch struct {
	Type  PathMatchType `json:"type"`
	Value string        `json:"value"`
}

// HeaderMatchType defines how header matching is performed.
type HeaderMatchType string

const (
	HeaderMatchExact             HeaderMatchType = "Exact"
	HeaderMatchRegularExpression HeaderMatchType = "RegularExpression"
)

// HeaderMatch specifies a condition on request headers.
type HeaderMatch struct {
	Type  HeaderMatchType `json:"type"`
	Name  string          `json:"name"`
	Value string          `json:"value"`
}

// QueryParamMatchType defines how query parameter matching is performed.
type QueryParamMatchType string

const (
	QueryParamMatchExact             QueryParamMatchType = "Exact"
	QueryParamMatchRegularExpression QueryParamMatchType = "RegularExpression"
)

// QueryParamMatch specifies a condition on URL query parameters.
type QueryParamMatch struct {
	Type  QueryParamMatchType `json:"type"`
	Name  string              `json:"name"`
	Value string              `json:"value"`
}

// RouteFilterType identifies the kind of filter to apply.
type RouteFilterType string

const (
	FilterRequestHeaderModifier  RouteFilterType = "RequestHeaderModifier"
	FilterResponseHeaderModifier RouteFilterType = "ResponseHeaderModifier"
	FilterRequestRedirect        RouteFilterType = "RequestRedirect"
	FilterURLRewrite             RouteFilterType = "URLRewrite"
	FilterRequestMirror          RouteFilterType = "RequestMirror"
	FilterCORS                   RouteFilterType = "CORS"
)

// RouteFilter defines a transformation to apply to matching requests.
type RouteFilter struct {
	Type                   RouteFilterType   `json:"type"`
	RequestHeaderModifier  *HeaderModifier   `json:"requestHeaderModifier,omitempty"`
	ResponseHeaderModifier *HeaderModifier   `json:"responseHeaderModifier,omitempty"`
	RequestRedirect        *RedirectConfig   `json:"requestRedirect,omitempty"`
	URLRewrite             *URLRewriteConfig `json:"urlRewrite,omitempty"`
	RequestMirror          *MirrorConfig     `json:"requestMirror,omitempty"`
	CORS                   *CORSConfig       `json:"cors,omitempty"`
}

// HeaderModifier describes modifications to HTTP headers.
type HeaderModifier struct {
	Set    []HeaderValue `json:"set,omitempty"`
	Add    []HeaderValue `json:"add,omitempty"`
	Remove []string      `json:"remove,omitempty"`
}

// HeaderValue is a name-value pair for a header.
type HeaderValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// RedirectConfig describes an HTTP redirect response.
type RedirectConfig struct {
	Scheme     *string       `json:"scheme,omitempty"`
	Hostname   *string       `json:"hostname,omitempty"`
	Port       *int32        `json:"port,omitempty"`
	Path       *RedirectPath `json:"path,omitempty"`
	StatusCode *int          `json:"statusCode,omitempty"`
}

// RedirectPathType defines the redirect path strategy.
type RedirectPathType string

const (
	// RedirectPathFullReplace replaces the entire path.
	RedirectPathFullReplace RedirectPathType = "ReplaceFullPath"
	// RedirectPathPrefixReplace replaces only the matched prefix portion.
	RedirectPathPrefixReplace RedirectPathType = "ReplacePrefixMatch"
)

// RedirectPath specifies how to modify the path in a redirect.
type RedirectPath struct {
	Type  RedirectPathType `json:"type"`
	Value string           `json:"value"`
}

// URLRewriteConfig describes how to rewrite the request URL.
type URLRewriteConfig struct {
	Hostname *string         `json:"hostname,omitempty"`
	Path     *URLRewritePath `json:"path,omitempty"`
}

// URLRewritePathType defines the rewrite strategy.
type URLRewritePathType string

const (
	URLRewriteFullPath    URLRewritePathType = "ReplaceFullPath"
	URLRewritePrefixMatch URLRewritePathType = "ReplacePrefixMatch"
)

// URLRewritePath specifies how to rewrite the path.
type URLRewritePath struct {
	Type               URLRewritePathType `json:"type"`
	ReplaceFullPath    *string            `json:"replaceFullPath,omitempty"`
	ReplacePrefixMatch *string            `json:"replacePrefixMatch,omitempty"`
}

// MirrorConfig describes where to mirror requests and at what rate.
type MirrorConfig struct {
	BackendURL string `json:"backendUrl"`
	// Percent is the percentage of requests to mirror to the backend
	// (0-100 inclusive). nil means 100% (mirror every request). A Fraction
	// on the Gateway API filter is normalized into Percent at conversion
	// time so the proxy speaks one shape.
	Percent *int32 `json:"percent,omitempty"`
	// TLS, when non-nil, indicates that a BackendTLSPolicy targets the
	// mirror destination Service. The filter borrows a per-cert TLS-aware
	// RoundTripper from the Handler's transport pool — same shape the main
	// leg uses — instead of dialing cleartext through the global
	// mirrorClient. Without this, a TLS-only mirror backend would refuse
	// the connection and the mirrored copy would be silently lost.
	TLS *BackendTLSConfig `json:"tls,omitempty"`
	// Protocol mirrors BackendRef.Protocol for the mirror destination so
	// the transport pool key (host, protocol, TLS, headerTimeout) matches
	// what the main leg would build for the same backend. Empty (default
	// BackendProtocolHTTP) for every mirror destination today; carried as
	// a field so future protocol marker additions to BackendRef
	// automatically extend to the mirror leg.
	Protocol BackendProtocol `json:"protocol,omitempty"`
}

// BackendProtocol identifies the application protocol the proxy must speak to a
// backend. It is derived from the Service port's appProtocol field.
type BackendProtocol string

const (
	// BackendProtocolHTTP is the default: HTTP/1.1 (and TLS when the URL scheme is https).
	BackendProtocolHTTP BackendProtocol = ""
	// BackendProtocolH2C is HTTP/2 over cleartext (prior knowledge), selected by
	// the Service port appProtocol kubernetes.io/h2c.
	BackendProtocolH2C BackendProtocol = "h2c"
)

// isKnownBackendProtocol reports whether p is one of the protocols the proxy
// recognises. Used by Config.Validate to reject pushed configs with unknown
// values rather than silently falling through to the default-transport path.
func isKnownBackendProtocol(p BackendProtocol) bool {
	switch p {
	case BackendProtocolHTTP, BackendProtocolH2C:
		return true
	default:
		return false
	}
}

// BackendTLSConfig captures the per-backend TLS verification settings derived
// from a Gateway API BackendTLSPolicy that targets the Service this BackendRef
// points at. The proxy uses it to build a TLS transport that validates the
// backend's server certificate against the supplied CA bundle and hostname.
type BackendTLSConfig struct {
	// CABundlePEM is the concatenated PEM-encoded CA certificate bundle the
	// proxy uses as the trust anchor when verifying the backend's certificate.
	CABundlePEM string `json:"caBundle,omitempty"`
	// ServerName is used both as the TLS SNI value and as the expected
	// hostname during certificate verification (DNS SAN match).
	ServerName string `json:"serverName,omitempty"`
	// SubjectAltNames lists hostnames (DNS SAN) that the backend cert may
	// present to satisfy this policy. Empty when only URI SANs are required.
	// When SubjectAltNames or SubjectAltNameURIs is non-empty, ServerName is
	// used for SNI only and NOT for authentication (per Gateway API spec).
	SubjectAltNames []string `json:"subjectAltNames,omitempty"`
	// SubjectAltNameURIs lists URI SANs (e.g. SPIFFE IDs) that the backend
	// cert may present to satisfy this policy. Matched by exact string
	// equality against each URI in the leaf cert's URIs field.
	SubjectAltNameURIs []string `json:"subjectAltNameUris,omitempty"`
	// ClientCertPEM is the PEM-encoded client certificate (optionally a chain
	// of leaf + intermediates) the proxy presents during the backend TLS
	// handshake. Sourced from gateway.spec.tls.backend.clientCertificateRef.
	// Empty when the Gateway does not configure a backend client certificate.
	ClientCertPEM []byte `json:"clientCertPem,omitempty"`
	// ClientKeyPEM is the PEM-encoded private key matching ClientCertPEM.
	// Must be set together with ClientCertPEM; either both or neither.
	ClientKeyPEM []byte `json:"clientKeyPem,omitempty"`
}

// HasSANConstraints reports whether the policy requires SAN-list verification
// (any of DNS or URI). When true, the proxy disables stdlib hostname
// verification and runs the manual chain + OR-match path.
func (c *BackendTLSConfig) HasSANConstraints() bool {
	if c == nil {
		return false
	}

	return len(c.SubjectAltNames) > 0 || len(c.SubjectAltNameURIs) > 0
}

// HasClientCert reports whether the config carries a non-empty client cert
// keypair that should be attached to the outgoing TLS handshake. Both PEM
// blocks must be set; either alone is treated as "no client cert" so that a
// half-configured policy never silently flips an mTLS connection to one-way.
func (c *BackendTLSConfig) HasClientCert() bool {
	if c == nil {
		return false
	}

	return len(c.ClientCertPEM) > 0 && len(c.ClientKeyPEM) > 0
}

// BackendRef identifies a backend service with a weight for traffic splitting.
type BackendRef struct {
	URL      string            `json:"url"`
	Weight   int32             `json:"weight"`
	Protocol BackendProtocol   `json:"protocol,omitempty"`
	TLS      *BackendTLSConfig `json:"tls,omitempty"`
	Filters  []RouteFilter     `json:"filters,omitempty"`
	// WebSocket flags this backend as WebSocket-capable per the operator's
	// `Service.spec.ports[].appProtocol: kubernetes.io/ws` (or `wss`) hint.
	// It is the sole input to the upgrade-aware timeout-skip in Handler:
	// requests carrying `Connection: Upgrade` headers bypass
	// `timeouts.request`/`timeouts.backend` only when this flag is true.
	// Gating on a client-controlled header alone would let any request
	// claim upgrade and bypass operator-declared deadlines, opening a
	// slow-loris vector and violating the Gateway API timeout contract
	// for non-WebSocket routes.
	WebSocket bool `json:"websocket,omitempty"`
	// UnavailableStatus, when non-zero, makes the proxy return this HTTP status
	// for requests routed to this backend instead of dialing it. The backend
	// stays in the weighted pool so its traffic fraction is preserved per the
	// Gateway API spec. The controller sets 500 for an invalid backendRef (a
	// nonexistent Service) and 503 for a Service that exists but has no ready
	// endpoints, applied to the proportion of requests that would otherwise have
	// been routed to this backend.
	UnavailableStatus int `json:"unavailableStatus,omitempty"`
}

// RouteTimeouts configures timeout durations for proxied requests.
// Custom JSON marshal/unmarshal serializes durations as human-readable strings
// (e.g. "10s") instead of nanosecond integers.
type RouteTimeouts struct {
	Request time.Duration `json:"request,omitempty"`
	Backend time.Duration `json:"backend,omitempty"`
}

// routeTimeoutsJSON is the JSON wire format for RouteTimeouts.
type routeTimeoutsJSON struct {
	Request string `json:"request,omitempty"`
	Backend string `json:"backend,omitempty"`
}

// MarshalJSON serializes durations as human-readable strings.
func (rt RouteTimeouts) MarshalJSON() ([]byte, error) {
	aux := routeTimeoutsJSON{}

	if rt.Request != 0 {
		aux.Request = rt.Request.String()
	}

	if rt.Backend != 0 {
		aux.Backend = rt.Backend.String()
	}

	result, err := json.Marshal(aux)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal route timeouts")
	}

	return result, nil
}

// UnmarshalJSON deserializes durations from human-readable strings.
func (rt *RouteTimeouts) UnmarshalJSON(data []byte) error {
	var aux routeTimeoutsJSON

	err := json.Unmarshal(data, &aux)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal route timeouts")
	}

	if aux.Request != "" {
		d, err := time.ParseDuration(aux.Request)
		if err != nil {
			return errors.Wrapf(err, "invalid request timeout %q", aux.Request)
		}

		rt.Request = d
	}

	if aux.Backend != "" {
		d, err := time.ParseDuration(aux.Backend)
		if err != nil {
			return errors.Wrapf(err, "invalid backend timeout %q", aux.Backend)
		}

		rt.Backend = d
	}

	return nil
}

// Validate checks that the Config is well-formed.
func (c *Config) Validate() error {
	if c.Version < 0 {
		return errors.New("version must be non-negative")
	}

	for idx, rule := range c.Rules {
		err := rule.validate()
		if err != nil {
			return errors.Wrapf(err, "rule[%d]", idx)
		}
	}

	return nil
}

func (r *RouteRule) validate() error {
	// Rules with empty backends and no redirect filter are valid —
	// per Gateway API spec, the proxy handler returns HTTP 500 for
	// routes with unresolvable backend refs.
	for idx, backend := range r.Backends {
		if backend.URL == "" {
			return errors.Wrapf(errURLRequired, "backend[%d]", idx)
		}

		if backend.Weight < 0 {
			return errors.Wrapf(errWeightNonNegative, "backend[%d]", idx)
		}

		if !isKnownBackendProtocol(backend.Protocol) {
			return errors.Errorf("backend[%d]: unknown protocol %q", idx, string(backend.Protocol))
		}

		for filterIdx, filter := range backend.Filters {
			err := filter.validate()
			if err != nil {
				return errors.Wrapf(err, "backend[%d].filter[%d]", idx, filterIdx)
			}
		}
	}

	for idx, match := range r.Matches {
		err := match.validate()
		if err != nil {
			return errors.Wrapf(err, "match[%d]", idx)
		}
	}

	for idx, filter := range r.Filters {
		err := filter.validate()
		if err != nil {
			return errors.Wrapf(err, "filter[%d]", idx)
		}
	}

	return nil
}

func (m *RouteMatch) validate() error {
	if m.Path != nil {
		switch m.Path.Type {
		case PathMatchExact, PathMatchPathPrefix, PathMatchRegularExpression:
			// valid
		default:
			return errors.Wrapf(errUnknownPathType, "%q", m.Path.Type)
		}

		if m.Path.Value == "" {
			return errPathValueRequired
		}
	}

	for idx, header := range m.Headers {
		switch header.Type {
		case HeaderMatchExact, HeaderMatchRegularExpression:
			// valid
		default:
			return errors.Wrapf(errUnknownHeaderType, "header[%d]: %q", idx, header.Type)
		}

		if header.Name == "" {
			return errors.Wrapf(errNameRequired, "header[%d]", idx)
		}
	}

	for idx, query := range m.QueryParams {
		switch query.Type {
		case QueryParamMatchExact, QueryParamMatchRegularExpression:
			// valid
		default:
			return errors.Wrapf(errUnknownQueryType, "queryParam[%d]: %q", idx, query.Type)
		}

		if query.Name == "" {
			return errors.Wrapf(errNameRequired, "queryParam[%d]", idx)
		}
	}

	return nil
}

func (f *RouteFilter) validate() error {
	switch f.Type {
	case FilterRequestHeaderModifier:
		if f.RequestHeaderModifier == nil {
			return errors.New("requestHeaderModifier config is required")
		}

		return nil
	case FilterResponseHeaderModifier:
		if f.ResponseHeaderModifier == nil {
			return errors.New("responseHeaderModifier config is required")
		}

		return nil
	case FilterRequestRedirect:
		if f.RequestRedirect == nil {
			return errors.New("requestRedirect config is required")
		}

		return nil
	case FilterURLRewrite:
		if f.URLRewrite == nil {
			return errors.New("urlRewrite config is required")
		}

		return nil
	case FilterRequestMirror:
		return validateMirrorFilter(f.RequestMirror)
	case FilterCORS:
		return validateCORSFilter(f.CORS)
	default:
		return errors.Wrapf(errUnknownFilterType, "%q", f.Type)
	}
}

// validateMirrorFilter checks per-spec invariants on a RequestMirror filter
// payload. Extracted so RouteFilter.validate stays under the cyclomatic budget
// as new filter types land.
func validateMirrorFilter(m *MirrorConfig) error {
	if m == nil {
		return errors.New("requestMirror config is required")
	}

	if pct := m.Percent; pct != nil && (*pct < 0 || *pct > 100) {
		return errors.Errorf("requestMirror percent must be in [0,100], got %d", *pct)
	}

	return nil
}

// validateCORSFilter checks per-spec invariants on a CORS filter payload.
// MaxAge is signed at the wire level so a negative value would emit a
// nonsensical Access-Control-Max-Age; reject at validate time.
func validateCORSFilter(c *CORSConfig) error {
	if c == nil {
		return errors.New("cors config is required")
	}

	if c.MaxAge < 0 {
		return errors.Errorf("cors maxAge must be non-negative, got %d", c.MaxAge)
	}

	return nil
}

// ParseConfig deserializes and validates a Config from JSON.
func ParseConfig(data []byte) (*Config, error) {
	var cfg Config

	err := json.Unmarshal(data, &cfg)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse config JSON")
	}

	err = cfg.Validate()
	if err != nil {
		return nil, errors.Wrap(err, "invalid config")
	}

	return &cfg, nil
}
