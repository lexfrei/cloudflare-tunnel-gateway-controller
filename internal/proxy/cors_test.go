package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// corsTestRequest builds an *http.Request shaped like one a browser would
// send. method is the wire-level method; method=OPTIONS plus a non-empty
// requestMethod becomes a CORS preflight.
func corsTestRequest(t *testing.T, method, origin, requestMethod, requestHeaders string) *http.Request {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), method, "http://example.com/", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}

	if requestMethod != "" {
		req.Header.Set("Access-Control-Request-Method", requestMethod)
	}

	if requestHeaders != "" {
		req.Header.Set("Access-Control-Request-Headers", requestHeaders)
	}

	return req
}

// corsTestResponse builds a synthetic backend *http.Response whose .Request
// is the supplied request — ProcessResponse uses Request.Header.Origin to
// decide whether to attach CORS response headers.
func corsTestResponse(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}
}

// TestOriginAllowed exercises the Gateway API CORS origin-matching contract.
// Allowed origin entries may be:
//   - the universal "*" wildcard (matches every origin);
//   - an exact scheme://host[:port] literal;
//   - a scheme://*.host[:port] pattern where "*" greedy-matches any number of
//     left-side DNS labels.
//
// Empty origin or empty allow-list always returns false (the CORS filter must
// not emit Access-Control-Allow-Origin if no Origin came in or no allow list
// is configured).
func TestOriginAllowed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		origin       string
		allowOrigins []string
		want         bool
	}{
		{
			name:         "empty origin never matches",
			origin:       "",
			allowOrigins: []string{"*", "https://foo.com"},
			want:         false,
		},
		{
			name:         "empty allow list never matches",
			origin:       "https://foo.com",
			allowOrigins: nil,
			want:         false,
		},
		{
			name:         "universal wildcard matches any origin",
			origin:       "https://anything.com",
			allowOrigins: []string{"*"},
			want:         true,
		},
		{
			name:         "exact literal match",
			origin:       "https://www.foo.com",
			allowOrigins: []string{"https://www.foo.com"},
			want:         true,
		},
		{
			name:         "exact literal mismatch",
			origin:       "https://other.com",
			allowOrigins: []string{"https://www.foo.com"},
			want:         false,
		},
		{
			name:         "scheme mismatch fails",
			origin:       "http://www.foo.com",
			allowOrigins: []string{"https://www.foo.com"},
			want:         false,
		},
		{
			name:         "wildcard host single label",
			origin:       "https://www.bar.com",
			allowOrigins: []string{"https://*.bar.com"},
			want:         true,
		},
		{
			name:         "wildcard host multiple labels",
			origin:       "https://xpto.www.bar.com",
			allowOrigins: []string{"https://*.bar.com"},
			want:         true,
		},
		{
			name:         "wildcard host does not match base domain",
			origin:       "https://bar.com",
			allowOrigins: []string{"https://*.bar.com"},
			want:         false,
		},
		{
			name:         "wildcard host scheme mismatch fails",
			origin:       "http://www.bar.com",
			allowOrigins: []string{"https://*.bar.com"},
			want:         false,
		},
		{
			name:         "multiple allow entries OR-matched",
			origin:       "https://www.bar.com",
			allowOrigins: []string{"https://www.foo.com", "https://*.bar.com"},
			want:         true,
		},
		{
			name:         "port must match when pattern carries port",
			origin:       "https://foo.com:8080",
			allowOrigins: []string{"https://foo.com:8080"},
			want:         true,
		},
		{
			name:         "port mismatch fails",
			origin:       "https://foo.com:8080",
			allowOrigins: []string{"https://foo.com:9090"},
			want:         false,
		},
		{
			name:         "pattern without port does not match origin with port",
			origin:       "https://foo.com:8080",
			allowOrigins: []string{"https://foo.com"},
			want:         false,
		},
		{
			name:         "wildcard host with explicit port",
			origin:       "https://www.bar.com:12345",
			allowOrigins: []string{"https://*.bar.com:12345"},
			want:         true,
		},
		{
			name:         "universal wildcard matches any port",
			origin:       "https://anything.com:9999",
			allowOrigins: []string{"*"},
			want:         true,
		},
		{
			name:         "Origin literal '*' is rejected (malformed request)",
			origin:       "*",
			allowOrigins: []string{"*"},
			want:         false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, originAllowed(tc.origin, tc.allowOrigins))
		})
	}
}

// TestIsCORSPreflight pins the preflight detection contract: OPTIONS method
// AND a non-empty Access-Control-Request-Method header → preflight; anything
// else → not a preflight.
func TestIsCORSPreflight(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		method        string
		requestMethod string
		want          bool
	}{
		{name: "OPTIONS + ACRM = preflight", method: http.MethodOptions, requestMethod: "GET", want: true},
		{name: "OPTIONS without ACRM is not a preflight", method: http.MethodOptions, want: false},
		{name: "GET with ACRM is not a preflight (browser quirk)", method: http.MethodGet, requestMethod: "GET", want: false},
		{name: "POST without ACRM is not a preflight", method: http.MethodPost, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := corsTestRequest(t, tc.method, "https://foo.com", tc.requestMethod, "")
			assert.Equal(t, tc.want, isCORSPreflight(req))
		})
	}
}

// TestCORSFilter_Preflight_AllowedOrigin pins the happy preflight path:
// matched Origin → 204 with Access-Control-Allow-Origin, ALlow-Methods,
// Allow-Headers, Max-Age headers. Credentials, expose, max-age all flow.
func TestCORSFilter_Preflight_AllowedOrigin(t *testing.T) {
	t.Parallel()

	cfg := &CORSConfig{
		AllowOrigins:     []string{"https://www.foo.com"},
		AllowMethods:     []string{"GET", "OPTIONS"},
		AllowHeaders:     []string{"x-header-1", "x-header-2"},
		ExposeHeaders:    []string{"x-header-3"},
		AllowCredentials: true,
		MaxAge:           3600,
	}

	f := NewCORSFilter(cfg)
	req := corsTestRequest(t, http.MethodOptions, "https://www.foo.com", "GET", "x-header-1, x-header-2")

	resp := f.ProcessRequest(req)
	require.NotNil(t, resp, "preflight from matched origin MUST short-circuit with a synthetic 204")

	defer resp.Body.Close()

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, "https://www.foo.com", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", resp.Header.Get("Access-Control-Allow-Credentials"))
	assert.Equal(t, "GET, OPTIONS", resp.Header.Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "x-header-1, x-header-2", resp.Header.Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "x-header-3", resp.Header.Get("Access-Control-Expose-Headers"))
	assert.Equal(t, "3600", resp.Header.Get("Access-Control-Max-Age"))
}

// TestCORSFilter_Preflight_NonMatchedOrigin pins the non-matching preflight
// path: 204 reply BUT no Access-Control-Allow-* headers. Browser then fails
// the cross-origin request on the client side.
func TestCORSFilter_Preflight_NonMatchedOrigin(t *testing.T) {
	t.Parallel()

	cfg := &CORSConfig{
		AllowOrigins: []string{"https://www.foo.com"},
		AllowMethods: []string{"GET"},
	}

	f := NewCORSFilter(cfg)
	req := corsTestRequest(t, http.MethodOptions, "https://attacker.com", "GET", "")

	resp := f.ProcessRequest(req)
	require.NotNil(t, resp, "preflight always short-circuits, even on origin mismatch")

	defer resp.Body.Close()

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"),
		"non-matched origin MUST NOT receive Access-Control-Allow-Origin")
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Methods"))
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Credentials"))
}

// TestCORSFilter_Preflight_WildcardMethodEchoesRequest verifies that
// AllowMethods=["*"] echoes back the requested method in the response header.
// This is the Envoy-compatible behaviour the Gateway API conformance tests
// expect.
func TestCORSFilter_Preflight_WildcardMethodEchoesRequest(t *testing.T) {
	t.Parallel()

	cfg := &CORSConfig{
		AllowOrigins: []string{"https://www.foo.com"},
		AllowMethods: []string{"*"},
	}

	f := NewCORSFilter(cfg)
	req := corsTestRequest(t, http.MethodOptions, "https://www.foo.com", "POST", "")

	resp := f.ProcessRequest(req)
	require.NotNil(t, resp)

	defer resp.Body.Close()

	allowMethods := resp.Header.Get("Access-Control-Allow-Methods")
	assert.Contains(t, []string{"POST", "*"}, allowMethods,
		"wildcard methods must echo the request method OR emit '*' (both spec-valid)")
}

// TestCORSFilter_Preflight_WildcardHeadersWithCredentialsHidesAuth verifies
// that when AllowHeaders=["*"] and AllowCredentials=false and the request
// carries no Access-Control-Request-Headers, the response can be "*" — but
// when AllowCredentials=true, "*" must NOT be emitted (it would expose
// credentials by accident).
func TestCORSFilter_Preflight_WildcardHeadersCredentialsBranching(t *testing.T) {
	t.Parallel()

	// Credentials path: no requested headers → MUST NOT emit "*".
	cfgCreds := &CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"*"},
		AllowHeaders:     []string{"*"},
		AllowCredentials: true,
	}

	req := corsTestRequest(t, http.MethodOptions, "https://other.foo.com", "PUT", "")
	resp := NewCORSFilter(cfgCreds).ProcessRequest(req)
	require.NotNil(t, resp)

	defer resp.Body.Close()

	assert.NotEqual(t, "*", resp.Header.Get("Access-Control-Allow-Headers"),
		"with credentials enabled and no request headers, Allow-Headers MUST NOT be '*'")
	assert.NotEqual(t, "*", resp.Header.Get("Access-Control-Allow-Origin"),
		"with credentials enabled, Allow-Origin MUST NOT be '*' even when AllowOrigins is wildcard")

	// Non-credentials path: same situation → "*" is permitted.
	cfgNoCreds := *cfgCreds
	cfgNoCreds.AllowCredentials = false

	respNoCreds := NewCORSFilter(&cfgNoCreds).ProcessRequest(req)
	require.NotNil(t, respNoCreds)

	defer respNoCreds.Body.Close()

	assert.Empty(t, respNoCreds.Header.Get("Access-Control-Allow-Credentials"),
		"credentials disabled → Access-Control-Allow-Credentials must NOT be set")
}

// TestCORSFilter_NonPreflight_PassesThrough verifies that a non-preflight
// request (no OPTIONS or no ACRM) is NOT short-circuited — the filter
// returns nil so the request proceeds to the backend; response headers are
// attached later by ProcessResponse.
func TestCORSFilter_NonPreflight_PassesThrough(t *testing.T) {
	t.Parallel()

	cfg := &CORSConfig{AllowOrigins: []string{"https://www.foo.com"}}
	f := NewCORSFilter(cfg)
	req := corsTestRequest(t, http.MethodGet, "https://www.foo.com", "", "")

	//nolint:bodyclose // ProcessRequest returns nil for non-preflight; nothing to close
	assert.Nil(t, f.ProcessRequest(req),
		"non-preflight requests MUST pass through ProcessRequest (nil return)")
}

// TestCORSFilter_NilConfig_NoOps pins the defensive contract: a CORSFilter
// built from a nil config no-ops on both request and response. This protects
// the proxy from a malformed config push.
func TestCORSFilter_NilConfig_NoOps(t *testing.T) {
	t.Parallel()

	f := NewCORSFilter(nil)
	preflight := corsTestRequest(t, http.MethodOptions, "https://www.foo.com", "GET", "")
	//nolint:bodyclose // nil cfg → nil response per contract; nothing to close
	assert.Nil(t, f.ProcessRequest(preflight))

	resp := corsTestResponse(corsTestRequest(t, http.MethodGet, "https://www.foo.com", "", ""))
	defer resp.Body.Close()

	f.ProcessResponse(resp)
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
}

// TestCORSFilter_SimpleRequest_AttachesAllowOrigin pins the simple-request
// path: GET with a matched Origin gets Access-Control-Allow-Origin attached
// to the backend response.
func TestCORSFilter_SimpleRequest_AttachesAllowOrigin(t *testing.T) {
	t.Parallel()

	cfg := &CORSConfig{
		AllowOrigins:     []string{"https://www.foo.com"},
		AllowCredentials: true,
		ExposeHeaders:    []string{"x-extra"},
	}

	f := NewCORSFilter(cfg)
	req := corsTestRequest(t, http.MethodGet, "https://www.foo.com", "", "")
	resp := corsTestResponse(req)
	defer resp.Body.Close()

	f.ProcessResponse(resp)

	assert.Equal(t, "https://www.foo.com", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", resp.Header.Get("Access-Control-Allow-Credentials"))
	assert.Equal(t, "x-extra", resp.Header.Get("Access-Control-Expose-Headers"))
}

// TestCORSFilter_SimpleRequest_NoOrigin_NoOps confirms that a backend
// response for a request without an Origin header doesn't gain CORS headers.
// (Same-origin requests behave exactly like they did before CORS was wired.)
func TestCORSFilter_SimpleRequest_NoOrigin_NoOps(t *testing.T) {
	t.Parallel()

	cfg := &CORSConfig{AllowOrigins: []string{"*"}}
	f := NewCORSFilter(cfg)
	req := corsTestRequest(t, http.MethodGet, "", "", "")
	resp := corsTestResponse(req)
	defer resp.Body.Close()

	f.ProcessResponse(resp)
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"),
		"request without an Origin header must NOT receive CORS response headers")
}

// TestCORSFilter_SimpleRequest_NonMatchedOrigin_NoOps confirms that a
// cross-origin request from a non-allowed origin gets NO CORS headers — the
// browser then silently fails the read.
func TestCORSFilter_SimpleRequest_NonMatchedOrigin_NoOps(t *testing.T) {
	t.Parallel()

	cfg := &CORSConfig{AllowOrigins: []string{"https://www.foo.com"}}
	f := NewCORSFilter(cfg)
	req := corsTestRequest(t, http.MethodGet, "https://attacker.com", "", "")
	resp := corsTestResponse(req)
	defer resp.Body.Close()

	f.ProcessResponse(resp)
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Credentials"))
}

// TestCORSFilter_ExposeHeadersWildcardCredentialsBranching pins the spec
// rule that `Access-Control-Expose-Headers: *` is forbidden when the request
// is credentialed. With AllowCredentials=true the proxy MUST omit the
// header entirely; with AllowCredentials=false the wildcard is allowed.
// Non-wildcard ExposeHeaders lists pass through verbatim under both.
func TestCORSFilter_ExposeHeadersWildcardCredentialsBranching(t *testing.T) {
	t.Parallel()

	// Wildcard + credentials: header MUST be omitted on both paths.
	credsWildcard := &CORSConfig{
		AllowOrigins:     []string{"https://www.foo.com"},
		AllowMethods:     []string{"GET"},
		ExposeHeaders:    []string{"*"},
		AllowCredentials: true,
	}

	preflight := corsTestRequest(t, http.MethodOptions, "https://www.foo.com", "GET", "")
	preflightResp := NewCORSFilter(credsWildcard).ProcessRequest(preflight)
	require.NotNil(t, preflightResp)

	defer preflightResp.Body.Close()

	assert.Empty(t, preflightResp.Header.Get("Access-Control-Expose-Headers"),
		"ExposeHeaders=['*'] with credentials enabled MUST omit the header (spec forbids '*' + credentials)")

	simpleReq := corsTestRequest(t, http.MethodGet, "https://www.foo.com", "", "")
	simpleResp := corsTestResponse(simpleReq)
	defer simpleResp.Body.Close()

	NewCORSFilter(credsWildcard).ProcessResponse(simpleResp)
	assert.Empty(t, simpleResp.Header.Get("Access-Control-Expose-Headers"),
		"ExposeHeaders=['*'] with credentials enabled MUST omit the header on simple responses too")

	// Wildcard, no credentials: header is permitted to carry "*".
	noCredsWildcard := *credsWildcard
	noCredsWildcard.AllowCredentials = false

	simpleResp2 := corsTestResponse(simpleReq)
	defer simpleResp2.Body.Close()

	NewCORSFilter(&noCredsWildcard).ProcessResponse(simpleResp2)
	assert.Equal(t, "*", simpleResp2.Header.Get("Access-Control-Expose-Headers"),
		"ExposeHeaders=['*'] WITHOUT credentials is spec-permitted")

	// Concrete list: pass through verbatim regardless of credentials flag.
	literal := &CORSConfig{
		AllowOrigins:     []string{"https://www.foo.com"},
		ExposeHeaders:    []string{"x-a", "x-b"},
		AllowCredentials: true,
	}

	simpleResp3 := corsTestResponse(simpleReq)
	defer simpleResp3.Body.Close()

	NewCORSFilter(literal).ProcessResponse(simpleResp3)
	assert.Equal(t, "x-a, x-b", simpleResp3.Header.Get("Access-Control-Expose-Headers"),
		"non-wildcard ExposeHeaders list MUST pass through, credentials flag irrelevant")
}

// TestCORSFilter_VaryHeaders pins the cache-safety contract: both preflight
// and simple responses MUST carry Vary: Origin (and the preflight reply
// MUST additionally carry Vary: Access-Control-Request-Method and Vary:
// Access-Control-Request-Headers). Without these a downstream cache could
// serve a previously-stamped Allow-Origin to a different request.
//
// Uses Add (not Set) so an upstream backend that already emitted Vary:
// Accept-Encoding survives — the test asserts both new and pre-existing
// values coexist on the simple-response path.
func TestCORSFilter_VaryHeaders(t *testing.T) {
	t.Parallel()

	cfg := &CORSConfig{
		AllowOrigins: []string{"https://www.foo.com"},
		AllowMethods: []string{"GET"},
	}

	// Preflight: three Vary entries on the synthetic 204.
	preflight := corsTestRequest(t, http.MethodOptions, "https://www.foo.com", "GET", "")
	preflightResp := NewCORSFilter(cfg).ProcessRequest(preflight)
	require.NotNil(t, preflightResp)

	defer preflightResp.Body.Close()

	vary := preflightResp.Header.Values("Vary")
	assert.Contains(t, vary, "Origin",
		"preflight reply MUST carry Vary: Origin (cache key must include Origin)")
	assert.Contains(t, vary, "Access-Control-Request-Method",
		"preflight reply MUST carry Vary: Access-Control-Request-Method")
	assert.Contains(t, vary, "Access-Control-Request-Headers",
		"preflight reply MUST carry Vary: Access-Control-Request-Headers")

	// Simple cross-origin: Vary: Origin appended to whatever the backend
	// already emitted.
	simpleReq := corsTestRequest(t, http.MethodGet, "https://www.foo.com", "", "")
	simpleResp := corsTestResponse(simpleReq)
	defer simpleResp.Body.Close()

	simpleResp.Header.Set("Vary", "Accept-Encoding") // pretend the backend emitted this

	NewCORSFilter(cfg).ProcessResponse(simpleResp)

	simpleVary := simpleResp.Header.Values("Vary")
	assert.Contains(t, simpleVary, "Accept-Encoding",
		"upstream-supplied Vary value MUST be preserved (use Add, not Set)")
	assert.Contains(t, simpleVary, "Origin",
		"simple cross-origin reply MUST carry Vary: Origin")
}

// TestCORSFilter_PreflightResponseNotDoubleStamped guards against the bug
// where ProcessResponse over-writes preflight headers a second time. The
// preflight is short-circuited at ProcessRequest, so ProcessResponse on the
// preflight request must be a no-op.
func TestCORSFilter_PreflightResponseNotDoubleStamped(t *testing.T) {
	t.Parallel()

	cfg := &CORSConfig{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{"*"},
	}

	f := NewCORSFilter(cfg)
	req := corsTestRequest(t, http.MethodOptions, "https://www.foo.com", "POST", "")
	// Suppose the synthetic preflight response loops back through
	// ProcessResponse for some reason — it must not double-write.
	resp := corsTestResponse(req)
	defer resp.Body.Close()

	resp.StatusCode = http.StatusNoContent
	resp.Header.Set("Access-Control-Allow-Origin", "https://www.foo.com")

	f.ProcessResponse(resp)
	assert.Equal(t, "https://www.foo.com", resp.Header.Get("Access-Control-Allow-Origin"),
		"ProcessResponse on a preflight request must not modify already-set CORS headers")
}
