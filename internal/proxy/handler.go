package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sync"
	"time"

	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
)

// Handler is the main HTTP handler for the L7 proxy.
// It routes requests, applies filters, and proxies to backends.
type Handler struct {
	router *Router
	// transports caches one http.RoundTripper per backend, keyed by
	// host|protocol|tlsFingerprint|headerTimeout (see transportKey). Every
	// dimension in the key is load-bearing for cache correctness:
	//   - protocol: flipping a Service's appProtocol (e.g. http -> h2c)
	//     evicts the stale HTTP/1.1 transport instead of masking the
	//     reconfiguration with a cached entry.
	//   - tlsFingerprint: adding, removing, or re-anchoring a
	//     BackendTLSPolicy (or its client-cert keypair) forces a fresh
	//     transport whose TLS config and presented identity match.
	//   - headerTimeout: ResponseHeaderTimeout is a *http.Transport field
	//     and stdlib does not let us override it per-call, so two routes
	//     with different per-rule timeouts against the same backend must
	//     not share the cached transport, or one route silently inherits
	//     the other's deadline.
	transports sync.Map // map[string]http.RoundTripper

	// wsDialTimeout bounds proxyWebSocketUpgrade's per-attempt TCP/TLS
	// dial to the backend. Zero means "use defaultWSDialTimeout". Set
	// via WithWSDialTimeout at construction time.
	wsDialTimeout time.Duration
	// wsHandshakeReadTimeout bounds proxyWebSocketUpgrade's wait for the
	// backend's 101 Switching Protocols response. Zero means "use
	// defaultWSHandshakeReadTimeout". Set via WithWSHandshakeReadTimeout.
	wsHandshakeReadTimeout time.Duration
}

// HandlerOption configures a Handler at construction time. Use the
// With* helpers below to build option values; pass them to NewHandler.
type HandlerOption func(*Handler)

// WithWSDialTimeout overrides the default 30s WebSocket-backend dial
// timeout. Zero or negative values are ignored (the default stays in
// place); callers that want to disable the bound entirely should set
// a deliberately large value rather than zero.
func WithWSDialTimeout(d time.Duration) HandlerOption {
	return func(h *Handler) {
		if d > 0 {
			h.wsDialTimeout = d
		}
	}
}

// WithWSHandshakeReadTimeout overrides the default 30s WebSocket 101
// read deadline. Zero or negative values are ignored.
func WithWSHandshakeReadTimeout(d time.Duration) HandlerOption {
	return func(h *Handler) {
		if d > 0 {
			h.wsHandshakeReadTimeout = d
		}
	}
}

// ruleHeaderTimeout derives the *http.Transport.ResponseHeaderTimeout to
// apply for a route's per-rule timeouts. Both Gateway API knobs --
// timeouts.request (total) and timeouts.backendRequest (per-attempt) --
// collapse onto the same transport-level header-only timeout because this
// proxy has no retry logic; a single backend attempt is the whole request.
// When both knobs are set the stricter (min) value wins. When neither is
// set the helper returns 0, which leaves ResponseHeaderTimeout at its
// stdlib default (no timeout) -- callers can use the zero value as a
// "skip this knob" signal.
//
// Streaming-response contract: ResponseHeaderTimeout bounds only the
// time to receive response headers. Once headers arrive, the body
// streams freely. SSE / chunked / large-file / gRPC-server-streaming
// responses are no longer truncated at the timeout boundary, which
// fixes the per-rule-timeouts-kill-streaming-responses bug. Tests
// TestHandler_StreamingResponseSurvivesRequestTimeout and
// TestHandler_StreamingResponseSurvivesBackendTimeout pin the
// behaviour against regression.
func ruleHeaderTimeout(timeouts *RouteTimeouts) time.Duration {
	if timeouts == nil {
		return 0
	}

	switch {
	case timeouts.Request > 0 && timeouts.Backend > 0:
		return min(timeouts.Request, timeouts.Backend)
	case timeouts.Request > 0:
		return timeouts.Request
	case timeouts.Backend > 0:
		return timeouts.Backend
	default:
		return 0
	}
}

// transportKey forms the cache key for a backend transport. Host, protocol,
// TLS identity, AND the response-header timeout all participate so that a
// config push which adds, removes, or re-anchors a BackendTLSPolicy -- or
// changes the rule's per-route timeouts -- forces a fresh transport instead
// of reusing a stale one. tlsFingerprint hashes the CA + ServerName + SANs
// into a short stable string. The header timeout participates because it is
// set on the *http.Transport itself (ResponseHeaderTimeout) and stdlib does
// not let us override it per-call; without including it in the key, a route
// reconfigured from `Request: 5s` to `Request: 30s` would silently keep
// using the cached 5s transport.
func transportKey(host string, protocol BackendProtocol, backendTLS *BackendTLSConfig, headerTimeout time.Duration) string {
	return host + "|" + string(protocol) + "|" + tlsFingerprint(backendTLS) + "|" + headerTimeout.String()
}

// tlsFingerprint returns a stable short hash of the TLS config, or "" when nil.
// The hash covers CA + ServerName + DNS SANs + URI SANs + client keypair so
// any change to the effective trust policy or the presented client identity
// evicts the cached transport. The client cert section is hashed last so
// existing keys with no client cert keep their byte layout intact (the
// trailing separator + empty payload is collision-free with prior URI SANs).
func tlsFingerprint(backendTLS *BackendTLSConfig) string {
	if backendTLS == nil {
		return ""
	}

	hasher := sha256.New()
	hasher.Write([]byte(backendTLS.CABundlePEM))
	hasher.Write([]byte{0})
	hasher.Write([]byte(backendTLS.ServerName))
	hasher.Write([]byte{0})

	for _, san := range backendTLS.SubjectAltNames {
		hasher.Write([]byte(san))
		hasher.Write([]byte{0})
	}
	// Use a distinct separator between DNS and URI SAN sections so a config
	// with DNS "foo" + URI "" can't collide with one carrying URI "foo" + DNS "".
	hasher.Write([]byte("|uri|"))

	for _, sanURI := range backendTLS.SubjectAltNameURIs {
		hasher.Write([]byte(sanURI))
		hasher.Write([]byte{0})
	}
	// Distinct separator before the client keypair so two Gateways serving
	// the same backend host with different client certs never share a
	// transport (each would otherwise present the cached transport's cert).
	hasher.Write([]byte("|client|"))
	hasher.Write(backendTLS.ClientCertPEM)
	hasher.Write([]byte{0})
	hasher.Write(backendTLS.ClientKeyPEM)

	sum := hasher.Sum(nil)

	return hex.EncodeToString(sum[:8])
}

// NewHandler creates a new proxy Handler backed by the given Router.
// Optional HandlerOption arguments configure per-Handler knobs (WebSocket
// dial / handshake timeouts, etc.); zero options leaves the package
// defaults in place.
func NewHandler(router *Router, opts ...HandlerOption) *Handler {
	handler := &Handler{
		router: router,
	}

	for _, opt := range opts {
		opt(handler)
	}

	return handler
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(writer http.ResponseWriter, req *http.Request) {
	result := h.router.Route(req)
	if result == nil {
		http.Error(writer, "no matching route", http.StatusNotFound)

		return
	}

	// Per-rule timeouts (Request / BackendRequest) are enforced
	// downstream by the cached transport's ResponseHeaderTimeout
	// (set in newTransport from ruleHeaderTimeout's collapse). No
	// context.WithTimeout wrapping here -- that would cancel the
	// body read and truncate streaming responses. See ruleHeaderTimeout
	// for the full rationale.

	// Store matched prefix in request context for URL rewrite filters.
	if result.MatchedPrefix != "" {
		req = req.WithContext(context.WithValue(req.Context(), matchedPrefixKey{}, result.MatchedPrefix))
	}

	// Apply pre-compiled rule-level request filters.
	redirectResp := ApplyRequestFilters(result.Filters, req)
	if redirectResp != nil {
		defer redirectResp.Body.Close()

		writeRedirectResponse(writer, redirectResp)

		return
	}

	// Apply backend-specific filters (e.g., per-backend header modifiers).
	if len(result.BackendFilters) > 0 {
		redirectResp = ApplyRequestFilters(result.BackendFilters, req)
		if redirectResp != nil {
			defer redirectResp.Body.Close()

			writeRedirectResponse(writer, redirectResp)

			return
		}
	}

	h.proxyToBackend(writer, req, result)
}

// PruneTransports removes cached transports whose composite key
// (host|protocol|tlsFingerprint|headerTimeout) is no longer present in
// activeKeys, closing their idle connections to prevent resource
// leaks. Keys are formed by transportKey; activeKeys is derived from
// the current config by extractActiveTransportKeys, which mirrors the
// exact key composition used by getTransport so a config change in
// any of those four dimensions cleanly evicts the stale entry.
func (h *Handler) PruneTransports(activeKeys map[string]bool) {
	h.transports.Range(func(rawKey, value any) bool {
		key, ok := rawKey.(string)
		if !ok {
			return true
		}

		if activeKeys[key] {
			return true
		}

		h.transports.Delete(key)

		// Both *http.Transport and *http2.Transport expose CloseIdleConnections.
		if closer, castOK := value.(interface{ CloseIdleConnections() }); castOK {
			closer.CloseIdleConnections()
		}

		return true
	})
}

// effectiveWSDialTimeout returns the configured WebSocket-backend dial
// timeout or the package default when none was set. Method (not field
// access) so the zero-value fallback lives next to the field rather
// than at every call site.
func (h *Handler) effectiveWSDialTimeout() time.Duration {
	if h.wsDialTimeout > 0 {
		return h.wsDialTimeout
	}

	return defaultWSDialTimeout
}

// effectiveWSHandshakeReadTimeout returns the configured 101-read
// deadline or the package default when none was set.
func (h *Handler) effectiveWSHandshakeReadTimeout() time.Duration {
	if h.wsHandshakeReadTimeout > 0 {
		return h.wsHandshakeReadTimeout
	}

	return defaultWSHandshakeReadTimeout
}

// proxyToBackend selects the backend from the route result and proxies the request.
func (h *Handler) proxyToBackend(writer http.ResponseWriter, req *http.Request, result *RouteResult) {
	// No backend available — this happens when all backend refs are invalid
	// (unsupported Kind, missing ReferenceGrant, non-existent Service),
	// when all backends have zero weight, or for redirect-only rules where
	// the redirect filter did not fire.
	// Per Gateway API spec: return 500 when backend refs cannot be resolved.
	if result.BackendIdx < 0 || result.BackendIdx >= len(result.Rule.Backends) {
		if len(result.Rule.Backends) > 0 {
			slog.Warn("all backends have zero weight; no traffic routed per Gateway API spec",
				slog.Int("backend_count", len(result.Rule.Backends)))
		}

		http.Error(writer, "no backend available for this route", http.StatusInternalServerError)

		return
	}

	backend := result.Rule.Backends[result.BackendIdx]

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		http.Error(writer, "invalid backend URL", http.StatusInternalServerError)

		return
	}

	// Merge rule-level and backend-specific filters for response processing.
	// slices.Concat allocates a fresh slice; using append on result.Filters
	// would alias its backing array if cap > len and races against concurrent
	// requests reading the same compiled rule. Computed once and shared
	// between the WS upgrade and non-upgrade branches so both paths apply
	// the same ResponseFilter pipeline, consistent with the non-upgrade
	// path's httputil.ReverseProxy.ModifyResponse callback.
	allFilters := slices.Concat(result.Filters, result.BackendFilters)

	// WebSocket upgrade: bypass httputil.ReverseProxy. ReverseProxy's
	// handleUpgradeResponse calls Hijack() on the writer BEFORE writing
	// the 101 status, then writes the raw HTTP/1.1 status line onto the
	// hijacked conn. That fails over cloudflared's HTTP/2 transport
	// because http2RespWriter.Hijack requires statusWritten=true and the
	// raw HTTP/1.1 bytes don't translate into HTTP/2 DATA frames the edge
	// can re-frame back to a client 101 — the client sees 403 / 502
	// instead of a successful upgrade. proxyWebSocketUpgrade does the
	// dial/handshake/hijack/pipe sequence directly; see the comment on
	// that method for the protocol-level details.
	if shouldUseWebSocketUpgradePath(req, backend.WebSocket) {
		h.proxyWebSocketUpgrade(writer, req, backendURL, backend.TLS, allFilters)

		return
	}

	// Per-rule Backend timeout flows into the transport's
	// ResponseHeaderTimeout (alongside the Request timeout, collapsed
	// via ruleHeaderTimeout). The header-only timing isolates the
	// header phase from the body phase, so streaming responses are
	// not killed at the timeout boundary. See ServeHTTP's per-rule
	// timeout comment for the rationale.
	headerTimeout := ruleHeaderTimeout(result.Rule.Timeouts)

	proxy := h.createReverseProxy(backendURL, backend.Protocol, backend.TLS, allFilters, headerTimeout)
	proxy.ServeHTTP(writer, req)
}

// createReverseProxy builds an httputil.ReverseProxy for the given backend.
// headerTimeout (the rule-derived response-header deadline; see
// ruleHeaderTimeout) becomes part of the transport cache key and the
// resulting transport's ResponseHeaderTimeout. Zero means "no header
// deadline".
func (h *Handler) createReverseProxy(backendURL *url.URL, protocol BackendProtocol, backendTLS *BackendTLSConfig, filters []Filter, headerTimeout time.Duration) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = backendURL.Scheme
			req.URL.Host = backendURL.Host

			// Preserve the original Host header per Gateway API spec.
			// Only override it if a URLRewrite filter explicitly set a new host.
			hostRewritten := isHostRewritten(req)
			if hostRewritten {
				req.Header.Del(hostRewrittenHeader)
			}

			// When X-Original-Host is present (set by TunnelRoundTripper to
			// bypass Cloudflare edge Host validation), restore it as the
			// request Host so the backend sees the intended hostname.
			// Skip restoration if a URL rewrite filter has explicitly set
			// a new host — the filter's host takes precedence.
			if !hostRewritten {
				if origHost := req.Header.Get("X-Original-Host"); origHost != "" {
					req.Host = origHost
				}
			}

			req.Header.Del("X-Original-Host")

			if _, ok := req.Header["User-Agent"]; !ok {
				req.Header.Set("User-Agent", "")
			}
		},
		Transport:    h.getTransport(backendURL.Host, protocol, backendTLS, headerTimeout),
		ErrorHandler: errorHandler,
		ModifyResponse: func(resp *http.Response) error {
			ApplyResponseFilters(filters, resp)

			return nil
		},
	}
}

// getTransport returns a shared transport for the given backend host /
// protocol / TLS / header-timeout tuple. The cache key includes the
// header timeout because ResponseHeaderTimeout is a *http.Transport
// field, not per-call -- two routes with different per-rule timeouts
// against the same backend must NOT share the cached transport, or one
// route silently inherits the other's deadline.
func (h *Handler) getTransport(host string, protocol BackendProtocol, backendTLS *BackendTLSConfig, headerTimeout time.Duration) http.RoundTripper {
	key := transportKey(host, protocol, backendTLS, headerTimeout)

	if transport, ok := h.transports.Load(key); ok {
		if rt, castOK := transport.(http.RoundTripper); castOK {
			return rt
		}
	}

	transport := newTransport(protocol, backendTLS, headerTimeout)
	actual, _ := h.transports.LoadOrStore(key, transport)

	if rt, ok := actual.(http.RoundTripper); ok {
		return rt
	}

	return transport
}

// h2cReadIdleTimeout sends an HTTP/2 PING on the multiplexed connection after
// this much idle time so a dead TCP connection (NodePort flap, kube-proxy
// churn, NAT timeout) gets evicted instead of blocking new requests.
const h2cReadIdleTimeout = 30 * time.Second

// h2cPingTimeout bounds how long the transport waits for a PING ACK before
// declaring the connection dead and closing it.
const h2cPingTimeout = 15 * time.Second

// h2cDialTimeout caps the time spent on a single TCP SYN to an h2c backend.
// Without this, a SYN against a gone pod hangs on kernel TCP defaults
// (often >1 min), stalling the request goroutine well past any sensible
// request budget. The value mirrors http.DefaultTransport's dialer.
const h2cDialTimeout = 30 * time.Second

// h2cDialKeepAlive matches http.DefaultTransport's dialer KeepAlive so TCP
// keepalives evict half-closed connections from the pool.
const h2cDialKeepAlive = 30 * time.Second

// newH2CDialer constructs the net.Dialer used for h2c backend connections.
// Exported indirectly via export_test.go so tests can assert the timeout fields.
func newH2CDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   h2cDialTimeout,
		KeepAlive: h2cDialKeepAlive,
	}
}

// newTransport builds a backend transport for the given protocol and optional
// TLS policy. Precedence: when backendTLS is set, the proxy dials TLS (HTTPS,
// with HTTP/2 auto-negotiated via ALPN) — h2c (cleartext) cannot coexist with
// TLS, so the protocol marker is intentionally ignored in that path. Otherwise
// h2c uses an HTTP/2 plaintext transport; default is a clone of the stdlib
// transport.
//
// headerTimeout (zero = unbounded) flows into the resulting *http.Transport
// as ResponseHeaderTimeout for the cleartext and TLS paths. The h2c
// path uses x/net/http2.Transport which does not expose an equivalent
// knob; per-rule timeouts therefore do not bound the header phase on
// h2c backends today (tracked as a follow-up). The pre-stream conn
// dial is still bounded by the h2c dialer's Timeout, so a fully dead
// backend still fails fast.
func newTransport(protocol BackendProtocol, backendTLS *BackendTLSConfig, headerTimeout time.Duration) http.RoundTripper {
	if backendTLS != nil {
		return newTLSTransport(backendTLS, headerTimeout)
	}

	if protocol == BackendProtocolH2C {
		dialer := newH2CDialer()

		//nolint:godox // FIXME(#270) carries an issue-tracker reference and is the project's tracked-follow-up pattern
		// FIXME(#270): per-rule headerTimeout is intentionally NOT
		// applied here -- golang.org/x/net/http2.Transport has no
		// ResponseHeaderTimeout-equivalent knob, so SSE / chunked /
		// gRPC streaming bodies over h2c are already safe from the
		// streaming-truncation bug this fix addresses for the
		// stdlib path. The cost is that a slow-to-respond h2c
		// backend is bounded only by the dialer's connect timeout,
		// not by the per-rule header deadline. Issue #270 tracks
		// the longer-term fix.
		return &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
			},
			ReadIdleTimeout: h2cReadIdleTimeout,
			PingTimeout:     h2cPingTimeout,
		}
	}

	if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := defaultTransport.Clone()
		cloned.ResponseHeaderTimeout = headerTimeout

		return cloned
	}

	return http.DefaultTransport
}

// Sentinel errors for backend TLS verification so wrappers can be matched with errors.Is.
var (
	errBackendTLSNoPeerCert  = errors.New("backend tls: no peer certificates presented")
	errBackendTLSSANMissing  = errors.New("backend tls: required SAN not present in cert SANs")
	errBackendTLSChainVerify = errors.New("backend tls: chain verification failed")
)

// newTLSTransport wires a BackendTLSConfig into an http.Transport that
// validates the backend cert chain against the policy's CA bundle.
//
// Two verification modes, per Gateway API BackendTLSPolicy spec:
//
//   - SubjectAltNames empty: stdlib hostname verification against ServerName
//     (i.e. policy Hostname is the authentication identity). ServerName is
//     also used as SNI.
//   - SubjectAltNames non-empty: Hostname is used ONLY for SNI/selection, NOT
//     authentication. We disable the default ServerName-vs-cert match and
//     manually verify the chain + OR-match the leaf against the policy's SAN
//     list using x509.VerifyHostname so RFC 6125 wildcards work.
//
// In both modes a CA pool that fails to parse any PEM block is treated as a
// hard failure so misconfigured operators see a TLS handshake error instead of
// silently trusting nothing (gosec G402-safe path).
func newTLSTransport(backendTLS *BackendTLSConfig, headerTimeout time.Duration) http.RoundTripper {
	rootCAs := x509.NewCertPool()
	if ok := rootCAs.AppendCertsFromPEM([]byte(backendTLS.CABundlePEM)); !ok {
		slog.Error("BackendTLSPolicy CA bundle did not parse — all backend TLS handshakes will fail",
			"serverName", backendTLS.ServerName,
		)
	}

	tlsConfig := buildBackendTLSConfig(backendTLS, rootCAs)

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// stdlib's DefaultTransport is always *http.Transport; fall back is
		// defensive only.
		return http.DefaultTransport
	}

	transport := base.Clone()
	transport.TLSClientConfig = tlsConfig
	transport.ResponseHeaderTimeout = headerTimeout

	return transport
}

// buildBackendTLSConfig assembles the *tls.Config for the two backend-TLS
// verification modes. Split from newTLSTransport for testability and to keep
// per-function complexity within the funlen budget.
func buildBackendTLSConfig(backendTLS *BackendTLSConfig, rootCAs *x509.CertPool) *tls.Config {
	var tlsConfig *tls.Config

	if !backendTLS.HasSANConstraints() {
		// Mode 1: Hostname-based authentication via ServerName.
		tlsConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    rootCAs,
			ServerName: backendTLS.ServerName,
		}
	} else {
		// Mode 2: SAN-list authentication. ServerName drives SNI only.
		expectedHostnames := slices.Clone(backendTLS.SubjectAltNames)
		expectedURIs := slices.Clone(backendTLS.SubjectAltNameURIs)
		tlsConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    rootCAs,
			ServerName: backendTLS.ServerName,
			// Hostname matching is intentionally bypassed — VerifyConnection
			// performs full chain validation AND the SAN OR-match below.
			// gosec G402: this is the documented Gateway API SAN-only verification
			// path; chain validation is preserved via leaf.Verify().
			InsecureSkipVerify: true, //nolint:gosec // see comment above
			VerifyConnection:   verifyBackendChainAndSANs(rootCAs, expectedHostnames, expectedURIs),
		}
	}

	attachClientCert(tlsConfig, backendTLS)

	return tlsConfig
}

// attachClientCert loads the Gateway-level client keypair into tlsConfig so
// the proxy can present it during the backend TLS handshake (mTLS). A parse
// failure is logged and the cert is left unattached so the handshake fails
// closed — a server that requires a client cert will reject the connection
// rather than silently fall back to one-way authentication. tls.X509KeyPair
// accepts a PEM chain (leaf + intermediates) in ClientCertPEM and pairs it
// with the single private key in ClientKeyPEM.
func attachClientCert(tlsConfig *tls.Config, backendTLS *BackendTLSConfig) {
	if !backendTLS.HasClientCert() {
		return
	}

	cert, err := tls.X509KeyPair(backendTLS.ClientCertPEM, backendTLS.ClientKeyPEM)
	if err != nil {
		slog.Error("backend client certificate keypair failed to parse — handshake will fail closed",
			"error", err,
			"serverName", backendTLS.ServerName,
		)

		return
	}

	tlsConfig.Certificates = []tls.Certificate{cert}
}

// verifyBackendChainAndSANs returns a VerifyConnection callback that runs both
// on fresh and resumed handshakes (per gosec G123). It manually verifies the
// chain against rootCAs (since InsecureSkipVerify=true disables the default
// path) and asserts the leaf cert matches at least one of the expected SANs.
// DNS SANs honour RFC 6125 wildcards via x509.Certificate.VerifyHostname;
// URI SANs are matched by exact string equality against leaf.URIs per the
// SPIFFE convention used by the Gateway API conformance suite.
func verifyBackendChainAndSANs(
	rootCAs *x509.CertPool,
	expectedHostnames []string,
	expectedURIs []string,
) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errBackendTLSNoPeerCert
		}

		leaf := state.PeerCertificates[0]

		intermediates := x509.NewCertPool()
		for _, intermediate := range state.PeerCertificates[1:] {
			intermediates.AddCert(intermediate)
		}

		// DNSName intentionally empty: hostname auth happens via the SAN
		// OR-match below per BackendTLSPolicy semantics.
		_, verifyErr := leaf.Verify(x509.VerifyOptions{
			Roots:         rootCAs,
			Intermediates: intermediates,
		})
		if verifyErr != nil {
			return fmt.Errorf("%w: %w", errBackendTLSChainVerify, verifyErr)
		}

		if matchAnyDNSSan(leaf, expectedHostnames) {
			return nil
		}

		if matchAnyURISan(leaf, expectedURIs) {
			return nil
		}

		certURIs := make([]string, 0, len(leaf.URIs))
		for _, certURI := range leaf.URIs {
			certURIs = append(certURIs, certURI.String())
		}

		return fmt.Errorf("%w: required one of DNS%v / URI%v, cert DNS%v / cert URIs%v",
			errBackendTLSSANMissing, expectedHostnames, expectedURIs, leaf.DNSNames, certURIs)
	}
}

// matchAnyDNSSan reports whether the leaf cert satisfies at least one of the
// expected DNS SAN values via x509.Certificate.VerifyHostname (RFC 6125,
// wildcard-aware). Empty expected list reports false.
func matchAnyDNSSan(leaf *x509.Certificate, expected []string) bool {
	for _, want := range expected {
		hostErr := leaf.VerifyHostname(want)
		if hostErr == nil {
			return true
		}
	}

	return false
}

// matchAnyURISan reports whether the leaf cert presents at least one URI SAN
// (as carried in leaf.URIs) that exactly matches one of the expected URI
// strings. Empty expected list reports false. Matching is plain string
// equality on the URI's canonical String() form — this is the convention
// used by SPIFFE and the Gateway API conformance suite.
func matchAnyURISan(leaf *x509.Certificate, expected []string) bool {
	if len(expected) == 0 {
		return false
	}

	for _, certURI := range leaf.URIs {
		if slices.Contains(expected, certURI.String()) {
			return true
		}
	}

	return false
}

// errorHandler handles proxy errors with appropriate HTTP status codes.
// Returns 504 Gateway Timeout for context-deadline AND transport-level
// header-timeout errors (the latter is how ResponseHeaderTimeout surfaces
// when a slow backend doesn't send response headers in time -- the
// returned error is not a wrapped context.DeadlineExceeded, just an
// internal *timeoutError that satisfies a Timeout() bool interface).
// Returns 502 Bad Gateway otherwise.
func errorHandler(writer http.ResponseWriter, _ *http.Request, err error) {
	if err == nil {
		return
	}

	if errors.Is(err, context.Canceled) {
		// Client disconnected — no point writing a response.
		return
	}

	if errors.Is(err, context.DeadlineExceeded) {
		http.Error(writer, "gateway timeout", http.StatusGatewayTimeout)

		return
	}

	// http.Transport.ResponseHeaderTimeout returns a sentinel that
	// satisfies the Timeout() bool method but is NOT a wrapped
	// context.DeadlineExceeded -- check the interface explicitly so a
	// header-timeout fires 504 just like a request-context deadline.
	var timeoutErr interface{ Timeout() bool }
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		http.Error(writer, "gateway timeout", http.StatusGatewayTimeout)

		return
	}

	http.Error(writer, "bad gateway", http.StatusBadGateway)
}

// shouldUseWebSocketUpgradePath combines the two predicates that
// decide whether a request goes through the dedicated
// proxyWebSocketUpgrade path instead of httputil.ReverseProxy: the
// client must actually be attempting an HTTP/1.1 upgrade AND the
// selected backend must have been declared WebSocket-capable by the
// operator via `appProtocol: kubernetes.io/ws[s]`. Gating on the
// client header alone would let any request hijack non-WS routes;
// gating on the operator declaration alone would force plain HTTP
// requests on a WS-marked route through the upgrade path.
//
// The function was historically named shouldSkipUpgradeTimeout
// because it ALSO governed a context.WithTimeout skip in the handler
// (per-rule deadlines used to be wrapped around the whole request,
// which broke WebSockets). That skip was removed when per-rule
// timeouts moved to *http.Transport.ResponseHeaderTimeout -- the
// upgrade path bypasses the cached transport entirely now, so no
// explicit skip is needed. The predicate's only remaining job is
// upgrade-path selection.
func shouldUseWebSocketUpgradePath(req *http.Request, operatorAllowsUpgrade bool) bool {
	return operatorAllowsUpgrade && isHTTPUpgradeRequest(req)
}

// isHTTPUpgradeRequest reports whether the request is an HTTP/1.1 upgrade
// per RFC 7230 §6.1 — a non-empty Upgrade header AND a Connection header
// containing the "upgrade" token. WebSocket is the most common case but the
// same predicate covers any upgrade (HTTP/2 prior knowledge over h2c
// originally negotiated by Upgrade, etc.).
//
// Used together with `BackendRef.WebSocket` to gate the upgrade-path
// selection: a client-controlled header alone is not enough — the
// operator must also have marked the relevant backend as WS-capable.
//
// Connection header parsing delegates to httpguts.HeaderValuesContainsToken
// — the same helper stdlib's net/http uses internally for the same RFC 7230
// token rules — instead of a hand-rolled SplitSeq + EqualFold loop. Single
// source of truth: any future tightening of RFC 7230 token parsing
// (whitespace, quoted-string handling, etc.) lands in httpguts and we
// inherit it for free.
func isHTTPUpgradeRequest(req *http.Request) bool {
	if req == nil || req.Header == nil {
		return false
	}

	if req.Header.Get("Upgrade") == "" {
		return false
	}

	return httpguts.HeaderValuesContainsToken(req.Header.Values("Connection"), "upgrade")
}

// writeRedirectResponse writes a short-circuit redirect response.
func writeRedirectResponse(writer http.ResponseWriter, resp *http.Response) {
	copyHeaderValues(writer.Header(), resp.Header)
	writer.WriteHeader(resp.StatusCode)
}

// copyHeaderValues performs a shallow copy of src into dst, preserving
// multi-value headers. Single shared helper for the package — used by
// both the redirect short-circuit path here and the WebSocket upgrade
// path in handler_websocket.go. Mirrors stdlib's internal `copyHeader`
// (`net/http/httputil/reverseproxy.go`) without taking a `httputil`
// import dependency in this file.
func copyHeaderValues(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
