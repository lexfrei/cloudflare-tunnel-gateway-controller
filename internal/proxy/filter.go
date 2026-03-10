package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

// mirrorTimeout is the maximum time to wait for a mirror request.
const mirrorTimeout = 5 * time.Second

// matchedPrefixKey is the context key for storing the matched path prefix.
type matchedPrefixKey struct{}

// SetMatchedPrefix stores the matched path prefix in the request context.
// Used by URL rewrite filters for ReplacePrefixMatch.
func SetMatchedPrefix(req *http.Request, prefix string) {
	ctx := context.WithValue(req.Context(), matchedPrefixKey{}, prefix)
	*req = *req.WithContext(ctx)
}

// getMatchedPrefix retrieves the matched path prefix from the request context.
func getMatchedPrefix(req *http.Request) string {
	prefix, _ := req.Context().Value(matchedPrefixKey{}).(string)

	return prefix
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
		path = *f.config.Path
	}

	if f.config.Port != nil {
		return fmt.Sprintf("%s://%s:%d%s", scheme, hostname, *f.config.Port, path)
	}

	return fmt.Sprintf("%s://%s%s", scheme, hostname, path)
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
	client     *http.Client
}

// NewRequestMirror creates a filter that mirrors requests to a backend URL.
func NewRequestMirror(backendURL string) Filter {
	return &requestMirror{
		backendURL: backendURL,
		client: &http.Client{
			Timeout: mirrorTimeout,
		},
	}
}

func (f *requestMirror) ProcessRequest(req *http.Request) *http.Response {
	mirrorReq := req.Clone(req.Context())
	mirrorReq.URL, _ = req.URL.Parse(f.backendURL + req.URL.Path)
	mirrorReq.Host = mirrorReq.URL.Host
	mirrorReq.RequestURI = ""

	go func() {
		resp, err := f.client.Do(mirrorReq) //nolint:gosec // mirror URL comes from trusted config
		if err == nil {
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
