package ingress

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSortRouteEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		entries  []routeEntry
		expected []routeEntry
	}{
		{
			name:     "empty entries",
			entries:  []routeEntry{},
			expected: []routeEntry{},
		},
		{
			name: "wildcard hostname last",
			entries: []routeEntry{
				{hostname: "*", path: "/", service: "svc1"},
				{hostname: "example.com", path: "/", service: "svc2"},
			},
			expected: []routeEntry{
				{hostname: "example.com", path: "/", service: "svc2"},
				{hostname: "*", path: "/", service: "svc1"},
			},
		},
		{
			name: "hostnames sorted alphabetically",
			entries: []routeEntry{
				{hostname: "zoo.com", path: "/", service: "svc1"},
				{hostname: "alpha.com", path: "/", service: "svc2"},
				{hostname: "beta.com", path: "/", service: "svc3"},
			},
			expected: []routeEntry{
				{hostname: "alpha.com", path: "/", service: "svc2"},
				{hostname: "beta.com", path: "/", service: "svc3"},
				{hostname: "zoo.com", path: "/", service: "svc1"},
			},
		},
		{
			name: "priority - exact before prefix",
			entries: []routeEntry{
				{hostname: "example.com", path: "/api", priority: 0, service: "prefix"},
				{hostname: "example.com", path: "/api", priority: 1, service: "exact"},
			},
			expected: []routeEntry{
				{hostname: "example.com", path: "/api", priority: 1, service: "exact"},
				{hostname: "example.com", path: "/api", priority: 0, service: "prefix"},
			},
		},
		{
			name: "longer paths first",
			entries: []routeEntry{
				{hostname: "example.com", path: "/a", service: "short"},
				{hostname: "example.com", path: "/api/v1", service: "long"},
			},
			expected: []routeEntry{
				{hostname: "example.com", path: "/api/v1", service: "long"},
				{hostname: "example.com", path: "/a", service: "short"},
			},
		},
		{
			name: "same length paths sorted alphabetically",
			entries: []routeEntry{
				{hostname: "example.com", path: "/multi-v3", service: "svc3"},
				{hostname: "example.com", path: "/multi-v1", service: "svc1"},
				{hostname: "example.com", path: "/multi-v2", service: "svc2"},
			},
			expected: []routeEntry{
				{hostname: "example.com", path: "/multi-v1", service: "svc1"},
				{hostname: "example.com", path: "/multi-v2", service: "svc2"},
				{hostname: "example.com", path: "/multi-v3", service: "svc3"},
			},
		},
		{
			name: "complex sorting - all criteria",
			entries: []routeEntry{
				{hostname: "*", path: "/", service: "catch-all"},
				{hostname: "api.example.com", path: "/v2", priority: 0, service: "api-v2-prefix"},
				{hostname: "api.example.com", path: "/v1", priority: 0, service: "api-v1-prefix"},
				{hostname: "api.example.com", path: "/v1", priority: 1, service: "api-v1-exact"},
				{hostname: "www.example.com", path: "/about", service: "www-about"},
				{hostname: "www.example.com", path: "/", service: "www-root"},
			},
			expected: []routeEntry{
				{hostname: "api.example.com", path: "/v1", priority: 1, service: "api-v1-exact"},
				{hostname: "api.example.com", path: "/v1", priority: 0, service: "api-v1-prefix"},
				{hostname: "api.example.com", path: "/v2", priority: 0, service: "api-v2-prefix"},
				{hostname: "www.example.com", path: "/about", service: "www-about"},
				{hostname: "www.example.com", path: "/", service: "www-root"},
				{hostname: "*", path: "/", service: "catch-all"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Make a copy to avoid mutating test data
			entries := make([]routeEntry, len(tt.entries))
			copy(entries, tt.entries)

			sortRouteEntries(entries)

			assert.Equal(t, tt.expected, entries)
		})
	}
}

func TestSortRouteEntries_Determinism(t *testing.T) {
	t.Parallel()

	// Run the same sorting multiple times to verify deterministic results
	entries := []routeEntry{
		{hostname: "example.com", path: "/multi-v3", service: "svc3"},
		{hostname: "example.com", path: "/multi-v1", service: "svc1"},
		{hostname: "example.com", path: "/multi-v2", service: "svc2"},
	}

	expected := []routeEntry{
		{hostname: "example.com", path: "/multi-v1", service: "svc1"},
		{hostname: "example.com", path: "/multi-v2", service: "svc2"},
		{hostname: "example.com", path: "/multi-v3", service: "svc3"},
	}

	for i := range 100 {
		entriesCopy := make([]routeEntry, len(entries))
		copy(entriesCopy, entries)

		sortRouteEntries(entriesCopy)

		assert.Equal(t, expected, entriesCopy, "sorting should be deterministic on iteration %d", i)
	}
}
