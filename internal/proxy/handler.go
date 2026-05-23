package proxy

import (
	"context"
	"crypto/tls"
	"errors"
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

// transportKey forms the cache key for a backend transport. The protocol is
// part of the key so a protocol flip on the same host:port forces a fresh
// transport.
func transportKey(host string, protocol BackendProtocol) string {
	return host + "|" + string(protocol)
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

	proxy := h.createReverseProxy(backendURL, backend.Protocol, allFilters)
	proxy.ServeHTTP(writer, req)
}

// createReverseProxy builds an httputil.ReverseProxy for the given backend.
func (h *Handler) createReverseProxy(backendURL *url.URL, protocol BackendProtocol, filters []Filter) *httputil.ReverseProxy {
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
		Transport:    h.getTransport(backendURL.Host, protocol),
		ErrorHandler: errorHandler,
		ModifyResponse: func(resp *http.Response) error {
			ApplyResponseFilters(filters, resp)

			return nil
		},
	}
}

// getTransport returns a shared transport for the given backend host/protocol.
// The cache key includes the protocol so that flipping a Service's appProtocol
// (e.g. http -> h2c) does not silently reuse a stale HTTP/1.1 transport.
func (h *Handler) getTransport(host string, protocol BackendProtocol) http.RoundTripper {
	key := transportKey(host, protocol)

	if transport, ok := h.transports.Load(key); ok {
		if rt, castOK := transport.(http.RoundTripper); castOK {
			return rt
		}
	}

	transport := newTransport(protocol)
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

// newTransport builds a backend transport for the given protocol. For h2c it
// returns an HTTP/2 transport that negotiates cleartext via prior knowledge
// (AllowHTTP with a plaintext dialer); otherwise a clone of the default transport.
func newTransport(protocol BackendProtocol) http.RoundTripper {
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
