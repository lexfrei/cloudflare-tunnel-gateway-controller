package routebinding

import (
	"testing"

	"github.com/stretchr/testify/assert"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func ptr[T any](v T) *T {
	return new(v)
}

func TestHostnamesIntersect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		listenerHost   *gatewayv1.Hostname
		routeHostnames []gatewayv1.Hostname
		expected       bool
	}{
		{
			name:           "nil listener matches any route hostname",
			listenerHost:   nil,
			routeHostnames: []gatewayv1.Hostname{"example.com"},
			expected:       true,
		},
		{
			name:           "empty string listener matches any route hostname",
			listenerHost:   ptr(gatewayv1.Hostname("")),
			routeHostnames: []gatewayv1.Hostname{"example.com"},
			expected:       true,
		},
		{
			name:           "empty route hostnames matches any listener",
			listenerHost:   ptr(gatewayv1.Hostname("example.com")),
			routeHostnames: nil,
			expected:       true,
		},
		{
			name:           "empty route hostnames slice matches any listener",
			listenerHost:   ptr(gatewayv1.Hostname("example.com")),
			routeHostnames: []gatewayv1.Hostname{},
			expected:       true,
		},
		{
			name:           "both nil/empty matches",
			listenerHost:   nil,
			routeHostnames: nil,
			expected:       true,
		},
		{
			name:           "exact match",
			listenerHost:   ptr(gatewayv1.Hostname("example.com")),
			routeHostnames: []gatewayv1.Hostname{"example.com"},
			expected:       true,
		},
		{
			name:           "no match different domains",
			listenerHost:   ptr(gatewayv1.Hostname("example.com")),
			routeHostnames: []gatewayv1.Hostname{"other.com"},
			expected:       false,
		},
		{
			name:           "wildcard listener matches subdomain",
			listenerHost:   ptr(gatewayv1.Hostname("*.example.com")),
			routeHostnames: []gatewayv1.Hostname{"foo.example.com"},
			expected:       true,
		},
		{
			name:           "wildcard listener matches nested subdomain",
			listenerHost:   ptr(gatewayv1.Hostname("*.example.com")),
			routeHostnames: []gatewayv1.Hostname{"bar.foo.example.com"},
			expected:       true,
		},
		{
			name:           "wildcard listener does NOT match exact domain",
			listenerHost:   ptr(gatewayv1.Hostname("*.example.com")),
			routeHostnames: []gatewayv1.Hostname{"example.com"},
			expected:       false,
		},
		{
			name:           "wildcard route matches specific listener",
			listenerHost:   ptr(gatewayv1.Hostname("api.example.com")),
			routeHostnames: []gatewayv1.Hostname{"*.example.com"},
			expected:       true,
		},
		{
			name:           "wildcard route does NOT match exact domain listener",
			listenerHost:   ptr(gatewayv1.Hostname("example.com")),
			routeHostnames: []gatewayv1.Hostname{"*.example.com"},
			expected:       false,
		},
		{
			name:           "both wildcards same domain intersect",
			listenerHost:   ptr(gatewayv1.Hostname("*.example.com")),
			routeHostnames: []gatewayv1.Hostname{"*.example.com"},
			expected:       true,
		},
		{
			name:           "multiple route hostnames one matches",
			listenerHost:   ptr(gatewayv1.Hostname("example.com")),
			routeHostnames: []gatewayv1.Hostname{"other.com", "another.com", "example.com"},
			expected:       true,
		},
		{
			name:           "multiple route hostnames none match",
			listenerHost:   ptr(gatewayv1.Hostname("example.com")),
			routeHostnames: []gatewayv1.Hostname{"other.com", "another.com"},
			expected:       false,
		},
		{
			name:           "wildcard listener multiple routes one matches",
			listenerHost:   ptr(gatewayv1.Hostname("*.example.com")),
			routeHostnames: []gatewayv1.Hostname{"other.com", "app.example.com"},
			expected:       true,
		},
		{
			name:           "case sensitivity exact match",
			listenerHost:   ptr(gatewayv1.Hostname("Example.COM")),
			routeHostnames: []gatewayv1.Hostname{"example.com"},
			expected:       true,
		},
		{
			name:           "case sensitivity wildcard match",
			listenerHost:   ptr(gatewayv1.Hostname("*.Example.COM")),
			routeHostnames: []gatewayv1.Hostname{"app.example.com"},
			expected:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := HostnamesIntersect(tt.listenerHost, tt.routeHostnames)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestHostnameIntersection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		listenerHost *gatewayv1.Hostname
		routeHost    gatewayv1.Hostname
		expectedHost gatewayv1.Hostname
		expectedOK   bool
	}{
		{
			name:         "nil listener passes route exact host through",
			listenerHost: nil,
			routeHost:    "example.com",
			expectedHost: "example.com",
			expectedOK:   true,
		},
		{
			name:         "empty listener passes route wildcard through",
			listenerHost: ptr(gatewayv1.Hostname("")),
			routeHost:    "*.example.com",
			expectedHost: "*.example.com",
			expectedOK:   true,
		},
		{
			name:         "equal exact hosts intersect to that host",
			listenerHost: ptr(gatewayv1.Hostname("example.com")),
			routeHost:    "example.com",
			expectedHost: "example.com",
			expectedOK:   true,
		},
		{
			name:         "different exact hosts do not intersect",
			listenerHost: ptr(gatewayv1.Hostname("example.com")),
			routeHost:    "other.com",
			expectedOK:   false,
		},
		{
			name:         "exact route host under wildcard listener yields the exact host",
			listenerHost: ptr(gatewayv1.Hostname("*.wildcard.io")),
			routeHost:    "foo.wildcard.io",
			expectedHost: "foo.wildcard.io",
			expectedOK:   true,
		},
		{
			name:         "exact route host under wildcard listener matches multi-label",
			listenerHost: ptr(gatewayv1.Hostname("*.wildcard.io")),
			routeHost:    "foo.bar.wildcard.io",
			expectedHost: "foo.bar.wildcard.io",
			expectedOK:   true,
		},
		{
			name:         "wildcard listener does not match its apex",
			listenerHost: ptr(gatewayv1.Hostname("*.wildcard.io")),
			routeHost:    "wildcard.io",
			expectedOK:   false,
		},
		{
			name:         "wildcard route over exact listener yields the listener exact host",
			listenerHost: ptr(gatewayv1.Hostname("very.specific.com")),
			routeHost:    "*.specific.com",
			expectedHost: "very.specific.com",
			expectedOK:   true,
		},
		{
			name:         "wildcard route does not match listener apex",
			listenerHost: ptr(gatewayv1.Hostname("specific.com")),
			routeHost:    "*.specific.com",
			expectedOK:   false,
		},
		{
			name:         "equal wildcards intersect to the wildcard",
			listenerHost: ptr(gatewayv1.Hostname("*.anotherwildcard.io")),
			routeHost:    "*.anotherwildcard.io",
			expectedHost: "*.anotherwildcard.io",
			expectedOK:   true,
		},
		{
			name:         "different wildcards do not intersect",
			listenerHost: ptr(gatewayv1.Hostname("*.wildcard.io")),
			routeHost:    "*.other.io",
			expectedOK:   false,
		},
		{
			name:         "nested wildcard route under broader wildcard listener yields the nested wildcard",
			listenerHost: ptr(gatewayv1.Hostname("*.wildcard.io")),
			routeHost:    "*.sub.wildcard.io",
			expectedHost: "*.sub.wildcard.io",
			expectedOK:   true,
		},
		{
			name:         "broader wildcard route over nested wildcard listener yields the nested wildcard",
			listenerHost: ptr(gatewayv1.Hostname("*.sub.wildcard.io")),
			routeHost:    "*.wildcard.io",
			expectedHost: "*.sub.wildcard.io",
			expectedOK:   true,
		},
		{
			name:         "exact route host does not match different wildcard listener",
			listenerHost: ptr(gatewayv1.Hostname("*.wildcard.io")),
			routeHost:    "foo.other.io",
			expectedOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			host, ok := HostnameIntersection(tt.listenerHost, tt.routeHost)
			assert.Equal(t, tt.expectedOK, ok)
			if tt.expectedOK {
				assert.Equal(t, tt.expectedHost, host)
			}
		})
	}
}

func TestEffectiveListenerHostnames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		listenerHost   *gatewayv1.Hostname
		routeHostnames []gatewayv1.Hostname
		expected       []gatewayv1.Hostname
	}{
		{
			name:         "hostname-less route inherits exact listener hostname",
			listenerHost: ptr(gatewayv1.Hostname("listener.example.com")),
			expected:     []gatewayv1.Hostname{"listener.example.com"},
		},
		{
			name:         "hostname-less route inherits wildcard listener hostname",
			listenerHost: ptr(gatewayv1.Hostname("*.example.com")),
			expected:     []gatewayv1.Hostname{"*.example.com"},
		},
		{
			name:           "hostname-less route and hostname-less listener stays catch-all",
			listenerHost:   nil,
			routeHostnames: nil,
			expected:       nil,
		},
		{
			name:           "route hostnames pass through a hostname-less listener unchanged",
			listenerHost:   nil,
			routeHostnames: []gatewayv1.Hostname{"a.example.com", "b.example.com"},
			expected:       []gatewayv1.Hostname{"a.example.com", "b.example.com"},
		},
		{
			name:           "exact listener narrows to only the matching declared host",
			listenerHost:   ptr(gatewayv1.Hostname("very.specific.com")),
			routeHostnames: []gatewayv1.Hostname{"non.matching.com", "*.nonmatchingwildcard.io", "very.specific.com"},
			expected:       []gatewayv1.Hostname{"very.specific.com"},
		},
		{
			name:           "wildcard listener keeps only subdomains, drops apex and non-matching",
			listenerHost:   ptr(gatewayv1.Hostname("*.wildcard.io")),
			routeHostnames: []gatewayv1.Hostname{"non.matching.com", "wildcard.io", "foo.wildcard.io", "bar.wildcard.io", "foo.bar.wildcard.io"},
			expected:       []gatewayv1.Hostname{"foo.wildcard.io", "bar.wildcard.io", "foo.bar.wildcard.io"},
		},
		{
			name:           "wildcard route over exact listener yields the listener host",
			listenerHost:   ptr(gatewayv1.Hostname("very.specific.com")),
			routeHostnames: []gatewayv1.Hostname{"non.matching.com", "*.specific.com"},
			expected:       []gatewayv1.Hostname{"very.specific.com"},
		},
		{
			name:           "equal wildcards yield the wildcard",
			listenerHost:   ptr(gatewayv1.Hostname("*.anotherwildcard.io")),
			routeHostnames: []gatewayv1.Hostname{"*.anotherwildcard.io"},
			expected:       []gatewayv1.Hostname{"*.anotherwildcard.io"},
		},
		{
			name:           "nested wildcard route hostname is kept under a broader wildcard listener",
			listenerHost:   ptr(gatewayv1.Hostname("*.wildcard.io")),
			routeHostnames: []gatewayv1.Hostname{"foo.wildcard.io", "*.sub.wildcard.io"},
			expected:       []gatewayv1.Hostname{"foo.wildcard.io", "*.sub.wildcard.io"},
		},
		{
			name:           "empty intersection yields no hostnames",
			listenerHost:   ptr(gatewayv1.Hostname("very.specific.com")),
			routeHostnames: []gatewayv1.Hostname{"non.matching.com", "wildcard.io"},
			expected:       nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := EffectiveListenerHostnames(tt.listenerHost, tt.routeHostnames)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestHostnameMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		listenerHost string
		routeHost    string
		expected     bool
	}{
		{
			name:         "exact match",
			listenerHost: "example.com",
			routeHost:    "example.com",
			expected:     true,
		},
		{
			name:         "no match",
			listenerHost: "example.com",
			routeHost:    "other.com",
			expected:     false,
		},
		{
			name:         "listener wildcard matches subdomain",
			listenerHost: "*.example.com",
			routeHost:    "app.example.com",
			expected:     true,
		},
		{
			name:         "listener wildcard matches deep subdomain",
			listenerHost: "*.example.com",
			routeHost:    "deep.app.example.com",
			expected:     true,
		},
		{
			name:         "listener wildcard does not match base domain",
			listenerHost: "*.example.com",
			routeHost:    "example.com",
			expected:     false,
		},
		{
			name:         "route wildcard matches specific listener",
			listenerHost: "app.example.com",
			routeHost:    "*.example.com",
			expected:     true,
		},
		{
			name:         "route wildcard does not match base domain listener",
			listenerHost: "example.com",
			routeHost:    "*.example.com",
			expected:     false,
		},
		{
			name:         "both wildcards same suffix",
			listenerHost: "*.example.com",
			routeHost:    "*.example.com",
			expected:     true,
		},
		{
			name:         "both wildcards different suffix",
			listenerHost: "*.example.com",
			routeHost:    "*.other.com",
			expected:     false,
		},
		{
			name:         "nested wildcard route under broader wildcard listener",
			listenerHost: "*.example.com",
			routeHost:    "*.sub.example.com",
			expected:     true,
		},
		{
			name:         "broader wildcard route over nested wildcard listener",
			listenerHost: "*.sub.example.com",
			routeHost:    "*.example.com",
			expected:     true,
		},
		{
			name:         "case insensitive exact",
			listenerHost: "EXAMPLE.COM",
			routeHost:    "example.com",
			expected:     true,
		},
		{
			name:         "case insensitive wildcard",
			listenerHost: "*.EXAMPLE.COM",
			routeHost:    "app.example.com",
			expected:     true,
		},
		{
			name:         "wildcard only in prefix position",
			listenerHost: "app.*.example.com",
			routeHost:    "app.test.example.com",
			expected:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := hostnameMatches(tt.listenerHost, tt.routeHost)
			assert.Equal(t, tt.expected, result)
		})
	}
}
