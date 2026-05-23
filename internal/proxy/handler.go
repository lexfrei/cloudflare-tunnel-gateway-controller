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

	"golang.org/x/net/http2"
)

// Handler is the main HTTP handler for the L7 proxy.
// It routes requests, applies filters, and proxies to backends.
type Handler struct {
	router *Router
	// transports caches one http.RoundTripper per backend, keyed by
	// host|protocol (see transportKey). Including the protocol in the key is
	// what makes flipping a Service's appProtocol (e.g. http -> h2c) take effect
	// without restarting the proxy: a stale HTTP/1.1 transport for the same
	// host can no longer mask an h2c reconfiguration.
	transports sync.Map // map[string]http.RoundTripper
}

// transportKey forms the cache key for a backend transport. Host, protocol
// AND TLS identity all participate so that a config push which adds, removes,
// or re-anchors a BackendTLSPolicy forces a fresh transport instead of reusing
// a stale one. tlsFingerprint hashes the CA + ServerName + SANs into a short
// stable string.
func transportKey(host string, protocol BackendProtocol, backendTLS *BackendTLSConfig) string {
	return host + "|" + string(protocol) + "|" + tlsFingerprint(backendTLS)
}

// tlsFingerprint returns a stable short hash of the TLS config, or "" when nil.
// The hash covers CA + ServerName + DNS SANs + URI SANs so any change to the
// effective trust policy evicts the cached transport.
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

	sum := hasher.Sum(nil)

	return hex.EncodeToString(sum[:8])
}

// NewHandler creates a new proxy Handler backed by the given Router.
func NewHandler(router *Router) *Handler {
	return &Handler{
		router: router,
	}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(writer http.ResponseWriter, req *http.Request) {
	result := h.router.Route(req)
	if result == nil {
		http.Error(writer, "no matching route", http.StatusNotFound)

		return
	}

	// Apply Request timeout early: it covers the entire handler (filters + backend call)
	// per Gateway API spec.
	if result.Rule.Timeouts != nil && result.Rule.Timeouts.Request > 0 {
		ctx, cancel := context.WithTimeout(req.Context(), result.Rule.Timeouts.Request)
		defer cancel()

		req = req.WithContext(ctx)
	}

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

// PruneTransports removes cached transports whose (host, protocol) key is no
// longer present in activeKeys, closing their idle connections to prevent
// resource leaks. Keys are formed by transportKey(host, protocol).
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

	// Apply Backend timeout: covers only the reverse proxy call to the upstream.
	if result.Rule.Timeouts != nil && result.Rule.Timeouts.Backend > 0 {
		ctx, cancel := context.WithTimeout(req.Context(), result.Rule.Timeouts.Backend)
		defer cancel()

		req = req.WithContext(ctx)
	}

	// Merge rule-level and backend-specific filters for response processing.
	// slices.Concat allocates a fresh slice; using append on result.Filters
	// would alias its backing array if cap > len and races against concurrent
	// requests reading the same compiled rule.
	allFilters := slices.Concat(result.Filters, result.BackendFilters)

	proxy := h.createReverseProxy(backendURL, backend.Protocol, backend.TLS, allFilters)
	proxy.ServeHTTP(writer, req)
}

// createReverseProxy builds an httputil.ReverseProxy for the given backend.
func (h *Handler) createReverseProxy(backendURL *url.URL, protocol BackendProtocol, backendTLS *BackendTLSConfig, filters []Filter) *httputil.ReverseProxy {
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
		Transport:    h.getTransport(backendURL.Host, protocol, backendTLS),
		ErrorHandler: errorHandler,
		ModifyResponse: func(resp *http.Response) error {
			ApplyResponseFilters(filters, resp)

			return nil
		},
	}
}

// getTransport returns a shared transport for the given backend host/protocol/TLS.
// The cache key includes protocol AND TLS identity so config flips that swap
// out either don't silently reuse a stale transport.
func (h *Handler) getTransport(host string, protocol BackendProtocol, backendTLS *BackendTLSConfig) http.RoundTripper {
	key := transportKey(host, protocol, backendTLS)

	if transport, ok := h.transports.Load(key); ok {
		if rt, castOK := transport.(http.RoundTripper); castOK {
			return rt
		}
	}

	transport := newTransport(protocol, backendTLS)
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
func newTransport(protocol BackendProtocol, backendTLS *BackendTLSConfig) http.RoundTripper {
	if backendTLS != nil {
		return newTLSTransport(backendTLS)
	}

	if protocol == BackendProtocolH2C {
		dialer := newH2CDialer()

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
		return defaultTransport.Clone()
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
func newTLSTransport(backendTLS *BackendTLSConfig) http.RoundTripper {
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

	return transport
}

// buildBackendTLSConfig assembles the *tls.Config for the two backend-TLS
// verification modes. Split from newTLSTransport for testability and to keep
// per-function complexity within the funlen budget.
func buildBackendTLSConfig(backendTLS *BackendTLSConfig, rootCAs *x509.CertPool) *tls.Config {
	if !backendTLS.HasSANConstraints() {
		// Mode 1: Hostname-based authentication via ServerName.
		return &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    rootCAs,
			ServerName: backendTLS.ServerName,
		}
	}

	// Mode 2: SAN-list authentication. ServerName drives SNI only.
	expectedHostnames := slices.Clone(backendTLS.SubjectAltNames)
	expectedURIs := slices.Clone(backendTLS.SubjectAltNameURIs)

	tlsConfig := &tls.Config{
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

	return tlsConfig
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
// Returns 504 Gateway Timeout for deadline/cancellation errors, 502 Bad Gateway otherwise.
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

	http.Error(writer, "bad gateway", http.StatusBadGateway)
}

// writeRedirectResponse writes a short-circuit redirect response.
func writeRedirectResponse(writer http.ResponseWriter, resp *http.Response) {
	for key, values := range resp.Header {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}

	writer.WriteHeader(resp.StatusCode)
}
