package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

// mirrorTimeout is the maximum time to wait for a mirror request.
const mirrorTimeout = 5 * time.Second

// mirrorClient is a shared HTTP client for all mirror filter instances.
// Using a single client ensures connection pooling across mirror filters.
//
//nolint:gochecknoglobals // shared client avoids unbounded transport pool creation
var mirrorClient = &http.Client{
	Timeout: mirrorTimeout,
}

// maxMirrorBodySize is the maximum request body size that will be buffered
// for mirroring. Bodies exceeding this limit cause mirroring to be skipped
// to avoid excessive memory usage.
const maxMirrorBodySize = 1 << 20 // 1 MiB

// matchedPrefixKey is the context key for storing the matched path prefix.
type matchedPrefixKey struct{}

// hostRewrittenHeader is an internal header set by URL rewrite filters
// to signal the Director not to overwrite the Host header.
// It is removed before the request is sent to the backend.
const hostRewrittenHeader = "X-Proxy-Host-Rewritten"

// SetMatchedPrefix returns a shallow copy of req with the matched path prefix
// stored in its context. The original request is NOT modified.
// Used by URL rewrite filters for ReplacePrefixMatch.
func SetMatchedPrefix(req *http.Request, prefix string) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), matchedPrefixKey{}, prefix))
}

// getMatchedPrefix retrieves the matched path prefix from the request context.
func getMatchedPrefix(req *http.Request) string {
	prefix, _ := req.Context().Value(matchedPrefixKey{}).(string)

	return prefix
}

// isHostRewritten returns true if a filter has rewritten the Host header.
func isHostRewritten(req *http.Request) bool {
	return req.Header.Get(hostRewrittenHeader) != ""
}

// Filter defines a transformation applied to matching requests or responses.
type Filter interface {
	// ProcessRequest modifies the request. Returns non-nil *http.Response to short-circuit
	// (e.g., for redirects). Returns nil to continue processing.
	ProcessRequest(req *http.Request) *http.Response

	// ProcessResponse modifies the response headers after proxying.
	ProcessResponse(resp *http.Response)
}

// requestHeaderModifier adds, sets, or removes request headers.
type requestHeaderModifier struct {
	modifier *HeaderModifier
}

// NewRequestHeaderModifier creates a filter that modifies request headers.
func NewRequestHeaderModifier(modifier *HeaderModifier) Filter {
	return &requestHeaderModifier{modifier: modifier}
}

func (f *requestHeaderModifier) ProcessRequest(req *http.Request) *http.Response {
	applyHeaderModifier(req.Header, f.modifier)

	return nil
}

func (f *requestHeaderModifier) ProcessResponse(_ *http.Response) {}

// responseHeaderModifier adds, sets, or removes response headers.
type responseHeaderModifier struct {
	modifier *HeaderModifier
}

// NewResponseHeaderModifier creates a filter that modifies response headers.
func NewResponseHeaderModifier(modifier *HeaderModifier) Filter {
	return &responseHeaderModifier{modifier: modifier}
}

func (f *responseHeaderModifier) ProcessRequest(_ *http.Request) *http.Response {
	return nil
}

func (f *responseHeaderModifier) ProcessResponse(resp *http.Response) {
	applyHeaderModifier(resp.Header, f.modifier)
}

// applyHeaderModifier applies set/add/remove operations to headers.
func applyHeaderModifier(headers http.Header, modifier *HeaderModifier) {
	for _, headerVal := range modifier.Set {
		headers.Set(headerVal.Name, headerVal.Value)
	}

	for _, headerVal := range modifier.Add {
		headers.Add(headerVal.Name, headerVal.Value)
	}

	for _, name := range modifier.Remove {
		headers.Del(name)
	}
}

// requestRedirect returns a redirect response, short-circuiting further processing.
type requestRedirect struct {
	config *RedirectConfig
}

// NewRequestRedirect creates a filter that returns an HTTP redirect response.
func NewRequestRedirect(config *RedirectConfig) Filter {
	return &requestRedirect{config: config}
}

func (f *requestRedirect) ProcessRequest(req *http.Request) *http.Response {
	location := f.buildRedirectURL(req)

	statusCode := http.StatusFound
	if f.config.StatusCode != nil {
		statusCode = *f.config.StatusCode
	}

	return &http.Response{
		StatusCode: statusCode,
		Header:     http.Header{"Location": {location}},
		Body:       http.NoBody,
	}
}

func (f *requestRedirect) ProcessResponse(_ *http.Response) {}

func (f *requestRedirect) buildRedirectURL(req *http.Request) string {
	scheme := req.URL.Scheme
	if scheme == "" {
		scheme = "https"
	}

	if f.config.Scheme != nil {
		scheme = *f.config.Scheme
	}

	hostname := req.Host
	if f.config.Hostname != nil {
		hostname = *f.config.Hostname
	}

	// Strip any existing port from hostname for clean URL construction.
	if idx := strings.LastIndex(hostname, ":"); idx != -1 {
		if !strings.Contains(hostname[idx:], "]") {
			hostname = hostname[:idx]
		}
	}

	path := req.URL.Path
	if f.config.Path != nil {
		switch f.config.Path.Type {
		case RedirectPathFullReplace:
			path = f.config.Path.Value
		case RedirectPathPrefixReplace:
			matchedPrefix := getMatchedPrefix(req)
			if matchedPrefix != "" {
				suffix := strings.TrimPrefix(req.URL.Path, matchedPrefix)
				path = f.config.Path.Value + suffix
			} else {
				path = f.config.Path.Value
			}
		}
	}

	query := req.URL.RawQuery

	if f.config.Port != nil {
		result := fmt.Sprintf("%s://%s:%d%s", scheme, hostname, *f.config.Port, path)
		if query != "" {
			return result + "?" + query
		}

		return result
	}

	result := fmt.Sprintf("%s://%s%s", scheme, hostname, path)
	if query != "" {
		return result + "?" + query
	}

	return result
}

// urlRewriter modifies the request URL path and/or host.
type urlRewriter struct {
	config *URLRewriteConfig
}

// NewURLRewriter creates a filter that rewrites the request URL.
func NewURLRewriter(config *URLRewriteConfig) Filter {
	return &urlRewriter{config: config}
}

func (f *urlRewriter) ProcessRequest(req *http.Request) *http.Response {
	if f.config.Hostname != nil {
		req.Host = *f.config.Hostname
		// Mark host as rewritten so the Director does not overwrite it.
		req.Header.Set(hostRewrittenHeader, "true")
	}

	if f.config.Path != nil {
		f.rewritePath(req)
	}

	return nil
}

func (f *urlRewriter) ProcessResponse(_ *http.Response) {}

func (f *urlRewriter) rewritePath(req *http.Request) {
	switch f.config.Path.Type {
	case URLRewriteFullPath:
		if f.config.Path.ReplaceFullPath != nil {
			req.URL.Path = *f.config.Path.ReplaceFullPath
		}
	case URLRewritePrefixMatch:
		if f.config.Path.ReplacePrefixMatch != nil {
			matchedPrefix := getMatchedPrefix(req)
			if matchedPrefix != "" {
				suffix := strings.TrimPrefix(req.URL.Path, matchedPrefix)
				req.URL.Path = *f.config.Path.ReplacePrefixMatch + suffix
			}
		}
	}
}

// requestMirror sends a copy of the request to a mirror backend asynchronously.
type requestMirror struct {
	backendURL string
}

// NewRequestMirror creates a filter that mirrors requests to a backend URL.
// All mirror instances share a single HTTP client for connection pooling.
func NewRequestMirror(backendURL string) Filter {
	return &requestMirror{
		backendURL: backendURL,
	}
}

func (f *requestMirror) ProcessRequest(req *http.Request) *http.Response {
	mirrorURL, err := req.URL.Parse(f.backendURL + req.URL.Path)
	if err != nil {
		slog.Warn("mirror: failed to parse backend URL", "error", err)

		return nil
	}

	// Buffer the request body so that both the mirror goroutine and the
	// main handler each get their own independent copy. Without this,
	// req.Clone performs a shallow copy and both readers race on the
	// same underlying io.Reader, corrupting the body.
	var bodyBuf []byte

	if req.Body != nil && req.Body != http.NoBody {
		bodyBuf, err = io.ReadAll(io.LimitReader(req.Body, maxMirrorBodySize+1))
		if err != nil {
			slog.Warn("mirror: failed to read request body, skipping mirror", "error", err)

			// Restore the body from whatever was buffered so the main handler
			// still receives the data that was read before the error.
			// Do not re-attach the original req.Body — it is in a failed state.
			if len(bodyBuf) > 0 {
				req.Body = io.NopCloser(bytes.NewReader(bodyBuf))
				req.ContentLength = int64(len(bodyBuf))
			}

			return nil
		}

		if int64(len(bodyBuf)) > maxMirrorBodySize {
			slog.Warn("mirror: request body exceeds maximum mirror size, skipping mirror",
				"max_bytes", maxMirrorBodySize)

			// Restore the original body so the main handler still works.
			req.Body = io.NopCloser(io.MultiReader(
				bytes.NewReader(bodyBuf),
				req.Body,
			))

			return nil
		}

		req.Body = io.NopCloser(bytes.NewReader(bodyBuf))
		req.ContentLength = int64(len(bodyBuf))
	}

	// Use a detached context so the mirror is fire-and-forget,
	// not cancelled when the original request completes.
	mirrorCtx, cancel := context.WithTimeout(context.Background(), mirrorTimeout)
	mirrorReq := req.Clone(mirrorCtx)
	mirrorReq.URL = mirrorURL
	mirrorReq.Host = mirrorURL.Host
	mirrorReq.RequestURI = ""

	// After Clone, both req and mirrorReq share the same body reader.
	// Give each its own independent reader from the buffered data.
	if bodyBuf != nil {
		req.Body = io.NopCloser(bytes.NewReader(bodyBuf))
		req.ContentLength = int64(len(bodyBuf))
		mirrorReq.Body = io.NopCloser(bytes.NewReader(bodyBuf))
		mirrorReq.ContentLength = int64(len(bodyBuf))
	}

	go func() {
		defer cancel()

		resp, doErr := mirrorClient.Do(mirrorReq) //nolint:gosec // mirror URL comes from trusted config
		if doErr == nil {
			resp.Body.Close()
		}
	}()

	return nil
}

func (f *requestMirror) ProcessResponse(_ *http.Response) {}

// CompileFilters converts RouteFilter specs into executable Filter instances.
func CompileFilters(filters []RouteFilter) ([]Filter, error) {
	compiled := make([]Filter, 0, len(filters))

	for idx, filter := range filters {
		compiledFilter, err := compileFilter(filter)
		if err != nil {
			return nil, errors.Wrapf(err, "filter[%d]", idx)
		}

		compiled = append(compiled, compiledFilter)
	}

	return compiled, nil
}

func compileFilter(filter RouteFilter) (Filter, error) {
	switch filter.Type {
	case FilterRequestHeaderModifier:
		return NewRequestHeaderModifier(filter.RequestHeaderModifier), nil
	case FilterResponseHeaderModifier:
		return NewResponseHeaderModifier(filter.ResponseHeaderModifier), nil
	case FilterRequestRedirect:
		return NewRequestRedirect(filter.RequestRedirect), nil
	case FilterURLRewrite:
		return NewURLRewriter(filter.URLRewrite), nil
	case FilterRequestMirror:
		return NewRequestMirror(filter.RequestMirror.BackendURL), nil
	default:
		return nil, errors.Wrapf(errUnknownFilterType, "%q", filter.Type)
	}
}

// ApplyRequestFilters runs all filters' ProcessRequest in order.
// Returns non-nil *http.Response if a filter short-circuits (e.g., redirect).
func ApplyRequestFilters(filters []Filter, req *http.Request) *http.Response {
	for _, filter := range filters {
		resp := filter.ProcessRequest(req)
		if resp != nil {
			return resp
		}
	}

	return nil
}

// ApplyResponseFilters runs all filters' ProcessResponse in order.
func ApplyResponseFilters(filters []Filter, resp *http.Response) {
	for _, filter := range filters {
		filter.ProcessResponse(resp)
	}
}
