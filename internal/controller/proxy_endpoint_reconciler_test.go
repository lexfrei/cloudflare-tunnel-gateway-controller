package controller

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/render"
)

// TestEndpointSliceMatchesProxy pins the watch predicate that drives
// per-pod config replay (ResyncPartition). The per-Gateway path depends on
// Kubernetes mirroring the Service's cf.k8s.lex.la/gateway label onto its
// EndpointSlices (kubernetes/kubernetes#94443, stable since 1.20) — without the
// mirror, a newly-joined or restarted per-Gateway proxy pod would never trigger
// a resync and would sit configless at /readyz. (The live mirror is exercised
// end to end by the e2e scale test; this pins the predicate logic.)
func TestEndpointSliceMatchesProxy(t *testing.T) {
	t.Parallel()

	reconciler := &ProxyEndpointReconciler{
		targets: []proxyServiceTarget{{namespace: "cf-system", name: "cftunnel-proxy"}},
	}
	pred := reconciler.endpointSliceMatchesProxy()

	perGateway := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant-a",
			Labels:    map[string]string{render.GatewayLabel: "edge"},
		},
	}
	shared := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "cf-system",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "cftunnel-proxy"},
		},
	}
	unrelated := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "other",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "something-else"},
		},
	}

	assert.True(t, pred.Create(event.CreateEvent{Object: perGateway}),
		"a per-Gateway EndpointSlice (mirrored Gateway label) must match")
	assert.True(t, pred.Create(event.CreateEvent{Object: shared}),
		"a shared-proxy EndpointSlice (matching service-name) must match")
	assert.False(t, pred.Create(event.CreateEvent{Object: unrelated}),
		"an unrelated EndpointSlice must not match")
	assert.False(t, pred.Create(event.CreateEvent{Object: &corev1.Pod{}}),
		"a non-EndpointSlice object must not match")
}

// TestParseProxyServiceTargets pins the URL-to-(svc,ns) shape recognition
// the EndpointSlice watch predicate depends on. Each row is a small fact
// about what we DO and DON'T pick up:
//
//   - cluster-DNS shapes (1, 2, and 3+ dotted segments after the host) all
//     resolve to the same (svc, ns) pair because we only need the first
//     two segments.
//   - bare hosts (no namespace) are skipped: we cannot watch
//     EndpointSlices across the cluster, and a single-segment Service
//     name is ambiguous.
//   - bare IPs are skipped for the same reason -- there is no Service
//     identity to match against.
//   - empty / unparseable URLs are silently dropped.
//   - duplicates collapse so the watch predicate doesn't double-fire.
func TestParseProxyServiceTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
		want []proxyServiceTarget
	}{
		{
			name: "fully qualified cluster-DNS",
			in:   []string{"http://cftunnel-proxy.cloudflare-tunnel-system.svc.cluster.local:8081/config"},
			want: []proxyServiceTarget{{name: "cftunnel-proxy", namespace: "cloudflare-tunnel-system"}},
		},
		{
			name: "svc and ns only",
			in:   []string{"http://cftunnel-proxy.cloudflare-tunnel-system:8081/config"},
			want: []proxyServiceTarget{{name: "cftunnel-proxy", namespace: "cloudflare-tunnel-system"}},
		},
		{
			name: "svc.ns.svc shorthand",
			in:   []string{"http://cftunnel-proxy.cloudflare-tunnel-system.svc:8081/config"},
			want: []proxyServiceTarget{{name: "cftunnel-proxy", namespace: "cloudflare-tunnel-system"}},
		},
		{
			name: "bare service name (no namespace) is skipped",
			in:   []string{"http://cftunnel-proxy:8081/config"},
			want: []proxyServiceTarget{},
		},
		{
			name: "bare IPv4 is skipped",
			in:   []string{"http://10.0.0.5:8081/config"},
			want: []proxyServiceTarget{},
		},
		{
			name: "empty / blank entries are silently dropped",
			in:   []string{"", "   "},
			want: []proxyServiceTarget{},
		},
		{
			name: "unparseable URL is silently dropped",
			in:   []string{":::not a url:::"},
			want: []proxyServiceTarget{},
		},
		{
			name: "duplicates collapse",
			in: []string{
				"http://cftunnel-proxy.cloudflare-tunnel-system.svc.cluster.local:8081/config",
				"http://cftunnel-proxy.cloudflare-tunnel-system:8081/config",
			},
			want: []proxyServiceTarget{{name: "cftunnel-proxy", namespace: "cloudflare-tunnel-system"}},
		},
		{
			name: "multiple distinct headless services kept",
			in: []string{
				"http://proxy-a.ns-a.svc.cluster.local:8081/config",
				"http://proxy-b.ns-b.svc.cluster.local:8081/config",
			},
			want: []proxyServiceTarget{
				{name: "proxy-a", namespace: "ns-a"},
				{name: "proxy-b", namespace: "ns-b"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := parseProxyServiceTargets(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestIsProbablyIP pins the cheap dotted-quad detector. False negatives
// (an IP that gets classified as a Service) just fall through to the
// multi-segment parser and produce a meaningless target -- ugly but
// recoverable. False positives (a Service host that looks like an IP)
// would silently drop the watch. We accept ASCII-digit-only segments
// in groups of four; an IPv6 host (which always contains "[" or "::")
// never satisfies the predicate. Service DNS names never look like
// dotted quads.
func TestIsProbablyIP(t *testing.T) {
	t.Parallel()

	cases := []struct {
		host string
		want bool
	}{
		{"10.0.0.5", true},
		{"255.255.255.255", true},
		{"127.0.0.1", true},
		{"1.2.3", false},
		{"1.2.3.4.5", false},
		{"a.b.c.d", false},
		{"10.0.0.x", false},
		{"", false},
		{"10..0.5", false},
		{"my-svc.ns.svc.cluster.local", false},
	}

	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, isProbablyIP(tc.host))
		})
	}
}

// TestPartitionKeyForLabel pins the attribution that sets the resync scope: a
// GatewayLabel value matching a live Gateway resolves to that Gateway's
// partition key (a targeted ResyncPartition); a value matching no live Gateway
// (the truncated form of a deleted name, or foreign) is unattributable, which
// the reconciler answers with a full ResyncAllPartitions rather than resyncing
// a partition that does not exist.
func TestPartitionKeyForLabel(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))

	gateway := &gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "tenant-a"}}
	testClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gateway).Build()
	reconciler := &ProxyEndpointReconciler{Client: testClient}

	key, ok := reconciler.partitionKeyForLabel(context.Background(), "tenant-a", render.GatewayLabelValue("edge"))
	assert.True(t, ok, "a label value matching a live Gateway must be attributable")
	assert.Equal(t, "tenant-a/edge", key)

	_, ok = reconciler.partitionKeyForLabel(context.Background(), "tenant-a", "ghost-of-a-deleted-gateway")
	assert.False(t, ok, "a label value matching no live Gateway must be unattributable")
}

// TestReconcile_UnattributableGatewayLabelResyncsAll pins that an EndpointSlice
// carrying a GatewayLabel value attributable to no live Gateway takes the
// full-resync branch and completes without error — rather than panicking or
// resyncing a partition that does not exist.
func TestReconcile_UnattributableGatewayLabelResyncsAll(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, discoveryv1.AddToScheme(scheme))

	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "es-ghost",
			Namespace: "tenant-a",
			Labels:    map[string]string{render.GatewayLabel: "ghost"},
		},
	}
	testClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(slice).Build()
	proxySyncer := NewProxySyncer("cluster.local", "", "", testClient, slog.Default())
	reconciler := &ProxyEndpointReconciler{Client: testClient, ProxySyncer: proxySyncer}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "es-ghost", Namespace: "tenant-a"},
	})
	require.NoError(t, err, "an unattributable Gateway label must resync all partitions without error")
}
