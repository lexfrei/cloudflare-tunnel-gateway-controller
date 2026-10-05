package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

func routeHostProto(router *proxy.Router, host, proto, path string) *proxy.RouteResult {
	header := http.Header{}
	if proto != "" {
		header.Set("X-Forwarded-Proto", proto)
	}

	return router.Route(&http.Request{
		Method: http.MethodGet,
		Host:   host,
		URL:    &url.URL{Path: path},
		Header: header,
	})
}

func backendOf(result *proxy.RouteResult) string {
	if result == nil {
		return ""
	}

	return result.Rule.Backends[0].URL
}

// TestRouter_ListenerPortMatching replays the upstream
// HTTPRouteListenerPortMatching test: listeners share hostnames across ports,
// and the port in Host picks the listener, 80 when Host carries none.
func TestRouter_ListenerPortMatching(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		GatewayListeners: map[string][]proxy.Listener{isolationGateway: {
			{Hostname: "foo.com", Port: 80},
			{Hostname: "foo.com", Port: 8080},
			{Hostname: "bar.com", Port: 8080},
			{Hostname: "foo.com", Port: 8090},
			{Hostname: "bar.com", Port: 8090},
		}},
		Rules: []proxy.RouteRule{
			isolationRule("/v1", []string{"foo.com"}, map[string][]proxy.Listener{isolationGateway: {
				{Hostname: "foo.com", Port: 80},
			}}),
			isolationRule("/v2", []string{"foo.com", "bar.com"}, map[string][]proxy.Listener{isolationGateway: {
				{Hostname: "foo.com", Port: 8080}, {Hostname: "bar.com", Port: 8080},
			}}),
			isolationRule("/v3", []string{"foo.com"}, map[string][]proxy.Listener{isolationGateway: {
				{Hostname: "foo.com", Port: 8090},
			}}),
		},
	}))

	answers := func(host string) []string {
		var out []string

		for _, path := range []string{"/v1", "/v2", "/v3"} {
			if result := routeHostProto(router, host, "http", path); result != nil {
				out = append(out, path)
			}
		}

		return out
	}

	assert.Equal(t, []string{"/v1"}, answers("foo.com"))
	assert.Equal(t, []string{"/v1"}, answers("foo.com:80"))
	assert.Equal(t, []string{"/v2"}, answers("foo.com:8080"))
	assert.Equal(t, []string{"/v2"}, answers("bar.com:8080"))
	assert.Equal(t, []string{"/v3"}, answers("FOO.com:8090"))
	assert.Empty(t, answers("bar.com:8090"), "no route is attached to bar.com:8090")
	assert.Empty(t, answers("foo.com:9000"), "no listener has port 9000")
}

// TestRouter_ListenerSchemeDefaultPort pins the port a request without one
// in Host is matched on: 443 for https, 80 for http and for a request that
// names no scheme.
func TestRouter_ListenerSchemeDefaultPort(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		GatewayListeners: map[string][]proxy.Listener{isolationGateway: {
			{Port: 80}, {Port: 443},
		}},
		Rules: []proxy.RouteRule{
			isolationRule("/http", nil, map[string][]proxy.Listener{isolationGateway: {{Port: 80}}}),
			isolationRule("/https", nil, map[string][]proxy.Listener{isolationGateway: {{Port: 443}}}),
		},
	}))

	cases := []struct {
		proto, path string
		answered    bool
	}{
		{"https", "/https", true},
		{"https", "/http", false},
		{"HTTPS", "/https", true},
		{"https, http", "/https", true},
		{"http", "/http", true},
		{"http", "/https", false},
		{"", "/http", true},
		{"", "/https", false},
		{"gopher", "/http", true},
	}

	for _, tc := range cases {
		result := routeHostProto(router, "app.example.com", tc.proto, tc.path)
		assert.Equal(t, tc.answered, result != nil, "X-Forwarded-Proto %q, %s", tc.proto, tc.path)
	}
}

// TestRouter_SamePathOnGatewaysWithDifferentPorts pins the collision the
// upstream HTTPRouteRedirectPortAndScheme test hits on one tunnel: three
// Gateways with a hostname-less listener each, on ports 80, 8080 and 443,
// carry a route with the same path. Each request reaches the route of the
// listener its port and scheme name, whichever route sorts first.
func TestRouter_SamePathOnGatewaysWithDifferentPorts(t *testing.T) {
	t.Parallel()

	const (
		gw80   = "infra/same-namespace"
		gw8080 = "infra/http-on-8080"
		gw443  = "infra/https"
	)

	rule := func(backend, gateway string, port int32) proxy.RouteRule {
		return proxy.RouteRule{
			Listeners: map[string][]proxy.Listener{gateway: {{Port: port}}},
			Matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{
				Type: proxy.PathMatchPathPrefix, Value: "/scheme-nil-and-port-nil",
			}}},
			Backends: []proxy.BackendRef{{URL: "http://" + backend + ":80", Weight: 1}},
		}
	}

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		GatewayListeners: map[string][]proxy.Listener{
			gw80: {{Port: 80}}, gw8080: {{Port: 8080}}, gw443: {{Port: 443}},
		},
		Rules: []proxy.RouteRule{
			rule("on-443", gw443, 443),
			rule("on-8080", gw8080, 8080),
			rule("on-80", gw80, 80),
		},
	}))

	const tunnelHost = "gw.cfargotunnel.example"

	assert.Equal(t, "http://on-80:80", backendOf(routeHostProto(router, tunnelHost, "http", "/scheme-nil-and-port-nil")))
	assert.Equal(t, "http://on-80:80", backendOf(routeHostProto(router, tunnelHost+":80", "http", "/scheme-nil-and-port-nil")))
	assert.Equal(t, "http://on-8080:80", backendOf(routeHostProto(router, tunnelHost+":8080", "http", "/scheme-nil-and-port-nil")))
	assert.Equal(t, "http://on-443:80", backendOf(routeHostProto(router, tunnelHost, "https", "/scheme-nil-and-port-nil")))
}

// TestRouter_ListenerWithoutPortMatchesEveryPort pins the port-less listener
// a config can carry: it is compared on hostname alone.
func TestRouter_ListenerWithoutPortMatchesEveryPort(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version:          1,
		GatewayListeners: map[string][]proxy.Listener{isolationGateway: {{Hostname: "*.example.com"}}},
		Rules: []proxy.RouteRule{
			isolationRule("/any", nil, map[string][]proxy.Listener{isolationGateway: {{Hostname: "*.example.com"}}}),
		},
	}))

	assert.NotNil(t, routeHostProto(router, "a.example.com:8443", "https", "/any"))
	assert.NotNil(t, routeHostProto(router, "a.example.com", "http", "/any"))
}

// TestHandler_XOriginalProtoNeedsOptIn pins the test-only scheme carrier:
// honoured over X-Forwarded-Proto only in a deployment that trusts
// X-Original-Host, and dropped otherwise.
func TestHandler_XOriginalProtoNeedsOptIn(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		Rules: []proxy.RouteRule{{
			Filters: []proxy.RouteFilter{{
				Type:            proxy.FilterRequestRedirect,
				RequestRedirect: &proxy.RedirectConfig{Hostname: new("example.org")},
			}},
		}},
	}))

	location := func(handler *proxy.Handler) string {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://gw.example/", nil)
		req.Host = "gw.example"
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Original-Host", "gw.example:1234")
		req.Header.Set("X-Original-Proto", "http")
		req.Header.Set("X-Original-Port", "8080")

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)

		return recorder.Header().Get("Location")
	}

	assert.Equal(t, "http://example.org:8080/", location(proxy.NewHandler(router, proxy.WithAllowXOriginalHost(true))))
	assert.Equal(t, "https://example.org/", location(proxy.NewHandler(router)))
}

// TestRouter_XOriginalPortWinsOverHostPort pins the conformance suite's
// connection port carrier: the suite may name any port in Host, while the
// listener is the one on the port it connected to.
func TestRouter_XOriginalPortWinsOverHostPort(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version:          1,
		GatewayListeners: map[string][]proxy.Listener{isolationGateway: {{Port: 80}}},
		Rules: []proxy.RouteRule{
			isolationRule("/any", nil, map[string][]proxy.Listener{isolationGateway: {{Port: 80}}}),
		},
	}))

	request := func(port string) *proxy.RouteResult {
		header := http.Header{"X-Original-Host": {"very.specific.com:1234"}, "X-Forwarded-Proto": {"https"}}
		if port != "" {
			header.Set("X-Original-Port", port)
		}

		return router.Route(&http.Request{Method: http.MethodGet, Host: "edge.example", URL: &url.URL{Path: "/any"}, Header: header})
	}

	assert.NotNil(t, request("80"))
	assert.Nil(t, request(""), "without the carrier the port in Host decides")
	assert.Nil(t, request("8080"))
	assert.Nil(t, request("bogus"), "an unparsable carrier falls back to the port in Host")
}

// TestRouter_HostPortParsing pins how a Host the port cannot be read from is
// matched: on the scheme's default port, with the host before the colon. A
// colon inside IPv6 brackets is not a port separator.
func TestRouter_HostPortParsing(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version:          1,
		GatewayListeners: map[string][]proxy.Listener{isolationGateway: {{Port: 80}, {Port: 8080}}},
		Rules: []proxy.RouteRule{
			isolationRule("/on-80", []string{"foo.com"}, map[string][]proxy.Listener{isolationGateway: {{Port: 80}}}),
			isolationRule("/on-8080", []string{"foo.com"}, map[string][]proxy.Listener{isolationGateway: {{Port: 8080}}}),
			isolationRule("/v6-80", nil, map[string][]proxy.Listener{isolationGateway: {{Port: 80}}}),
			isolationRule("/v6-8080", nil, map[string][]proxy.Listener{isolationGateway: {{Port: 8080}}}),
		},
	}))

	cases := []struct {
		host, path string
		answered   bool
	}{
		{"foo.com:abc", "/on-80", true},
		{"foo.com:", "/on-80", true},
		{"foo.com:65536", "/on-80", true},
		{"foo.com:0", "/on-80", true},
		{"foo.com:abc", "/on-8080", false},
		{"[::1]:8080", "/v6-8080", true},
		{"[::1]:8080", "/v6-80", false},
		{"[::1]", "/v6-80", true},
	}

	for _, tc := range cases {
		assert.Equal(t, tc.answered, routeHostProto(router, tc.host, "http", tc.path) != nil, "Host %q, %s", tc.host, tc.path)
	}
}
