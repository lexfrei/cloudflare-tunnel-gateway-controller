package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

func acceptedBinding(gatewayKey string) routeBindingInfo {
	return acceptedOnListeners(map[string]string{gatewayListenerKey(gatewayKey, "http"): gatewayKey})
}

// acceptedOnListeners builds a binding with one accepted parent per listener,
// listeners mapping each listener key to the Gateway it belongs to.
func acceptedOnListeners(listeners map[string]string) routeBindingInfo {
	info := routeBindingInfo{
		bindingResults:   map[int]routebinding.BindingResult{},
		acceptedGateways: map[string]bool{},
		parentGateways:   map[int]string{},
		parentListeners:  map[int][]string{},
	}

	idx := 0
	for listener, gateway := range listeners {
		info.bindingResults[idx] = routebinding.BindingResult{Accepted: true, Reason: gatewayv1.RouteReasonAccepted}
		info.acceptedGateways[gateway] = true
		info.parentGateways[idx] = gateway
		info.parentListeners[idx] = []string{listener}
		idx++
	}

	return info
}

func gatewayListenerKey(gatewayKey, section string) string {
	namespace, name, _ := strings.Cut(gatewayKey, "/")

	return parentListenerKey(kindGateway, namespace, name, gatewayv1.SectionName(section))
}

// TestResolveCrossTypeConflicts_DifferentListenersNoConflict pins the
// listener scope of the spec's rule ("attached to a Listener that already has
// another Route of the other type attached"): hostname-less routes attached
// through two different listeners of one Gateway do not conflict, although
// their declared hostnames trivially intersect.
func TestResolveCrossTypeConflicts_DifferentListenersNoConflict(t *testing.T) {
	t.Parallel()

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a-http"}}},
		bindings: map[string]routeBindingInfo{"ns/a-http": acceptedOnListeners(map[string]string{
			gatewayListenerKey("ns/gw", "web"): "ns/gw",
		})},
	}
	grpcResult := &grpcRouteResult{
		accepted: []gatewayv1.GRPCRoute{{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "b-grpc"}}},
		bindings: map[string]routeBindingInfo{"ns/b-grpc": acceptedOnListeners(map[string]string{
			gatewayListenerKey("ns/gw", "rpc"): "ns/gw",
		})},
	}

	resolveCrossTypeConflicts(httpResult, grpcResult)

	assert.Len(t, httpResult.accepted, 1)
	assert.Len(t, grpcResult.accepted, 1)
}

// TestResolveCrossTypeConflicts_ListenerSetEntryIsItsOwnListener pins that a
// ListenerSet entry and a Gateway listener sharing a section name are two
// listeners, so routes attached one through each do not conflict.
func TestResolveCrossTypeConflicts_ListenerSetEntryIsItsOwnListener(t *testing.T) {
	t.Parallel()

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a-http"}}},
		bindings: map[string]routeBindingInfo{"ns/a-http": acceptedOnListeners(map[string]string{
			gatewayListenerKey("ns/gw", "http"): "ns/gw",
		})},
	}
	grpcResult := &grpcRouteResult{
		accepted: []gatewayv1.GRPCRoute{{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "b-grpc"}}},
		bindings: map[string]routeBindingInfo{"ns/b-grpc": acceptedOnListeners(map[string]string{
			parentListenerKey(kindListenerSet, "ns", "gw", "http"): "ns/gw",
		})},
	}

	resolveCrossTypeConflicts(httpResult, grpcResult)

	assert.Len(t, httpResult.accepted, 1)
	assert.Len(t, grpcResult.accepted, 1)
}

// TestResolveCrossTypeConflicts_LoserKeepsUncontestedGateway pins the
// rejection scope: the losing route reports Accepted=False/Conflicted on the
// parent where the conflict is, and stays accepted, and served, on a Gateway
// where the route of the other type is not attached.
func TestResolveCrossTypeConflicts_LoserKeepsUncontestedGateway(t *testing.T) {
	t.Parallel()

	older := metav1.NewTime(time.Unix(1000, 0))
	newer := metav1.NewTime(time.Unix(2000, 0))

	contested := gatewayListenerKey("ns/gw-a", "http")
	httpBinding := acceptedOnListeners(map[string]string{contested: "ns/gw-a"})
	httpBinding.bindingResults[1] = routebinding.BindingResult{Accepted: true, Reason: gatewayv1.RouteReasonAccepted}
	httpBinding.acceptedGateways["ns/gw-b"] = true
	httpBinding.parentGateways[1] = "ns/gw-b"
	httpBinding.parentListeners[1] = []string{gatewayListenerKey("ns/gw-b", "http")}

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a-http", CreationTimestamp: newer},
			Spec:       gatewayv1.HTTPRouteSpec{Hostnames: []gatewayv1.Hostname{"app.example.com"}},
		}},
		bindings: map[string]routeBindingInfo{"ns/a-http": httpBinding},
	}
	grpcResult := &grpcRouteResult{
		accepted: []gatewayv1.GRPCRoute{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "b-grpc", CreationTimestamp: older},
			Spec:       gatewayv1.GRPCRouteSpec{Hostnames: []gatewayv1.Hostname{"app.example.com"}},
		}},
		bindings: map[string]routeBindingInfo{"ns/b-grpc": acceptedOnListeners(map[string]string{contested: "ns/gw-a"})},
	}

	resolveCrossTypeConflicts(httpResult, grpcResult)

	require.Len(t, httpResult.accepted, 1, "the loser is still served through its uncontested Gateway")
	assert.Empty(t, httpResult.rejected)

	loser := httpResult.bindings["ns/a-http"]
	assert.Equal(t, routeReasonConflicted, loser.bindingResults[0].Reason)
	assert.False(t, loser.bindingResults[0].Accepted)
	assert.True(t, loser.bindingResults[1].Accepted)
	assert.Equal(t, map[string]bool{"ns/gw-b": true}, loser.acceptedGateways,
		"the contested Gateway must leave the set its data plane is built from")
	assert.Len(t, grpcResult.accepted, 1)
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

// TestResolveCrossTypeConflicts_LoserOnSameGatewaySiblingListenerIsRejected
// pins the rejection scope on the contested Gateway: a loser parent attached
// through an uncontested listener of that same Gateway is rejected too, since
// the proxy scopes a route per Gateway and would otherwise serve it on the
// contested listener.
func TestResolveCrossTypeConflicts_LoserOnSameGatewaySiblingListenerIsRejected(t *testing.T) {
	t.Parallel()

	older := metav1.NewTime(time.Unix(1000, 0))
	newer := metav1.NewTime(time.Unix(2000, 0))

	contested := gatewayListenerKey("ns/gw", "a")
	httpBinding := acceptedOnListeners(map[string]string{contested: "ns/gw"})
	httpBinding.bindingResults[1] = routebinding.BindingResult{Accepted: true, Reason: gatewayv1.RouteReasonAccepted}
	httpBinding.parentGateways[1] = "ns/gw"
	httpBinding.parentListeners[1] = []string{gatewayListenerKey("ns/gw", "b")}

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a-http", CreationTimestamp: newer}}},
		bindings: map[string]routeBindingInfo{"ns/a-http": httpBinding},
	}
	grpcResult := &grpcRouteResult{
		accepted: []gatewayv1.GRPCRoute{{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "b-grpc", CreationTimestamp: older}}},
		bindings: map[string]routeBindingInfo{"ns/b-grpc": acceptedOnListeners(map[string]string{contested: "ns/gw"})},
	}

	resolveCrossTypeConflicts(httpResult, grpcResult)

	assert.Empty(t, httpResult.accepted)
	require.Len(t, httpResult.rejected, 1)

	loser := httpResult.bindings["ns/a-http"]
	for idx := range 2 {
		assert.False(t, loser.bindingResults[idx].Accepted, "parent %d", idx)
		assert.Equal(t, routeReasonConflicted, loser.bindingResults[idx].Reason, "parent %d", idx)
	}
}

// TestResolveCrossTypeConflicts_IncompleteLoserIsFalse pins that losing a
// conflict is a refusal, reported Accepted=False, even when the parent's
// binding was incomplete because another listener could not be evaluated.
func TestResolveCrossTypeConflicts_IncompleteLoserIsFalse(t *testing.T) {
	t.Parallel()

	older := metav1.NewTime(time.Unix(1000, 0))
	newer := metav1.NewTime(time.Unix(2000, 0))

	contested := gatewayListenerKey("ns/gw", "a")
	httpBinding := acceptedOnListeners(map[string]string{contested: "ns/gw"})
	incomplete := httpBinding.bindingResults[0]
	incomplete.Incomplete = true
	httpBinding.bindingResults[0] = incomplete

	httpResult := &httpRouteResult{
		accepted: []gatewayv1.HTTPRoute{{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "a-http", CreationTimestamp: newer}}},
		bindings: map[string]routeBindingInfo{"ns/a-http": httpBinding},
	}
	grpcResult := &grpcRouteResult{
		accepted: []gatewayv1.GRPCRoute{{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "b-grpc", CreationTimestamp: older}}},
		bindings: map[string]routeBindingInfo{"ns/b-grpc": acceptedOnListeners(map[string]string{contested: "ns/gw"})},
	}

	resolveCrossTypeConflicts(httpResult, grpcResult)

	cond := buildAcceptedCondition(1, metav1.Now(), httpResult.bindings["ns/a-http"], 0, nil, nil)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, string(routeReasonConflicted), cond.Reason)
}

// crossTypeBindSyncer serves one Gateway with two hostname-pinned listeners,
// a and b, to the bind path.
func crossTypeBindSyncer(t *testing.T, objects ...runtime.Object) *RouteSyncer {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	listener := func(name string) gatewayv1.Listener {
		return gatewayv1.Listener{
			Name:     gatewayv1.SectionName(name),
			Hostname: new(gatewayv1.Hostname(name + ".example.com")),
			Port:     80,
			Protocol: gatewayv1.HTTPProtocolType,
		}
	}

	objects = append(objects,
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-tunnel"},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: "cloudflare-tunnel"},
		},
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "default"},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "cloudflare-tunnel",
				Listeners:        []gatewayv1.Listener{listener("a"), listener("b")},
			},
		})

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()

	return NewRouteSyncer(fakeClient, scheme, "cluster.local", "cloudflare-tunnel",
		config.NewResolver(fakeClient, "default", cfmetrics.NewNoopCollector(), verifiedClaims()),
		cfmetrics.NewNoopCollector(), nil)
}

func crossTypeParentRefs(section string) gatewayv1.CommonRouteSpec {
	ref := gatewayv1.ParentReference{Name: "gw"}
	if section != "" {
		ref.SectionName = new(gatewayv1.SectionName(section))
	}

	return gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{ref}}
}

// TestResolveCrossTypeConflicts_BindPathScopesToListener runs the conflict
// over what the bind path records: hostname-less routes pinned to different
// listeners of one Gateway both stay accepted, and the same routes on one
// shared listener conflict.
func TestResolveCrossTypeConflicts_BindPathScopesToListener(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                     string
		httpSection, grpcSection string
		wantConflict             bool
	}{
		{name: "different listeners", httpSection: "a", grpcSection: "b"},
		{name: "same listener", httpSection: "a", grpcSection: "a", wantConflict: true},
		{name: "every listener", wantConflict: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			older := metav1.NewTime(time.Unix(1000, 0))
			newer := metav1.NewTime(time.Unix(2000, 0))
			syncer := crossTypeBindSyncer(t,
				&gatewayv1.HTTPRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", CreationTimestamp: newer},
					Spec:       gatewayv1.HTTPRouteSpec{CommonRouteSpec: crossTypeParentRefs(tt.httpSection)},
				},
				&gatewayv1.GRPCRoute{
					ObjectMeta: metav1.ObjectMeta{Name: "rpc", Namespace: "default", CreationTimestamp: older},
					Spec:       gatewayv1.GRPCRouteSpec{CommonRouteSpec: crossTypeParentRefs(tt.grpcSection)},
				})

			httpResult, err := syncer.getRelevantHTTPRoutes(context.Background(), nil)
			require.NoError(t, err)
			grpcResult, err := syncer.getRelevantGRPCRoutes(context.Background(), nil)
			require.NoError(t, err)
			require.Len(t, httpResult.accepted, 1)
			require.Len(t, grpcResult.accepted, 1)

			resolveCrossTypeConflicts(httpResult, grpcResult)

			assert.Len(t, grpcResult.accepted, 1, "the older route always stays")

			if tt.wantConflict {
				assert.Empty(t, httpResult.accepted)
				assert.Equal(t, routeReasonConflicted, httpResult.bindings["default/web"].bindingResults[0].Reason)
			} else {
				assert.Len(t, httpResult.accepted, 1)
			}
		})
	}
}
