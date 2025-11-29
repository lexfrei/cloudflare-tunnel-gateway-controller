package routebinding

import (
	"testing"

	"github.com/stretchr/testify/assert"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestIsRouteKindAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		allowedRoutes *gatewayv1.AllowedRoutes
		protocol      gatewayv1.ProtocolType
		routeKind     gatewayv1.Kind
		expected      bool
	}{
		{
			name:          "nil allowedRoutes HTTP protocol allows HTTPRoute",
			allowedRoutes: nil,
			protocol:      gatewayv1.HTTPProtocolType,
			routeKind:     "HTTPRoute",
			expected:      true,
		},
		{
			name:          "nil allowedRoutes HTTP protocol allows GRPCRoute",
			allowedRoutes: nil,
			protocol:      gatewayv1.HTTPProtocolType,
			routeKind:     "GRPCRoute",
			expected:      true,
		},
		{
			name:          "nil allowedRoutes HTTPS protocol allows HTTPRoute",
			allowedRoutes: nil,
			protocol:      gatewayv1.HTTPSProtocolType,
			routeKind:     "HTTPRoute",
			expected:      true,
		},
		{
			name:          "nil allowedRoutes HTTPS protocol allows GRPCRoute",
			allowedRoutes: nil,
			protocol:      gatewayv1.HTTPSProtocolType,
			routeKind:     "GRPCRoute",
			expected:      true,
		},
		{
			name: "empty kinds uses protocol defaults - HTTP allows HTTPRoute",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: nil,
			},
			protocol:  gatewayv1.HTTPProtocolType,
			routeKind: "HTTPRoute",
			expected:  true,
		},
		{
			name: "empty kinds uses protocol defaults - HTTP allows GRPCRoute",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: nil,
			},
			protocol:  gatewayv1.HTTPProtocolType,
			routeKind: "GRPCRoute",
			expected:  true,
		},
		{
			name: "explicit HTTPRoute only allows HTTPRoute",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: []gatewayv1.RouteGroupKind{
					{
						Group: groupPtr(gatewayv1.GroupName),
						Kind:  "HTTPRoute",
					},
				},
			},
			protocol:  gatewayv1.HTTPProtocolType,
			routeKind: "HTTPRoute",
			expected:  true,
		},
		{
			name: "explicit HTTPRoute only rejects GRPCRoute",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: []gatewayv1.RouteGroupKind{
					{
						Group: groupPtr(gatewayv1.GroupName),
						Kind:  "HTTPRoute",
					},
				},
			},
			protocol:  gatewayv1.HTTPProtocolType,
			routeKind: "GRPCRoute",
			expected:  false,
		},
		{
			name: "explicit GRPCRoute only allows GRPCRoute",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: []gatewayv1.RouteGroupKind{
					{
						Group: groupPtr(gatewayv1.GroupName),
						Kind:  "GRPCRoute",
					},
				},
			},
			protocol:  gatewayv1.HTTPProtocolType,
			routeKind: "GRPCRoute",
			expected:  true,
		},
		{
			name: "explicit GRPCRoute only rejects HTTPRoute",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: []gatewayv1.RouteGroupKind{
					{
						Group: groupPtr(gatewayv1.GroupName),
						Kind:  "GRPCRoute",
					},
				},
			},
			protocol:  gatewayv1.HTTPProtocolType,
			routeKind: "HTTPRoute",
			expected:  false,
		},
		{
			name: "both HTTPRoute and GRPCRoute allowed",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: []gatewayv1.RouteGroupKind{
					{
						Group: groupPtr(gatewayv1.GroupName),
						Kind:  "HTTPRoute",
					},
					{
						Group: groupPtr(gatewayv1.GroupName),
						Kind:  "GRPCRoute",
					},
				},
			},
			protocol:  gatewayv1.HTTPProtocolType,
			routeKind: "HTTPRoute",
			expected:  true,
		},
		{
			name: "nil group defaults to gateway API group",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: []gatewayv1.RouteGroupKind{
					{
						Group: nil,
						Kind:  "HTTPRoute",
					},
				},
			},
			protocol:  gatewayv1.HTTPProtocolType,
			routeKind: "HTTPRoute",
			expected:  true,
		},
		{
			name: "empty group defaults to gateway API group",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: []gatewayv1.RouteGroupKind{
					{
						Group: groupPtr(""),
						Kind:  "HTTPRoute",
					},
				},
			},
			protocol:  gatewayv1.HTTPProtocolType,
			routeKind: "HTTPRoute",
			expected:  true,
		},
		{
			name: "different group rejects route",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: []gatewayv1.RouteGroupKind{
					{
						Group: groupPtr("custom.example.com"),
						Kind:  "HTTPRoute",
					},
				},
			},
			protocol:  gatewayv1.HTTPProtocolType,
			routeKind: "HTTPRoute",
			expected:  false,
		},
		{
			name:          "TLS protocol allows TLSRoute by default",
			allowedRoutes: nil,
			protocol:      gatewayv1.TLSProtocolType,
			routeKind:     "TLSRoute",
			expected:      true,
		},
		{
			name:          "TLS protocol rejects HTTPRoute by default",
			allowedRoutes: nil,
			protocol:      gatewayv1.TLSProtocolType,
			routeKind:     "HTTPRoute",
			expected:      false,
		},
		{
			name:          "TCP protocol allows TCPRoute by default",
			allowedRoutes: nil,
			protocol:      gatewayv1.TCPProtocolType,
			routeKind:     "TCPRoute",
			expected:      true,
		},
		{
			name:          "UDP protocol allows UDPRoute by default",
			allowedRoutes: nil,
			protocol:      gatewayv1.UDPProtocolType,
			routeKind:     "UDPRoute",
			expected:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := IsRouteKindAllowed(tt.allowedRoutes, tt.protocol, tt.routeKind)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func groupPtr(g gatewayv1.Group) *gatewayv1.Group {
	return &g
}
