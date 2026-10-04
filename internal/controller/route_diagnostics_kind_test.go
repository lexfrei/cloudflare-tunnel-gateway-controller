package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestStatusEntries_DiagnosticsMatchRouteKind covers an HTTPRoute and a
// GRPCRoute with the same namespace and name, where only the GRPCRoute has a
// diagnostic. The HTTPRoute must not pick it up.
func TestStatusEntries_DiagnosticsMatchRouteKind(t *testing.T) {
	t.Parallel()

	meta := metav1.ObjectMeta{Name: "same", Namespace: "default"}
	result := &SyncResult{
		HTTPRoutes: []gatewayv1.HTTPRoute{{ObjectMeta: meta}},
		GRPCRoutes: []gatewayv1.GRPCRoute{{ObjectMeta: meta}},
	}

	diags := []proxy.RouteDiagnostic{{
		Kind: kindGRPCRouteDiag, Namespace: "default", Name: "same",
		Target: proxy.DiagnosticProxyConfigPush, Reason: routeReasonParentNotEvaluated, Message: "grpc only",
	}}

	httpEntries := result.httpStatusEntries(diags, nil)
	grpcEntries := result.grpcStatusEntries(diags, nil)

	require.Len(t, httpEntries, 1)
	require.Len(t, grpcEntries, 1)
	assert.Empty(t, httpEntries[0].diagnostics, "a GRPCRoute's diagnostic must not land on the HTTPRoute of the same name")
	assert.Len(t, grpcEntries[0].diagnostics, 1)
}

// TestPartitionRouteDiagnostics_CarryRouteKind pins the kind on the
// controller-synthesized per-partition diagnostics.
func TestPartitionRouteDiagnostics_CarryRouteKind(t *testing.T) {
	t.Parallel()

	meta := metav1.ObjectMeta{Name: "same", Namespace: "default"}
	partition := &routePartition{
		HTTPRoutes: []gatewayv1.HTTPRoute{{ObjectMeta: meta}},
		GRPCRoutes: []gatewayv1.GRPCRoute{{ObjectMeta: meta}},
	}

	diags := partitionRouteDiagnostics(partition, proxy.DiagnosticTunnelShared, "reason", "message")
	require.Len(t, diags, 2)
	assert.ElementsMatch(t, []string{kindHTTPRouteDiag, kindGRPCRouteDiag}, []string{diags[0].Kind, diags[1].Kind})
}

// TestReportUndecidedParent_CarriesRouteKind pins the kind on the diagnostic
// for a route left out because a parent could not be evaluated.
func TestReportUndecidedParent_CarriesRouteKind(t *testing.T) {
	t.Parallel()

	ourHost := gatewayv1.Hostname("ours.example.com")
	cli := failingGetClient(t, &gatewayv1.Gateway{}, gatewayUnderClass("ours", "our-class", &ourHost))

	httpRoute := httpRouteTo()
	httpRoute.Spec.ParentRefs = parentRefsToGateways("ours")

	grpcRoute := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "team"},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: parentRefsToGateways("ours")},
		},
	}

	_, httpDiags, _ := withEffectiveHostnames(context.Background(), cli, "", []*gatewayv1.HTTPRoute{httpRoute}, nil, nil)
	_, grpcDiags, _ := withEffectiveHostnamesGRPC(context.Background(), cli, "", []*gatewayv1.GRPCRoute{grpcRoute}, nil, nil)

	require.Len(t, httpDiags, 1)
	require.Len(t, grpcDiags, 1)
	assert.Equal(t, kindHTTPRouteDiag, httpDiags[0].Kind)
	assert.Equal(t, kindGRPCRouteDiag, grpcDiags[0].Kind)
}
