package proxy

import (
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/cockroachdb/errors"
)

// RequestMatcher evaluates whether an incoming HTTP request matches a condition.
type RequestMatcher interface {
	Match(r *http.Request) bool
}

// exactPathMatcher matches requests with an exactly equal URL path.
type exactPathMatcher struct {
	path string
}

// NewExactPathMatcher creates a matcher that requires exact path equality.
func NewExactPathMatcher(path string) RequestMatcher {
	return &exactPathMatcher{path: path}
}

func (m *exactPathMatcher) Match(r *http.Request) bool {
	return r.URL.Path == m.path
}

// prefixPathMatcher matches requests whose URL path starts with the given prefix.
type prefixPathMatcher struct {
	prefix string
}

// NewPrefixPathMatcher creates a matcher for path prefix matching.
func NewPrefixPathMatcher(prefix string) RequestMatcher {
	return &prefixPathMatcher{prefix: prefix}
}

func (m *prefixPathMatcher) Match(r *http.Request) bool {
	path := r.URL.Path

	if !strings.HasPrefix(path, m.prefix) {
		return false
	}

	// Gateway API requires segment-aware prefix matching:
	// /foo matches /foo and /foo/bar but NOT /foobar.
	if len(path) == len(m.prefix) {
		return true
	}

	// Prefix ends with / — any continuation is valid.
	if strings.HasSuffix(m.prefix, "/") {
		return true
	}

	// Next character after prefix must be a segment boundary.
	return path[len(m.prefix)] == '/'
}

// regexPathMatcher matches requests whose URL path matches a precompiled regex.
type regexPathMatcher struct {
	re *regexp.Regexp
}

// NewRegexPathMatcher creates a matcher using a regular expression on the path.
// The regex is compiled once at creation time.
func NewRegexPathMatcher(pattern string) (RequestMatcher, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, errors.Wrap(err, "failed to compile regex path pattern")
	}

	return &regexPathMatcher{re: re}, nil
}

func (m *regexPathMatcher) Match(r *http.Request) bool {
	return m.re.MatchString(r.URL.Path)
}

// exactHeaderMatcher matches when a request header has an exact value.
type exactHeaderMatcher struct {
	name  string
	value string
}

// NewExactHeaderMatcher creates a matcher for exact header value comparison.
// Header name lookup is case-insensitive (per HTTP spec).
func NewExactHeaderMatcher(name, value string) RequestMatcher {
	return &exactHeaderMatcher{name: name, value: value}
}

func (m *exactHeaderMatcher) Match(r *http.Request) bool {
	return slices.Contains(r.Header.Values(m.name), m.value)
}

// regexHeaderMatcher matches when any value of a header matches a regex.
type regexHeaderMatcher struct {
	name string
	re   *regexp.Regexp
}

// NewRegexHeaderMatcher creates a matcher using regex on header values.
func NewRegexHeaderMatcher(name, pattern string) (RequestMatcher, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, errors.Wrap(err, "failed to compile regex header pattern")
	}

	return &regexHeaderMatcher{name: name, re: re}, nil
}

func (m *regexHeaderMatcher) Match(r *http.Request) bool {
	return slices.ContainsFunc(r.Header.Values(m.name), m.re.MatchString)
}

// exactQueryMatcher matches when a query parameter has an exact value.
type exactQueryMatcher struct {
	param string
	value string
}

// NewExactQueryMatcher creates a matcher for exact query parameter value.
func NewExactQueryMatcher(param, value string) RequestMatcher {
	return &exactQueryMatcher{param: param, value: value}
}

func (m *exactQueryMatcher) Match(r *http.Request) bool {
	return slices.Contains(r.URL.Query()[m.param], m.value)
}

// regexQueryMatcher matches when any value of a query param matches a regex.
type regexQueryMatcher struct {
	param string
	re    *regexp.Regexp
}

// NewRegexQueryMatcher creates a matcher using regex on query parameter values.
func NewRegexQueryMatcher(param, pattern string) (RequestMatcher, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, errors.Wrap(err, "failed to compile regex query pattern")
	}

	return &regexQueryMatcher{param: param, re: re}, nil
}

func (m *regexQueryMatcher) Match(r *http.Request) bool {
	return slices.ContainsFunc(r.URL.Query()[m.param], m.re.MatchString)
}

// methodMatcher matches requests by HTTP method.
type methodMatcher struct {
	method string
}

// NewMethodMatcher creates a matcher for a specific HTTP method.
func NewMethodMatcher(method string) RequestMatcher {
	return &methodMatcher{method: method}
}

func (m *methodMatcher) Match(r *http.Request) bool {
	return r.Method == m.method
}

// CompiledMatch is a precompiled set of matchers for a single RouteMatch.
// All matchers must match (AND logic) for the CompiledMatch to match.
type CompiledMatch struct {
	matchers []RequestMatcher
}

// Match evaluates all matchers against the request. Returns true if all match.
// An empty matcher list matches everything.
func (cm *CompiledMatch) Match(r *http.Request) bool {
	for _, m := range cm.matchers {
		if !m.Match(r) {
			return false
		}
	}

	return true
}

// CompileMatch compiles a RouteMatch into a CompiledMatch with precompiled regex patterns.
func CompileMatch(match RouteMatch) (*CompiledMatch, error) {
	var matchers []RequestMatcher

	if match.Path != nil {
		pathMatcher, err := compilePathMatcher(match.Path)
		if err != nil {
			return nil, err
		}

		matchers = append(matchers, pathMatcher)
	}

	if match.Method != "" {
		matchers = append(matchers, NewMethodMatcher(match.Method))
	}

	for _, header := range match.Headers {
		headerMatcher, err := compileHeaderMatcher(header)
		if err != nil {
			return nil, err
		}

		matchers = append(matchers, headerMatcher)
	}

	for _, query := range match.QueryParams {
		queryMatcher, err := compileQueryMatcher(query)
		if err != nil {
			return nil, err
		}

		matchers = append(matchers, queryMatcher)
	}

	return &CompiledMatch{matchers: matchers}, nil
}

func compilePathMatcher(pathMatch *PathMatch) (RequestMatcher, error) {
	switch pathMatch.Type {
	case PathMatchExact:
		return NewExactPathMatcher(pathMatch.Value), nil
	case PathMatchPathPrefix:
		return NewPrefixPathMatcher(pathMatch.Value), nil
	case PathMatchRegularExpression:
		return NewRegexPathMatcher(pathMatch.Value)
	default:
		return nil, errors.Wrapf(errUnknownPathType, "%s", pathMatch.Type)
	}
}

func compileHeaderMatcher(headerMatch HeaderMatch) (RequestMatcher, error) {
	switch headerMatch.Type {
	case HeaderMatchExact:
		return NewExactHeaderMatcher(headerMatch.Name, headerMatch.Value), nil
	case HeaderMatchRegularExpression:
		return NewRegexHeaderMatcher(headerMatch.Name, headerMatch.Value)
	default:
		return nil, errors.Wrapf(errUnknownHeaderType, "%s", headerMatch.Type)
	}
}

func compileQueryMatcher(queryMatch QueryParamMatch) (RequestMatcher, error) {
	switch queryMatch.Type {
	case QueryParamMatchExact:
		return NewExactQueryMatcher(queryMatch.Name, queryMatch.Value), nil
	case QueryParamMatchRegularExpression:
		return NewRegexQueryMatcher(queryMatch.Name, queryMatch.Value)
	default:
		return nil, errors.Wrapf(errUnknownQueryType, "%s", queryMatch.Type)
	}
}
