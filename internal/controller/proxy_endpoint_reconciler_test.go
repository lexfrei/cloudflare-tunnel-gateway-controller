package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
