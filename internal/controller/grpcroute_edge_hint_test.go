package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/routebinding"
)

// grpcEdgeHintSubstring is the load-bearing phrase the breadcrumb Event must
// carry so an operator scanning `kubectl describe grpcroute` lands on the
// dashboard toggle. Pinned here so the tests fail loudly if the message is
// reworded into something that no longer names the fix.
const grpcEdgeHintSubstring = "Network → gRPC"

// acceptedGRPCBinding is a routeBindingInfo with one managed parent accepted —
// the shape a GRPCRoute carries once it has bound to a managed Gateway (the only
// state in which the controller programs it and the edge breadcrumb applies).
func acceptedGRPCBinding() routeBindingInfo {
	return routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: true, Reason: gatewayv1.RouteReasonAccepted, Message: "Route accepted"},
		},
	}
}

// TestGRPCRouteReconciler_updateRouteStatus_EmitsEdgeHint pins that an accepted
// GRPCRoute on a gRPC-capable transport (auto/unset/http2) gets a non-fatal
// breadcrumb Event reminding the operator to enable Cloudflare zone gRPC
// proxying. The edge 403 for application/grpc happens upstream of the tunnel, so
// this Event is the only in-cluster signal the operator has — without it the
// route looks healthy while every gRPC call fails.
func TestGRPCRouteReconciler_updateRouteStatus_EmitsEdgeHint(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.GRPCRoute{ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "default"}}
	scheme, builder := eventDiagSchemeAndClient(t)
	cli := builder.WithObjects(route).WithStatusSubresource(route).Build()

	rec := events.NewFakeRecorder(5)
	r := &GRPCRouteReconciler{
		Client:         cli,
		Scheme:         scheme,
		ControllerName: "test-controller",
		Recorder:       rec,
		// TunnelProtocol left empty (auto/unset) — the gRPC-capable path.
	}

	require.NoError(t, r.updateRouteStatus(context.Background(), route, acceptedGRPCBinding(), nil, nil, nil))

	got := drainEvents(rec)

	var hints []string

	for _, e := range got {
		if strings.Contains(e, grpcEdgeHintSubstring) {
			hints = append(hints, e)
		}
	}

	require.Len(t, hints, 1, "an accepted GRPCRoute on a gRPC-capable transport must emit exactly one edge-toggle breadcrumb")
	assert.Contains(t, hints[0], "gRPC proxying", "the breadcrumb must name the prerequisite, not just the dashboard path")
}

// TestGRPCRouteReconciler_updateRouteStatus_NoEdgeHintOnExplicitQUIC pins that
// the breadcrumb is suppressed when the transport is explicitly quic: that route
// is already rejected Accepted=False / UnsupportedProtocol (gRPC cannot work over
// QUIC at all), so the edge-toggle reminder would be misleading noise on a route
// that fails for a different, already-surfaced reason.
func TestGRPCRouteReconciler_updateRouteStatus_NoEdgeHintOnExplicitQUIC(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.GRPCRoute{ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "default"}}
	scheme, builder := eventDiagSchemeAndClient(t)
	cli := builder.WithObjects(route).WithStatusSubresource(route).Build()

	rec := events.NewFakeRecorder(5)
	r := &GRPCRouteReconciler{
		Client:         cli,
		Scheme:         scheme,
		ControllerName: "test-controller",
		Recorder:       rec,
		TunnelProtocol: "quic",
	}

	require.NoError(t, r.updateRouteStatus(context.Background(), route, acceptedGRPCBinding(), nil, nil, nil))

	for _, e := range drainEvents(rec) {
		assert.NotContains(t, e, grpcEdgeHintSubstring,
			"an explicit-quic GRPCRoute must not get the edge-toggle breadcrumb (it is rejected for a different reason)")
	}
}

// TestGRPCRouteReconciler_updateRouteStatus_NoEdgeHintOnBindingRejected pins that
// the breadcrumb is suppressed for a GRPCRoute that bound to a managed Gateway
// but was rejected at binding validation (Accepted=False / e.g. NoMatchingParent).
// updateRouteStatus runs over RejectedGRPCRoutes too, so without an acceptance
// guard the hint would fire on a route that is provably not Accepted — its
// message would claim the route is Accepted and send the operator to flip an
// unrelated edge setting.
func TestGRPCRouteReconciler_updateRouteStatus_NoEdgeHintOnBindingRejected(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.GRPCRoute{ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "default"}}
	scheme, builder := eventDiagSchemeAndClient(t)
	cli := builder.WithObjects(route).WithStatusSubresource(route).Build()

	rec := events.NewFakeRecorder(5)
	r := &GRPCRouteReconciler{
		Client:         cli,
		Scheme:         scheme,
		ControllerName: "test-controller",
		Recorder:       rec,
	}

	rejected := routeBindingInfo{
		bindingResults: map[int]routebinding.BindingResult{
			0: {Accepted: false, Reason: gatewayv1.RouteReasonNoMatchingParent, Message: "no matching parent"},
		},
	}

	require.NoError(t, r.updateRouteStatus(context.Background(), route, rejected, nil, nil, nil))

	for _, e := range drainEvents(rec) {
		assert.NotContains(t, e, grpcEdgeHintSubstring,
			"a binding-rejected GRPCRoute (Accepted=False) must not get the edge-toggle breadcrumb")
	}
}

// TestGRPCRouteReconciler_updateRouteStatus_NoEdgeHintOnSyncError pins that the
// breadcrumb is suppressed when the sync failed: the route is Accepted=False /
// Pending, so the "this route is Accepted" framing of the hint would be wrong.
func TestGRPCRouteReconciler_updateRouteStatus_NoEdgeHintOnSyncError(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.GRPCRoute{ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "default"}}
	scheme, builder := eventDiagSchemeAndClient(t)
	cli := builder.WithObjects(route).WithStatusSubresource(route).Build()

	rec := events.NewFakeRecorder(5)
	r := &GRPCRouteReconciler{
		Client:         cli,
		Scheme:         scheme,
		ControllerName: "test-controller",
		Recorder:       rec,
	}

	require.NoError(t, r.updateRouteStatus(context.Background(), route, acceptedGRPCBinding(), nil, nil, errCloudflareSync))

	for _, e := range drainEvents(rec) {
		assert.NotContains(t, e, grpcEdgeHintSubstring,
			"a GRPCRoute pending on a sync error must not get the edge-toggle breadcrumb")
	}
}

// TestGRPCRouteReconciler_updateRouteStatus_NoEdgeHintWhenEveryRuleUnservable
// pins that the breadcrumb is suppressed when converter diagnostics mark every
// rule of the route wholly unservable. That flips the route to
// Accepted=False / UnsupportedValue even with no sync error and a bound parent,
// so — like the binding-rejected and sync-error cases — the edge reminder would
// land on a route the same status update reports as not Accepted.
func TestGRPCRouteReconciler_updateRouteStatus_NoEdgeHintWhenEveryRuleUnservable(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "default"},
		Spec:       gatewayv1.GRPCRouteSpec{Rules: []gatewayv1.GRPCRouteRule{{}}},
	}
	scheme, builder := eventDiagSchemeAndClient(t)
	cli := builder.WithObjects(route).WithStatusSubresource(route).Build()

	rec := events.NewFakeRecorder(5)
	r := &GRPCRouteReconciler{
		Client:         cli,
		Scheme:         scheme,
		ControllerName: "test-controller",
		Recorder:       rec,
	}

	// The route's only rule is wholly unservable, so len(wholeRuleIdx) >= ruleCount
	// and diagnosticConditions yields an Accepted=False/UnsupportedValue override.
	diagnostics := []proxy.RouteDiagnostic{
		{Namespace: "default", Name: "grpc", Target: proxy.DiagnosticAccepted, WholeRule: true, RuleIndex: 0, Message: "gRPC rule filters are not supported"},
	}

	require.NoError(t, r.updateRouteStatus(context.Background(), route, acceptedGRPCBinding(), nil, diagnostics, nil))

	for _, e := range drainEvents(rec) {
		assert.NotContains(t, e, grpcEdgeHintSubstring,
			"a GRPCRoute whose every rule is unservable (Accepted=False) must not get the edge-toggle breadcrumb")
	}
}

// TestEmitGRPCEdgeHint_NilRecorderNoPanic pins the no-op-safety contract: a
// reconciler constructed without a Recorder (e.g. in a unit test) must not panic
// when the breadcrumb path runs. Mirrors emitDiagnosticEvents' nil guard.
func TestEmitGRPCEdgeHint_NilRecorderNoPanic(t *testing.T) {
	t.Parallel()

	route := &gatewayv1.GRPCRoute{ObjectMeta: metav1.ObjectMeta{Name: "grpc", Namespace: "default"}}

	assert.NotPanics(t, func() { emitGRPCEdgeHint(nil, route) })
}
