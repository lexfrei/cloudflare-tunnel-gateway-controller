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
)

// Config is the top-level configuration pushed by the controller.
// It contains a version for ordering and a list of routing rules.
type Config struct {
	Version int64       `json:"version"`
	Rules   []RouteRule `json:"rules"`
}

// RouteRule represents a single routing rule derived from a Gateway API HTTPRoute.
// Each rule maps a set of hostnames + match conditions to a set of backends.
type RouteRule struct {
	Hostnames []string       `json:"hostnames,omitempty"`
	Matches   []RouteMatch   `json:"matches,omitempty"`
	Filters   []RouteFilter  `json:"filters,omitempty"`
	Backends  []BackendRef   `json:"backends"`
	Timeouts  *RouteTimeouts `json:"timeouts,omitempty"`
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
)

// RouteFilter defines a transformation to apply to matching requests.
type RouteFilter struct {
	Type                   RouteFilterType   `json:"type"`
	RequestHeaderModifier  *HeaderModifier   `json:"requestHeaderModifier,omitempty"`
	ResponseHeaderModifier *HeaderModifier   `json:"responseHeaderModifier,omitempty"`
	RequestRedirect        *RedirectConfig   `json:"requestRedirect,omitempty"`
	URLRewrite             *URLRewriteConfig `json:"urlRewrite,omitempty"`
	RequestMirror          *MirrorConfig     `json:"requestMirror,omitempty"`
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
	Scheme     *string `json:"scheme,omitempty"`
	Hostname   *string `json:"hostname,omitempty"`
	Port       *int32  `json:"port,omitempty"`
	Path       *string `json:"path,omitempty"`
	StatusCode *int    `json:"statusCode,omitempty"`
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

// MirrorConfig describes where to mirror requests.
type MirrorConfig struct {
	BackendURL string `json:"backendUrl"`
}

// BackendRef identifies a backend service with a weight for traffic splitting.
type BackendRef struct {
	URL    string `json:"url"`
	Weight int32  `json:"weight"`
}

// RouteTimeouts configures timeout durations for proxied requests.
type RouteTimeouts struct {
	Request time.Duration `json:"request,omitempty"`
	Backend time.Duration `json:"backend,omitempty"`
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
	if len(r.Backends) == 0 {
		return errors.New("at least one backend is required")
	}

	for idx, backend := range r.Backends {
		if backend.URL == "" {
			return errors.Wrapf(errURLRequired, "backend[%d]", idx)
		}

		if backend.Weight < 0 {
			return errors.Wrapf(errWeightNonNegative, "backend[%d]", idx)
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
	case FilterResponseHeaderModifier:
		if f.ResponseHeaderModifier == nil {
			return errors.New("responseHeaderModifier config is required")
		}
	case FilterRequestRedirect:
		if f.RequestRedirect == nil {
			return errors.New("requestRedirect config is required")
		}
	case FilterURLRewrite:
		if f.URLRewrite == nil {
			return errors.New("urlRewrite config is required")
		}
	case FilterRequestMirror:
		if f.RequestMirror == nil {
			return errors.New("requestMirror config is required")
		}
	default:
		return errors.Wrapf(errUnknownFilterType, "%q", f.Type)
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
