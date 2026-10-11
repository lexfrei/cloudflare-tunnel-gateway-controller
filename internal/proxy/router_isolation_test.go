package proxy_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

const isolationGateway = "infra/gw"

// conformanceListeners are the listeners of the upstream
// GatewayHTTPListenerIsolation conformance test.
var conformanceListeners = hostListeners("", "*.example.com", "*.foo.example.com", "abc.foo.example.com")

// hostListeners returns listeners compared on hostname alone.
func hostListeners(hostnames ...string) []proxy.Listener {
	out := make([]proxy.Listener, 0, len(hostnames))
	for _, hostname := range hostnames {
		out = append(out, proxy.Listener{Hostname: hostname})
	}

	return out
}

func isolationRule(path string, hostnames []string, listeners map[string][]proxy.Listener) proxy.RouteRule {
	return proxy.RouteRule{
		Hostnames: hostnames,
		Listeners: listeners,
		Matches: []proxy.RouteMatch{{Path: &proxy.PathMatch{
			Type: proxy.PathMatchPathPrefix, Value: path,
		}}},
		Backends: []proxy.BackendRef{{URL: "http://" + path[1:] + ":80", Weight: 1}},
	}
}

func routeHost(router *proxy.Router, host, path string) *proxy.RouteResult {
	return router.Route(&http.Request{
		Method: http.MethodGet,
		Host:   host,
		URL:    &url.URL{Path: path},
		Header: http.Header{},
	})
}

// assertOwnership checks every (host, path) pair: only the path in owners
// for that host is answered.
func assertOwnership(t *testing.T, router *proxy.Router, owners map[string]string, paths []string) {
	t.Helper()

	for host, owner := range owners {
		for _, path := range paths {
			result := routeHost(router, host, path)
			if path != owner {
				assert.Nil(t, result, "%s%s must not be answered", host, path)

				continue
			}

			if assert.NotNil(t, result, "%s%s must be answered", host, path) {
				assert.Equal(t, "http://"+path[1:]+":80", result.Rule.Backends[0].URL)
			}
		}
	}
}

// TestRouter_ListenerIsolation replays the request matrix of the upstream
// GatewayHTTPListenerIsolation test: one route per listener, each answering
// only the hosts its listener is the most specific match for.
func TestRouter_ListenerIsolation(t *testing.T) {
	t.Parallel()

	attached := func(listener string) map[string][]proxy.Listener {
		return map[string][]proxy.Listener{isolationGateway: hostListeners(listener)}
	}

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version:          1,
		GatewayListeners: map[string][]proxy.Listener{isolationGateway: conformanceListeners},
		Rules: []proxy.RouteRule{
			isolationRule("/empty", nil, attached("")),
			isolationRule("/wild", []string{"*.example.com"}, attached("*.example.com")),
			isolationRule("/foo", []string{"*.foo.example.com"}, attached("*.foo.example.com")),
			isolationRule("/abc", []string{"abc.foo.example.com"}, attached("abc.foo.example.com")),
		},
	}))

	assertOwnership(t, router, map[string]string{
		"bar.com":             "/empty",
		"bar.example.com":     "/wild",
		"bar.foo.example.com": "/foo",
		"abc.foo.example.com": "/abc",
		"ABC.foo.example.com": "/abc",
	}, []string{"/empty", "/wild", "/foo", "/abc"})
}

// TestRouter_ListenerIsolationWithHostnameIntersection replays the variant
// where every route also declares hostnames: a route on the hostname-less
// listener keeps only bar.com, whatever wildcards it declares.
func TestRouter_ListenerIsolationWithHostnameIntersection(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version:          1,
		GatewayListeners: map[string][]proxy.Listener{isolationGateway: conformanceListeners},
		Rules: []proxy.RouteRule{
			isolationRule("/empty", []string{"bar.com", "*.example.com", "*.foo.example.com", "abc.foo.example.com"},
				map[string][]proxy.Listener{isolationGateway: hostListeners("")}),
			isolationRule("/wild", []string{"*.example.com", "*.foo.example.com", "abc.foo.example.com"},
				map[string][]proxy.Listener{isolationGateway: hostListeners("*.example.com")}),
		},
	}))

	assertOwnership(t, router, map[string]string{
		"bar.com":             "/empty",
		"bar.example.com":     "/wild",
		"bar.foo.example.com": "",
		"abc.foo.example.com": "",
	}, []string{"/empty", "/wild"})
}

// TestRouter_ListenerIsolationCatchAllWithNestedListener pins a catch-all
// route attached to the hostname-less listener and to a nested wildcard, but
// not to the wildcard between them: a host under the nested wildcard is still
// the route's, even though the skipped listener also matches it.
func TestRouter_ListenerIsolationCatchAllWithNestedListener(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version:          1,
		GatewayListeners: map[string][]proxy.Listener{isolationGateway: conformanceListeners},
		Rules: []proxy.RouteRule{
			isolationRule("/any", nil, map[string][]proxy.Listener{isolationGateway: hostListeners("", "*.foo.example.com")}),
		},
	}))

	assertOwnership(t, router, map[string]string{
		"bar.com":             "/any",
		"bar.foo.example.com": "/any",
		"bar.example.com":     "",
		"abc.foo.example.com": "",
	}, []string{"/any"})
}

// TestRouter_ListenerIsolationIsPerGateway pins that a rule is answered when
// ANY of its Gateways gives the host to a listener the route is attached to.
func TestRouter_ListenerIsolationIsPerGateway(t *testing.T) {
	t.Parallel()

	const other = "infra/other"

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version: 1,
		GatewayListeners: map[string][]proxy.Listener{
			isolationGateway: hostListeners("*.example.com", "foo.example.com"),
			other:            hostListeners("*.example.com"),
		},
		Rules: []proxy.RouteRule{
			isolationRule("/one", []string{"*.example.com"}, map[string][]proxy.Listener{isolationGateway: hostListeners("*.example.com")}),
			isolationRule("/both", []string{"*.example.com"}, map[string][]proxy.Listener{
				isolationGateway: hostListeners("*.example.com"),
				other:            hostListeners("*.example.com"),
			}),
		},
	}))

	assert.Nil(t, routeHost(router, "foo.example.com", "/one"))
	assert.NotNil(t, routeHost(router, "foo.example.com", "/both"))
	assert.NotNil(t, routeHost(router, "bar.example.com", "/one"))
}

// TestRouter_RuleWithoutListenersIsNotIsolated pins the behaviour for a
// config from a controller that sends no listener data.
func TestRouter_RuleWithoutListenersIsNotIsolated(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version:          1,
		GatewayListeners: map[string][]proxy.Listener{isolationGateway: conformanceListeners},
		Rules:            []proxy.RouteRule{isolationRule("/any", nil, nil)},
	}))

	assert.NotNil(t, routeHost(router, "abc.foo.example.com", "/any"))
}

// TestRouter_ListenerIsolationHostWithoutListener pins that a host no
// listener of the Gateway matches is answered by no rule attached to it.
func TestRouter_ListenerIsolationHostWithoutListener(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version:          1,
		GatewayListeners: map[string][]proxy.Listener{isolationGateway: hostListeners("*.example.com")},
		Rules: []proxy.RouteRule{
			isolationRule("/any", nil, map[string][]proxy.Listener{isolationGateway: hostListeners("*.example.com")}),
		},
	}))

	assert.NotNil(t, routeHost(router, "bar.example.com", "/any"))
	assert.Nil(t, routeHost(router, "bar.com", "/any"))
}

// TestRouter_ListenerIsolationUnknownGatewayIsNotIsolated pins that a rule
// attached to a Gateway the config carries no listeners for is not isolated
// by it.
func TestRouter_ListenerIsolationUnknownGatewayIsNotIsolated(t *testing.T) {
	t.Parallel()

	router := proxy.NewRouter()
	require.NoError(t, router.UpdateConfig(&proxy.Config{
		Version:          1,
		GatewayListeners: map[string][]proxy.Listener{isolationGateway: conformanceListeners},
		Rules: []proxy.RouteRule{
			isolationRule("/any", []string{"*.example.com"}, map[string][]proxy.Listener{"infra/unread": hostListeners("*.example.com")}),
		},
	}))

	assert.NotNil(t, routeHost(router, "abc.foo.example.com", "/any"))
}
