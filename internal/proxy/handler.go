package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

// Handler is the main HTTP handler for the L7 proxy.
// It routes requests, applies filters, and proxies to backends.
type Handler struct {
	router     *Router
	transports sync.Map // map[string]*http.Transport — per-backend transport pool
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

	// Apply pre-compiled request filters.
	redirectResp := ApplyRequestFilters(result.Filters, req)
	if redirectResp != nil {
		defer redirectResp.Body.Close()

		writeRedirectResponse(writer, redirectResp)

		return
	}

	h.proxyToBackend(writer, req, result)
}

// PruneTransports removes cached transports for hosts that are no longer active,
// closing their idle connections to prevent resource leaks.
func (h *Handler) PruneTransports(activeHosts map[string]bool) {
	h.transports.Range(func(key, value any) bool {
		host, ok := key.(string)
		if !ok {
			return true
		}

		if activeHosts[host] {
			return true
		}

		h.transports.Delete(host)

		if transport, castOK := value.(*http.Transport); castOK {
			transport.CloseIdleConnections()
		}

		return true
	})
}

// proxyToBackend selects the backend from the route result and proxies the request.
func (h *Handler) proxyToBackend(writer http.ResponseWriter, req *http.Request, result *RouteResult) {
	// No backend available — this happens for redirect-only rules where the
	// redirect filter did not fire (e.g., filter order issue or missing config).
	if result.BackendIdx < 0 || result.BackendIdx >= len(result.Rule.Backends) {
		http.Error(writer, "no backend configured for this route", http.StatusBadGateway)

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

	proxy := h.createReverseProxy(backendURL, result.Filters)
	proxy.ServeHTTP(writer, req)
}

// createReverseProxy builds an httputil.ReverseProxy for the given backend.
func (h *Handler) createReverseProxy(backendURL *url.URL, filters []Filter) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = backendURL.Scheme
			req.URL.Host = backendURL.Host
			req.Host = backendURL.Host

			if _, ok := req.Header["User-Agent"]; !ok {
				req.Header.Set("User-Agent", "")
			}
		},
		Transport:    h.getTransport(backendURL.Host),
		ErrorHandler: errorHandler,
		ModifyResponse: func(resp *http.Response) error {
			ApplyResponseFilters(filters, resp)

			return nil
		},
	}
}

// getTransport returns a shared transport for the given backend host.
func (h *Handler) getTransport(host string) http.RoundTripper {
	if transport, ok := h.transports.Load(host); ok {
		loadedTransport, castOK := transport.(*http.Transport)
		if castOK {
			return loadedTransport
		}
	}

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}

	transport := defaultTransport.Clone()
	actual, _ := h.transports.LoadOrStore(host, transport)

	loadedTransport, ok := actual.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}

	return loadedTransport
}

// errorHandler handles proxy errors with appropriate HTTP status codes.
// Returns 504 Gateway Timeout for deadline/cancellation errors, 502 Bad Gateway otherwise.
func errorHandler(writer http.ResponseWriter, _ *http.Request, err error) {
	if err == nil {
		return
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
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
