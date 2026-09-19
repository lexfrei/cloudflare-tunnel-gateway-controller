package controller

// Covers what happens when a tunnel's ingress document crosses Cloudflare's
// per-tunnel rule cap: the write is refused for the WHOLE document, which is
// every tenant's configuration on that tunnel, not just the one that grew.
// The two audiences of that failure get different text — the operator needs to
// know which namespace to talk to, and the tenant must not learn who their
// neighbours are.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/api/v1alpha1"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
)

const budgetClassTunnelID = "99999999-9999-4999-8999-999999999999"

// budgetRoute builds a route whose single rule fans out over hostnames x path
// matches. That product is what fills the ingress document — one entry per
// (rule, hostname, match) — so a two-object fixture can cross the cap the way
// a real tenant does, without a thousand routes.
func budgetRoute(namespace, name string, hostnames, matches int) *gatewayv1.HTTPRoute {
	pathPrefix := gatewayv1.PathMatchPathPrefix
	port := gatewayv1.PortNumber(80)

	hosts := make([]gatewayv1.Hostname, 0, hostnames)
	for i := range hostnames {
		hosts = append(hosts, gatewayv1.Hostname(fmt.Sprintf("h%d.%s.example.com", i, namespace)))
	}

	pathMatches := make([]gatewayv1.HTTPRouteMatch, 0, matches)
	for i := range matches {
		pathMatches = append(pathMatches, gatewayv1.HTTPRouteMatch{
			Path: &gatewayv1.HTTPPathMatch{Type: &pathPrefix, Value: new(fmt.Sprintf("/p%d", i))},
		})
	}

	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Name:      "shared-gw",
					Namespace: new(gatewayv1.Namespace("default")),
				}},
			},
			Hostnames: hosts,
			Rules: []gatewayv1.HTTPRouteRule{
				{
					Matches: pathMatches,
					BackendRefs: []gatewayv1.HTTPBackendRef{
						{BackendRef: gatewayv1.BackendRef{
							BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc", Port: &port},
							Weight:                 new(int32(1)),
						}},
					},
				},
			},
		},
	}
}

// budgetObjects is one shared tunnel serving two namespaces: tenant-a is far
// over the cap on its own, tenant-b contributes a single rule and is the
// bystander whose configuration the refusal also freezes.
func budgetObjects(t *testing.T) []runtime.Object {
	t.Helper()

	return []runtime.Object{
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "cf-test"},
			Spec: gatewayv1.GatewayClassSpec{
				ControllerName: skipTestControllerName,
				ParametersRef: &gatewayv1.ParametersReference{
					Group: config.ParametersRefGroup, Kind: config.ParametersRefKind, Name: "cfg",
				},
			},
		},
		&v1alpha1.GatewayClassConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "cfg"},
			Spec: v1alpha1.GatewayClassConfigSpec{
				CloudflareCredentialsSecretRef: v1alpha1.SecretReference{Name: "creds", Namespace: "default"},
				AccountID:                      "test-account",
				TunnelID:                       budgetClassTunnelID,
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
			Data:       map[string][]byte{"api-token": []byte("test-token")},
		},
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "shared-gw", Namespace: "default"},
			Spec:       gatewayv1.GatewaySpec{GatewayClassName: "cf-test", Listeners: httpListener()},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "tenant-a"},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "tenant-b"},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		},
		budgetRoute("tenant-a", "big", 40, 26),
		budgetRoute("tenant-b", "small", 1, 1),
	}
}

// TestSyncAllRoutes_RuleBudgetExhausted_OperatorLogNamesTheNamespaces pins the
// half of the failure the operator needs: which namespace to go and talk to.
// Without per-namespace attribution the log says only that some number above
// the cap was reached, which is true of every tenant on the tunnel and points
// at none of them.
func TestSyncAllRoutes_RuleBudgetExhausted_OperatorLogNamesTheNamespaces(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := partitionSyncSyncerFor(t, api, budgetObjects(t))

	var logged bytes.Buffer

	syncer.Logger = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.Error(t, err, "a document over the cap is refused, not truncated")

	assert.Equal(t, 0, api.tunnelsWritten(), "nothing is written while the document is over the cap")

	assert.Contains(t, logged.String(), "tenant-a=1040",
		"the operator log attributes the rules to the namespace that owns them")
	assert.Contains(t, logged.String(), "tenant-b=1",
		"every contributing namespace is attributed, not just the largest")
}

// TestSyncAllRoutes_RuleBudgetExhausted_TenantMessageNamesNoNeighbour pins the
// other half. This error becomes the Accepted=False message on every route on
// the tunnel, and a route's status is readable by whoever owns that route — so
// a per-namespace breakdown would hand each tenant the namespace names and
// relative traffic shape of everyone else sharing the tunnel.
func TestSyncAllRoutes_RuleBudgetExhausted_TenantMessageNamesNoNeighbour(t *testing.T) {
	t.Parallel()

	api := newRecordingTunnelAPI(t)
	syncer := partitionSyncSyncerFor(t, api, budgetObjects(t))

	_, _, err := syncer.SyncAllRoutes(context.Background())
	require.Error(t, err)

	message := err.Error()

	assert.NotContains(t, message, "tenant-a", "the tenant-facing message names no other namespace")
	assert.NotContains(t, message, "tenant-b", "nor the reader's own, which would still leak the pattern")
	assert.NotContains(t, message, "1040", "nor a count another tenant's share can be inferred from")
	assert.NotContains(t, message, "1041", "nor the document total")

	assert.Contains(t, message, "dedicated data plane",
		"the tenant is told what they can actually do about it")
}

// TestIngressRuleAttribution covers the rendering the operator actually reads:
// merged across both builders, largest share first so the offender leads, ties
// broken on name so the line does not reshuffle between reconciles of an
// unchanged cluster, and capped so a cluster with hundreds of namespaces does
// not turn one log line into a page.
func TestIngressRuleAttribution(t *testing.T) {
	t.Parallel()

	crowded := make(map[string]int, ruleAttributionLimit+1)
	for i := 1; i <= ruleAttributionLimit+1; i++ {
		crowded[fmt.Sprintf("ns%02d", i)] = i
	}

	tests := []struct {
		name string
		http map[string]int
		grpc map[string]int
		want string
	}{
		{name: "nothing to attribute", want: ""},
		{
			name: "http and grpc shares of one namespace add up",
			http: map[string]int{"team-a": 2},
			grpc: map[string]int{"team-a": 3, "team-b": 1},
			want: "team-a=5,team-b=1",
		},
		{
			name: "largest share leads",
			http: map[string]int{"small": 1, "big": 9, "middle": 4},
			want: "big=9,middle=4,small=1",
		},
		{
			name: "equal shares are ordered by name",
			http: map[string]int{"b": 2, "a": 2, "c": 2},
			want: "a=2,b=2,c=2",
		},
		{
			name: "the tail is counted, not listed",
			http: crowded,
			want: "ns11=11,ns10=10,ns09=9,ns08=8,ns07=7,ns06=6,ns05=5,ns04=4,ns03=3,ns02=2,+1 more",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, ingressRuleAttribution(tt.http, tt.grpc))
		})
	}
}
