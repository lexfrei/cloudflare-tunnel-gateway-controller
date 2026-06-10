package controller

// Benchmark for the per-sync rebuild cost at fleet scale: the controller
// rebuilds the full desired document from the informer cache on every route
// change (the Cloudflare configurations endpoint is whole-document, so a
// partial rebuild would not save an API byte). This pins the controller-side
// cost of that O(N) rebuild at N=500 routes so a regression in the build or
// diff path is measurable.

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/cloudflare/cloudflare-go/v7/zero_trust"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
)

// benchmarkRoutes builds n single-rule HTTPRoutes with distinct hostnames.
func benchmarkRoutes(n int) []gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)

	routes := make([]gatewayv1.HTTPRoute, 0, n)

	for i := range n {
		path := fmt.Sprintf("/app-%d", i)
		routes = append(routes, gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("route-%d", i), Namespace: "default"},
			Spec: gatewayv1.HTTPRouteSpec{
				Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(fmt.Sprintf("app-%d.example.com", i))},
				Rules: []gatewayv1.HTTPRouteRule{{
					Matches: []gatewayv1.HTTPRouteMatch{
						{Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: &path}},
					},
					BackendRefs: []gatewayv1.HTTPBackendRef{{
						BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{
								Name: gatewayv1.ObjectName(fmt.Sprintf("svc-%d", i)),
								Port: &port,
							},
						},
					}},
				}},
			},
		})
	}

	return routes
}

// BenchmarkRebuildAndDiff_500Routes measures the full controller-side rebuild
// for one sync at a 500-route fleet: ingress-rule build, merge/sort, diff
// against the deployed document, apply, sort, and the no-op equality check.
// This is everything a steady-state sync does besides the (skipped) API
// write.
func BenchmarkRebuildAndDiff_500Routes(b *testing.B) {
	routes := benchmarkRoutes(500)
	builder := ingress.NewBuilder("cluster.local", nil, nil, nil, slog.New(slog.DiscardHandler))

	// Deployed document = what the same build produces, so the benchmark
	// exercises the steady-state (no-op) path end to end.
	baseline := builder.Build(context.Background(), routes)
	deployed := make([]zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress, 0, len(baseline.Rules))

	for idx := range baseline.Rules {
		rule := ingress.RuleFromUpdate(&baseline.Rules[idx])
		deployed = append(deployed, zero_trust.TunnelCloudflaredConfigurationGetResponseConfigIngress{
			Hostname: rule.Hostname,
			Path:     rule.Path,
			Service:  rule.Service,
		})
	}

	b.ResetTimer()

	for b.Loop() {
		buildResult := builder.Build(context.Background(), routes)
		desired := mergeAndSortRules(buildResult.Rules, nil)

		toAdd, toRemove := ingress.DiffRules(deployed, desired)
		finalRules := ingress.ApplyDiff(deployed, toAdd, toRemove)
		finalRules = sortIngressRules(finalRules)
		finalRules = ingress.EnsureCatchAll(finalRules)

		if !ingress.RulesUnchanged(deployed, finalRules) {
			b.Fatal("steady-state benchmark must hit the unchanged path")
		}
	}
}
