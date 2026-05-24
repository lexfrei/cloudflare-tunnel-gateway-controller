package proxy

import (
	"io"
	"net/http"
	"strconv"
	"strings"
)

// corsWildcard is the literal string the Gateway API CORS filter uses to mean
// "match any" in AllowOrigins / AllowMethods / AllowHeaders / ExposeHeaders.
const corsWildcard = "*"

// defaultCORSMaxAgeSeconds matches the Gateway API spec default applied by
// the CRD when MaxAge is omitted.
const defaultCORSMaxAgeSeconds = 5

// HTTP header names used by the CORS filter. Comparison against incoming
// request headers is case-insensitive (http.Header normalises on read);
// emission uses these canonical forms.
const (
	headerOrigin                        = "Origin"
	headerAccessControlRequestMethod    = "Access-Control-Request-Method"
	headerAccessControlRequestHeaders   = "Access-Control-Request-Headers"
	headerAccessControlAllowOrigin      = "Access-Control-Allow-Origin"
	headerAccessControlAllowCredentials = "Access-Control-Allow-Credentials"
	headerAccessControlAllowMethods     = "Access-Control-Allow-Methods"
	headerAccessControlAllowHeaders     = "Access-Control-Allow-Headers"
	headerAccessControlExposeHeaders    = "Access-Control-Expose-Headers"
	headerAccessControlMaxAge           = "Access-Control-Max-Age"
)

// CORSConfig captures the Gateway API HTTPCORSFilter parameters needed by the
// proxy to implement CORS preflight and to attach response headers on simple
// (non-preflight) cross-origin requests.
//
// AllowOrigins entries are either the universal "*" (matches all), an exact
// scheme://host[:port] literal, or a scheme://*.host[:port] pattern where
// "*" greedy-matches any number of left-side DNS labels.
//
// AllowMethods / AllowHeaders entries are specific names or a single "*"
// (matches all). MaxAge defaults to 5 seconds when zero.
type CORSConfig struct {
	AllowOrigins     []string `json:"allowOrigins,omitempty"`
	AllowCredentials bool     `json:"allowCredentials,omitempty"`
	AllowMethods     []string `json:"allowMethods,omitempty"`
	AllowHeaders     []string `json:"allowHeaders,omitempty"`
	ExposeHeaders    []string `json:"exposeHeaders,omitempty"`
	MaxAge           int32    `json:"maxAge,omitempty"`
}

// CORSFilter implements Gateway API HTTPCORSFilter for both preflight
// (OPTIONS + Access-Control-Request-Method) and simple cross-origin requests.
// Preflight is short-circuited at the request stage; simple requests pass
// through to the backend and gain CORS response headers on the way back.
type CORSFilter struct {
	cfg *CORSConfig
}

// NewCORSFilter constructs a CORSFilter from a config. cfg may be nil — the
// filter then no-ops on both request and response, matching what an
// HTTPRouteFilter of type CORS with a missing cors field would mean.
func NewCORSFilter(cfg *CORSConfig) *CORSFilter {
	return &CORSFilter{cfg: cfg}
}

// ProcessRequest handles CORS preflight. Non-preflight requests pass through
// (returns nil); response headers are added later by ProcessResponse. A
// preflight from a matched Origin returns a synthetic 204 carrying the
// negotiated CORS headers. A preflight from a non-matched Origin returns
// 204 with NO CORS headers — the browser then fails the cross-origin
// request on the client side.
func (f *CORSFilter) ProcessRequest(req *http.Request) *http.Response {
	if f.cfg == nil || !isCORSPreflight(req) {
		return nil
	}

	origin := req.Header.Get(headerOrigin)
	header := make(http.Header)

	if originAllowed(origin, f.cfg.AllowOrigins) {
		f.writePreflightHeaders(header, origin, req)
	}

	return &http.Response{
		StatusCode: http.StatusNoContent,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}
}

// ProcessResponse attaches CORS response headers to a simple (non-preflight)
// cross-origin response. No-ops when the request carries no Origin header,
// when the Origin is not in the allow list, or when the request is a
// preflight (whose response was already stamped by ProcessRequest).
func (f *CORSFilter) ProcessResponse(resp *http.Response) {
	if f.cfg == nil || resp == nil || resp.Request == nil {
		return
	}

	if isCORSPreflight(resp.Request) {
		return
	}

	origin := resp.Request.Header.Get(headerOrigin)
	if origin == "" {
		return
	}

	if !originAllowed(origin, f.cfg.AllowOrigins) {
		return
	}

	f.writeSimpleHeaders(resp.Header, origin)
}

// writePreflightHeaders fills in the Access-Control-* response headers for a
// preflight reply.
func (f *CORSFilter) writePreflightHeaders(header http.Header, origin string, req *http.Request) {
	header.Set(headerAccessControlAllowOrigin, origin)

	if f.cfg.AllowCredentials {
		header.Set(headerAccessControlAllowCredentials, "true")
	}

	if methods := allowedMethodsValue(req.Header.Get(headerAccessControlRequestMethod), f.cfg); methods != "" {
		header.Set(headerAccessControlAllowMethods, methods)
	}

	if headers := allowedHeadersValue(req.Header.Get(headerAccessControlRequestHeaders), f.cfg); headers != "" {
		header.Set(headerAccessControlAllowHeaders, headers)
	}

	if expose := exposeHeadersValue(f.cfg); expose != "" {
		header.Set(headerAccessControlExposeHeaders, expose)
	}

	header.Set(headerAccessControlMaxAge, strconv.Itoa(maxAgeSeconds(f.cfg)))

	// Vary headers MUST be set whenever the response varies by Origin or by
	// preflight-request inputs — otherwise an intermediate cache (Cloudflare
	// edge, downstream CDN, browser HTTP cache) could serve a cached
	// Access-Control-Allow-* response from a previous matched origin to a
	// request from a different (potentially non-allowed) origin. Use Add so
	// any upstream-supplied Vary value (e.g. backend's "Vary: Accept-Encoding")
	// is preserved.
	header.Add("Vary", "Origin")
	header.Add("Vary", headerAccessControlRequestMethod)
	header.Add("Vary", headerAccessControlRequestHeaders)
}

// writeSimpleHeaders fills in the Access-Control-* response headers for a
// simple (non-preflight) cross-origin response.
func (f *CORSFilter) writeSimpleHeaders(header http.Header, origin string) {
	header.Set(headerAccessControlAllowOrigin, origin)

	if f.cfg.AllowCredentials {
		header.Set(headerAccessControlAllowCredentials, "true")
	}

	if expose := exposeHeadersValue(f.cfg); expose != "" {
		header.Set(headerAccessControlExposeHeaders, expose)
	}

	// Vary: Origin is required so caches don't reuse an Allow-Origin from a
	// previous matched origin for a different (potentially non-allowed) one.
	// Use Add to preserve any upstream Vary value.
	header.Add("Vary", "Origin")
}

// isCORSPreflight reports whether a request is a CORS preflight per the Fetch
// spec: method OPTIONS AND an Access-Control-Request-Method header.
func isCORSPreflight(req *http.Request) bool {
	if req == nil || req.Method != http.MethodOptions {
		return false
	}

	return req.Header.Get(headerAccessControlRequestMethod) != ""
}

// allowedMethodsValue returns the value of the Access-Control-Allow-Methods
// response header. When the policy uses "*", the requested method is echoed
// back when present. With AllowCredentials=true the gateway must NOT emit
// "*" — so if there's no request method to echo and credentials are
// enabled, the header is omitted entirely.
func allowedMethodsValue(requestedMethod string, cfg *CORSConfig) string {
	if len(cfg.AllowMethods) == 0 {
		return ""
	}

	if len(cfg.AllowMethods) == 1 && cfg.AllowMethods[0] == corsWildcard {
		if requestedMethod != "" {
			return requestedMethod
		}

		if cfg.AllowCredentials {
			return ""
		}

		return corsWildcard
	}

	return strings.Join(cfg.AllowMethods, ", ")
}

// allowedHeadersValue returns the value of the Access-Control-Allow-Headers
// response header. Same wildcard/credentials logic as allowedMethodsValue.
func allowedHeadersValue(requestedHeaders string, cfg *CORSConfig) string {
	if len(cfg.AllowHeaders) == 0 {
		return ""
	}

	if len(cfg.AllowHeaders) == 1 && cfg.AllowHeaders[0] == corsWildcard {
		if requestedHeaders != "" {
			return requestedHeaders
		}

		if cfg.AllowCredentials {
			return ""
		}

		return corsWildcard
	}

	return strings.Join(cfg.AllowHeaders, ", ")
}

// exposeHeadersValue returns the value of the Access-Control-Expose-Headers
// response header. Per Gateway API spec (HTTPCORSFilter.ExposeHeaders docs):
// "The Access-Control-Expose-Headers response header can only use `*`
// wildcard as value when the request is not credentialed." So when the policy
// is `["*"]` AND credentials are enabled, the header is omitted entirely —
// there's nothing safe to echo here (the spec doesn't define an
// Access-Control-Request-Expose-Headers request header to mirror).
func exposeHeadersValue(cfg *CORSConfig) string {
	if len(cfg.ExposeHeaders) == 0 {
		return ""
	}

	if len(cfg.ExposeHeaders) == 1 && cfg.ExposeHeaders[0] == corsWildcard {
		if cfg.AllowCredentials {
			return ""
		}

		return corsWildcard
	}

	return strings.Join(cfg.ExposeHeaders, ", ")
}

// maxAgeSeconds returns the MaxAge value to emit, applying the spec default
// of 5 seconds when the policy carries 0.
func maxAgeSeconds(cfg *CORSConfig) int {
	if cfg.MaxAge <= 0 {
		return defaultCORSMaxAgeSeconds
	}

	return int(cfg.MaxAge)
}

// originAllowed reports whether origin satisfies any entry in allowOrigins.
// "*" alone matches every origin. Other entries must be a scheme://host[:port]
// pattern with the same scheme as the origin; the host part may carry the
// leading "*." wildcard which greedy-matches any number of left-side DNS
// labels (e.g. "https://*.bar.com" matches "https://x.y.bar.com").
//
// Empty origin or empty allow-list always returns false. Defensive check: a
// literal "*" in the request Origin header is treated as malformed (browsers
// never send it; the Fetch spec defines Origin as scheme+host[+port] or
// the literal "null"). Reflecting "*" back as Access-Control-Allow-Origin
// would let a misbehaving client manufacture a wildcard response from an
// otherwise-strict policy.
func originAllowed(origin string, allowOrigins []string) bool {
	if origin == "" || origin == corsWildcard || len(allowOrigins) == 0 {
		return false
	}

	for _, allowed := range allowOrigins {
		if allowed == corsWildcard {
			return true
		}

		if originMatchesPattern(origin, allowed) {
			return true
		}
	}

	return false
}

// originMatchesPattern matches origin against a single allow-list pattern
// (NOT the universal "*"). Scheme must match. Host portion may carry a
// leading "*." wildcard.
func originMatchesPattern(origin, pattern string) bool {
	if pattern == origin {
		return true
	}

	originScheme, originHost, ok := splitOrigin(origin)
	if !ok {
		return false
	}

	patternScheme, patternHost, ok := splitOrigin(pattern)
	if !ok {
		return false
	}

	if originScheme != patternScheme {
		return false
	}

	return hostMatchesPattern(originHost, patternHost)
}

// splitOrigin returns (scheme, host[:port], ok) for an origin string. ok=false
// when the value isn't a well-formed scheme://rest pair.
func splitOrigin(origin string) (string, string, bool) {
	scheme, rest, ok := strings.Cut(origin, "://")
	if !ok || scheme == "" || rest == "" {
		return "", "", false
	}

	return scheme, rest, true
}

// hostMatchesPattern matches an origin host[:port] against a pattern that may
// start with "*." — in which case any number of left-side DNS labels are
// allowed. Port matching is by string equality, so the pattern must include
// the port explicitly when the origin carries one (and vice versa).
func hostMatchesPattern(host, pattern string) bool {
	if pattern == host {
		return true
	}

	const wildcardPrefix = "*."

	if !strings.HasPrefix(pattern, wildcardPrefix) {
		return false
	}

	// Keep the leading "." so "*.bar.com" → ".bar.com" — that way "bar.com"
	// (no leading label) is rejected, only "x.bar.com" and below match.
	suffix := pattern[len(wildcardPrefix)-1:]

	return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
}
