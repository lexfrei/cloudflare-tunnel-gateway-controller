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

func TestFilterSupportedKinds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		allowedRoutes     *gatewayv1.AllowedRoutes
		protocol          gatewayv1.ProtocolType
		expectedKinds     []gatewayv1.Kind
		expectedHasAny    bool
		expectedHasInvald bool
	}{
		{
			name:              "nil allowedRoutes HTTP returns HTTPRoute and GRPCRoute",
			allowedRoutes:     nil,
			protocol:          gatewayv1.HTTPProtocolType,
			expectedKinds:     []gatewayv1.Kind{"HTTPRoute", "GRPCRoute"},
			expectedHasAny:    true,
			expectedHasInvald: false,
		},
		{
			name:              "nil allowedRoutes HTTPS returns HTTPRoute and GRPCRoute",
			allowedRoutes:     nil,
			protocol:          gatewayv1.HTTPSProtocolType,
			expectedKinds:     []gatewayv1.Kind{"HTTPRoute", "GRPCRoute"},
			expectedHasAny:    true,
			expectedHasInvald: false,
		},
		{
			name:              "nil allowedRoutes TLS returns empty - TLSRoute not supported but not invalid",
			allowedRoutes:     nil,
			protocol:          gatewayv1.TLSProtocolType,
			expectedKinds:     nil,
			expectedHasAny:    false,
			expectedHasInvald: false, // Default kinds don't count as invalid
		},
		{
			name:              "nil allowedRoutes TCP returns empty - TCPRoute not supported but not invalid",
			allowedRoutes:     nil,
			protocol:          gatewayv1.TCPProtocolType,
			expectedKinds:     nil,
			expectedHasAny:    false,
			expectedHasInvald: false, // Default kinds don't count as invalid
		},
		{
			name:              "nil allowedRoutes UDP returns empty - UDPRoute not supported but not invalid",
			allowedRoutes:     nil,
			protocol:          gatewayv1.UDPProtocolType,
			expectedKinds:     nil,
			expectedHasAny:    false,
			expectedHasInvald: false, // Default kinds don't count as invalid
		},
		{
			name: "explicit HTTPRoute only",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: []gatewayv1.RouteGroupKind{
					{
						Group: groupPtr(gatewayv1.GroupName),
						Kind:  "HTTPRoute",
					},
				},
			},
			protocol:          gatewayv1.HTTPProtocolType,
			expectedKinds:     []gatewayv1.Kind{"HTTPRoute"},
			expectedHasAny:    true,
			expectedHasInvald: false,
		},
		{
			name: "explicit GRPCRoute only",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: []gatewayv1.RouteGroupKind{
					{
						Group: groupPtr(gatewayv1.GroupName),
						Kind:  "GRPCRoute",
					},
				},
			},
			protocol:          gatewayv1.HTTPProtocolType,
			expectedKinds:     []gatewayv1.Kind{"GRPCRoute"},
			expectedHasAny:    true,
			expectedHasInvald: false,
		},
		{
			name: "explicit unsupported TLSRoute only returns empty and marks invalid",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: []gatewayv1.RouteGroupKind{
					{
						Group: groupPtr(gatewayv1.GroupName),
						Kind:  "TLSRoute",
					},
				},
			},
			protocol:          gatewayv1.HTTPSProtocolType,
			expectedKinds:     nil,
			expectedHasAny:    false,
			expectedHasInvald: true, // Explicitly specified invalid kind
		},
		{
			name: "mixed supported and unsupported kinds filters correctly and marks invalid",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: []gatewayv1.RouteGroupKind{
					{
						Group: groupPtr(gatewayv1.GroupName),
						Kind:  "HTTPRoute",
					},
					{
						Group: groupPtr(gatewayv1.GroupName),
						Kind:  "TLSRoute",
					},
					{
						Group: groupPtr(gatewayv1.GroupName),
						Kind:  "GRPCRoute",
					},
				},
			},
			protocol:          gatewayv1.HTTPProtocolType,
			expectedKinds:     []gatewayv1.Kind{"HTTPRoute", "GRPCRoute"},
			expectedHasAny:    true,
			expectedHasInvald: true, // TLSRoute is explicitly specified and invalid
		},
		{
			name: "different group is not supported and marks invalid",
			allowedRoutes: &gatewayv1.AllowedRoutes{
				Kinds: []gatewayv1.RouteGroupKind{
					{
						Group: groupPtr("custom.example.com"),
						Kind:  "HTTPRoute",
					},
				},
			},
			protocol:          gatewayv1.HTTPProtocolType,
			expectedKinds:     nil,
			expectedHasAny:    false,
			expectedHasInvald: true, // Explicitly specified with wrong group
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kinds, hasAny, hasInvalid := FilterSupportedKinds(tt.allowedRoutes, tt.protocol)

			assert.Equal(t, tt.expectedHasAny, hasAny, "hasAny mismatch")
			assert.Equal(t, tt.expectedHasInvald, hasInvalid, "hasInvalid mismatch")

			if tt.expectedKinds == nil {
				assert.Empty(t, kinds)
			} else {
				assert.Len(t, kinds, len(tt.expectedKinds))
				for i, expectedKind := range tt.expectedKinds {
					assert.Equal(t, expectedKind, kinds[i].Kind)
				}
			}
		})
	}
}
