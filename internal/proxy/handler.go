package proxy

import (
	"context"
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

	// No backend available (e.g., redirect-only rule that didn't redirect).
	if result.BackendIdx < 0 || result.BackendIdx >= len(result.Rule.Backends) {
		http.Error(writer, "no backend available", http.StatusInternalServerError)

		return
	}

	// Proxy to backend.
	backend := result.Rule.Backends[result.BackendIdx]

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		http.Error(writer, "invalid backend URL", http.StatusInternalServerError)

		return
	}

	// Apply route timeouts to the request context.
	// Request timeout covers the entire transaction; Backend timeout covers the backend call.
	// Use the shorter of the two if both are set.
	if result.Rule.Timeouts != nil {
		timeout := result.Rule.Timeouts.Request
		if result.Rule.Timeouts.Backend > 0 && (timeout == 0 || result.Rule.Timeouts.Backend < timeout) {
			timeout = result.Rule.Timeouts.Backend
		}

		if timeout > 0 {
			ctx, cancel := context.WithTimeout(req.Context(), timeout)
			defer cancel()

			req = req.WithContext(ctx)
		}
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

// errorHandler handles proxy errors by returning 502 Bad Gateway.
func errorHandler(writer http.ResponseWriter, _ *http.Request, err error) {
	if err == nil {
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
