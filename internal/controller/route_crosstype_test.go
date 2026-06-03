package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

func acceptedBinding(gatewayKey string) routeBindingInfo {
	return routeBindingInfo{
		bindingResults:   map[int]routebinding.BindingResult{0: {Accepted: true, Reason: gatewayv1.RouteReasonAccepted}},
		acceptedGateways: map[string]bool{gatewayKey: true},
	}
}

// TestResolveCrossTypeConflicts_OlderWins pins the Gateway API cross-route-type
// rule (grpcroute_types.go:129-138): an HTTPRoute and a GRPCRoute on the same
// Gateway with intersecting hostnames must resolve to exactly one accepted route
// — the oldest by creationTimestamp — and the loser gets Accepted=False/Conflicted.
func TestResolveCrossTypeConflicts_OlderWins(t *testing.T) {
	t.Parallel()

	older := metav1.NewTime(time.Unix(1000, 0))
	newer := metav1.NewTime(time.Unix(2000, 0))

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a-http", CreationTimestamp: newer},
			Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{"app.example.com"}},
		}},
		bindings: map[string]routeBindingInfo{"ns/a-http": acceptedBinding("ns/gw")},
	}
	grpcResult := &grpcRouteResult{
		accepted: []gatewayv1.GRPCRoute{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "b-grpc", CreationTimestamp: older},
			Spec:       gatewayv1.GRPCRouteSpec{Hostnames: []gatewayv1.Hostname{"app.example.com"}},
		}},
		bindings: map[string]routeBindingInfo{"ns/b-grpc": acceptedBinding("ns/gw")},
	}

	resolveCrossTypeConflicts(httpResult, grpcResult)

	// The newer HTTPRoute loses and is rejected with Conflicted.
	assert.Empty(t, httpResult.accepted, "the newer route must be removed from accepted")
	require.Len(t, httpResult.rejected, 1)
	assert.Equal(t, "a-http", httpResult.rejected[0].Name)

	loser := httpResult.bindings["ns/a-http"]
	require.Contains(t, loser.bindingResults, 0)
	assert.False(t, loser.bindingResults[0].Accepted)
	assert.Equal(t, "Conflicted", string(loser.bindingResults[0].Reason))

	// The older GRPCRoute wins and stays accepted.
	require.Len(t, grpcResult.accepted, 1)
	assert.Empty(t, grpcResult.rejected)
}

// TestResolveCrossTypeConflicts_AlphabeticalTiebreak pins the spec's second
// tiebreak: when two conflicting routes share a creationTimestamp, the one first
// alphabetically by {namespace}/{name} is accepted and the other rejected.
func TestResolveCrossTypeConflicts_AlphabeticalTiebreak(t *testing.T) {
	t.Parallel()

	same := metav1.NewTime(time.Unix(1000, 0))

	// Equal timestamps: "ns/a-http" sorts before "ns/z-grpc", so the GRPCRoute loses.
	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a-http", CreationTimestamp: same},
			Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{"app.example.com"}},
		}},
		bindings: map[string]routeBindingInfo{"ns/a-http": acceptedBinding("ns/gw")},
	}
	grpcResult := &grpcRouteResult{
		accepted: []gatewayv1.GRPCRoute{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "z-grpc", CreationTimestamp: same},
			Spec:       gatewayv1.GRPCRouteSpec{Hostnames: []gatewayv1.Hostname{"app.example.com"}},
		}},
		bindings: map[string]routeBindingInfo{"ns/z-grpc": acceptedBinding("ns/gw")},
	}

	resolveCrossTypeConflicts(httpResult, grpcResult)

	assert.Len(t, httpResult.accepted, 1, "the alphabetically-first route (HTTPRoute) must win")
	assert.Empty(t, httpResult.rejected)
	assert.Empty(t, grpcResult.accepted)
	require.Len(t, grpcResult.rejected, 1)
	assert.Equal(t, "Conflicted", string(grpcResult.bindings["ns/z-grpc"].bindingResults[0].Reason))
}

// TestResolveCrossTypeConflicts_EmptyHostnameMatchesAll pins that a route with
// no hostnames (which inherits the listener's hostnames, matching all) conflicts
// with any same-Gateway route of the other type.
func TestResolveCrossTypeConflicts_EmptyHostnameMatchesAll(t *testing.T) {
	t.Parallel()

	older := metav1.NewTime(time.Unix(1000, 0))
	newer := metav1.NewTime(time.Unix(2000, 0))

	// The HTTPRoute has no hostnames (matches all) and is newer, so it loses to
	// the older GRPCRoute that names a specific host — empty intersects with anything.
	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a-http", CreationTimestamp: newer},
			Spec:       gatewayv1.HTTPRouteSpec{},
		}},
		bindings: map[string]routeBindingInfo{"ns/a-http": acceptedBinding("ns/gw")},
	}
	grpcResult := &grpcRouteResult{
		accepted: []gatewayv1.GRPCRoute{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "b-grpc", CreationTimestamp: older},
			Spec:       gatewayv1.GRPCRouteSpec{Hostnames: []gatewayv1.Hostname{"app.example.com"}},
		}},
		bindings: map[string]routeBindingInfo{"ns/b-grpc": acceptedBinding("ns/gw")},
	}

	resolveCrossTypeConflicts(httpResult, grpcResult)

	assert.Empty(t, httpResult.accepted, "a hostname-less route matches all and conflicts")
	require.Len(t, httpResult.rejected, 1)
	require.Len(t, grpcResult.accepted, 1)
}

// TestHostnameMatches pins the wildcard hostname-intersection logic that drives
// cross-type conflict scoping.
func TestHostnameMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		left, right string
		want        bool
	}{
		{"exact equal", "app.example.com", "app.example.com", true},
		{"exact different", "a.example.com", "b.example.com", false},
		{"wildcard covers subdomain", "*.example.com", "app.example.com", true},
		{"wildcard misses apex", "*.example.com", "example.com", false},
		{"wildcard different domain", "*.example.com", "app.example.org", false},
		{"exact under right wildcard", "app.example.com", "*.example.com", true},
		{"nested wildcards overlap", "*.example.com", "*.sub.example.com", true},
		{"disjoint wildcards", "*.example.com", "*.example.org", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, hostnameMatches(tt.left, tt.right))
		})
	}
}

// TestResolveCrossTypeConflicts_NoIntersectionNoConflict pins that routes whose
// hostnames do not intersect both stay accepted.
func TestResolveCrossTypeConflicts_NoIntersectionNoConflict(t *testing.T) {
	t.Parallel()

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a-http"},
			Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{"http.example.com"}},
		}},
		bindings: map[string]routeBindingInfo{"ns/a-http": acceptedBinding("ns/gw")},
	}
	grpcResult := &grpcRouteResult{
		accepted: []gatewayv1.GRPCRoute{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "b-grpc"},
			Spec:       gatewayv1.GRPCRouteSpec{Hostnames: []gatewayv1.Hostname{"grpc.example.com"}},
		}},
		bindings: map[string]routeBindingInfo{"ns/b-grpc": acceptedBinding("ns/gw")},
	}

	resolveCrossTypeConflicts(httpResult, grpcResult)

	assert.Len(t, httpResult.accepted, 1)
	assert.Empty(t, httpResult.rejected)
	assert.Len(t, grpcResult.accepted, 1)
	assert.Empty(t, grpcResult.rejected)
}

// TestResolveCrossTypeConflicts_DifferentGatewaysNoConflict pins that an
// HTTPRoute and a GRPCRoute on different Gateways do not conflict even when their
// hostnames intersect — there is no shared attachment point.
func TestResolveCrossTypeConflicts_DifferentGatewaysNoConflict(t *testing.T) {
	t.Parallel()

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a-http"},
			Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{"app.example.com"}},
		}},
		bindings: map[string]routeBindingInfo{"ns/a-http": acceptedBinding("ns/gw-a")},
	}
	grpcResult := &grpcRouteResult{
		accepted: []gatewayv1.GRPCRoute{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "b-grpc"},
			Spec:       gatewayv1.GRPCRouteSpec{Hostnames: []gatewayv1.Hostname{"app.example.com"}},
		}},
		bindings: map[string]routeBindingInfo{"ns/b-grpc": acceptedBinding("ns/gw-b")},
	}

	resolveCrossTypeConflicts(httpResult, grpcResult)

	assert.Len(t, httpResult.accepted, 1)
	assert.Len(t, grpcResult.accepted, 1)
	assert.Empty(t, httpResult.rejected)
	assert.Empty(t, grpcResult.rejected)
}
