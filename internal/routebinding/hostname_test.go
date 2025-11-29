package routebinding

import (
	"testing"

	"github.com/stretchr/testify/assert"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func ptr[T any](v T) *T {
	return &v
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
