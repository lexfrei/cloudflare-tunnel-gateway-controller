package proxy_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestConvertHTTPRoutes_PopulatesProvenance pins the parallel-slice contract
// DetectShadowedRules depends on: every flattened rule carries its source
// route's identity, in flattening (precedence) order.
func TestConvertHTTPRoutes_PopulatesProvenance(t *testing.T) {
	t.Parallel()

	older := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	newer := metav1.NewTime(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))

	// Listed newest-first to prove provenance follows precedence sorting, not
	// input order.
	routes := []*gatewayv1.HTTPRoute{
		crossRouteHTTPRoute("team-b", "newer", newer, "app.example.com", "svc-b"),
		crossRouteHTTPRoute("team-a", "older", older, "app.example.com", "svc-a"),
	}

	cfg := proxy.ConvertHTTPRoutes(t.Context(), routes, "cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Provenance, len(cfg.Rules), "Provenance must parallel Rules")
	require.Len(t, cfg.Provenance, 2)

	assert.Equal(t, proxy.RuleProvenance{
		Kind: "HTTPRoute", Namespace: "team-a", Name: "older",
		CreationTimestamp: older, RuleIndex: 0,
	}, cfg.Provenance[0], "the older route's rule must be flattened (and attributed) first")
	assert.Equal(t, "newer", cfg.Provenance[1].Name)
}

// TestConvertGRPCRoutes_PopulatesProvenance pins the same contract for the
// gRPC converter, with Kind=GRPCRoute.
func TestConvertGRPCRoutes_PopulatesProvenance(t *testing.T) {
	t.Parallel()

	created := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	service := "pkg.Svc"
	method := "Do"
	port := gatewayv1.PortNumber(50051)

	route := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: "grpc-ns", Name: "grpc-route", CreationTimestamp: created},
		Spec: gatewayv1.GRPCRouteSpec{
			Hostnames: []gatewayv1.Hostname{"grpc.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{
				{
					Matches: []gatewayv1.GRPCRouteMatch{
						{Method: &gatewayv1.GRPCMethodMatch{Service: &service, Method: &method}},
					},
					BackendRefs: []gatewayv1.GRPCBackendRef{
						{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: "grpc-svc", Port: &port,
						}}},
					},
				},
			},
		},
	}

	cfg := proxy.ConvertGRPCRoutes(t.Context(), []*gatewayv1.GRPCRoute{route}, "cluster.local", nil, nil, nil, nil)

	require.Len(t, cfg.Provenance, len(cfg.Rules))
	require.Len(t, cfg.Provenance, 1)
	assert.Equal(t, "GRPCRoute", cfg.Provenance[0].Kind)
	assert.Equal(t, "grpc-ns", cfg.Provenance[0].Namespace)
}
