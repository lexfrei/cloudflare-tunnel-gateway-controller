package proxy_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// backendURLs collects the resolved URLs of a rule's backends, preserving order,
// so tests can assert on the expanded endpoint set.
func backendURLs(backends []proxy.BackendRef) []string {
	urls := make([]string, 0, len(backends))
	for i := range backends {
		urls = append(urls, backends[i].URL)
	}

	return urls
}

// TestExpandHeadlessBackend_SingleBackendExpands proves a lone headless backend
// is replaced by one backend per resolved endpoint, dialing the endpoint port
// (3000) rather than the Service port (8080).
func TestExpandHeadlessBackend_SingleBackendExpands(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://headless.default.svc.cluster.local:8080", Weight: 1},
				},
			},
		},
	}

	proxy.ExpandHeadlessBackend(cfg, "cluster.local", "default", "headless", 8080, []proxy.ResolvedEndpoint{
		{Host: "10.0.0.1", Port: 3000},
		{Host: "10.0.0.2", Port: 3000},
	})

	assert.Equal(t, []string{
		"http://10.0.0.1:3000",
		"http://10.0.0.2:3000",
	}, backendURLs(cfg.Rules[0].Backends))

	for i := range cfg.Rules[0].Backends {
		assert.Equal(t, int32(1), cfg.Rules[0].Backends[i].Weight, "single backendRef → equal endpoint weights")
	}
}

// TestExpandHeadlessBackend_InheritsFields proves the per-endpoint backends carry
// the matched backend's Protocol, TLS, Filters and WebSocket flag — only the dial
// URL changes.
func TestExpandHeadlessBackend_InheritsFields(t *testing.T) {
	t.Parallel()

	tlsCfg := &proxy.BackendTLSConfig{ServerName: "headless.default.svc.cluster.local"}
	filters := []proxy.RouteFilter{{Type: proxy.FilterRequestHeaderModifier}}

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{
						URL:       "https://headless.default.svc.cluster.local:8443",
						Weight:    1,
						Protocol:  proxy.BackendProtocolH2C,
						TLS:       tlsCfg,
						Filters:   filters,
						WebSocket: true,
					},
				},
			},
		},
	}

	proxy.ExpandHeadlessBackend(cfg, "cluster.local", "default", "headless", 8443, []proxy.ResolvedEndpoint{
		{Host: "10.0.0.1", Port: 3000},
	})

	require.Len(t, cfg.Rules[0].Backends, 1)
	got := cfg.Rules[0].Backends[0]
	assert.Equal(t, "https://10.0.0.1:3000", got.URL, "scheme preserved, IP:targetPort dialed")
	assert.Equal(t, proxy.BackendProtocolH2C, got.Protocol)
	assert.Same(t, tlsCfg, got.TLS, "TLS config (with SNI) inherited")
	assert.Equal(t, filters, got.Filters)
	assert.True(t, got.WebSocket)
}

// TestExpandHeadlessBackend_IPv6Bracketed proves an IPv6 endpoint address is
// bracketed in the dial URL so the host:port parses correctly.
func TestExpandHeadlessBackend_IPv6Bracketed(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://headless.default.svc.cluster.local:8080", Weight: 1},
				},
			},
		},
	}

	proxy.ExpandHeadlessBackend(cfg, "cluster.local", "default", "headless", 8080, []proxy.ResolvedEndpoint{
		{Host: "2001:db8::1", Port: 3000},
	})

	assert.Equal(t, []string{"http://[2001:db8::1]:3000"}, backendURLs(cfg.Rules[0].Backends))
}

// TestExpandHeadlessBackend_PreservesProportionWithSibling proves that expanding a
// headless backendRef sharing a rule with a non-headless sibling keeps the
// headless ref's Gateway API traffic proportion: the sibling weight is scaled by
// the endpoint count so the aggregate ratio is unchanged.
func TestExpandHeadlessBackend_PreservesProportionWithSibling(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://headless.default.svc.cluster.local:8080", Weight: 1},
					{URL: "http://clusterip.default.svc.cluster.local:80", Weight: 1},
				},
			},
		},
	}

	proxy.ExpandHeadlessBackend(cfg, "cluster.local", "default", "headless", 8080, []proxy.ResolvedEndpoint{
		{Host: "10.0.0.1", Port: 3000},
		{Host: "10.0.0.2", Port: 3000},
		{Host: "10.0.0.3", Port: 3000},
	})

	backends := cfg.Rules[0].Backends
	require.Len(t, backends, 4, "3 endpoints + 1 sibling")

	// Three endpoints, each weight 1 → headless aggregate 3.
	var headlessTotal, sibling int32
	for i := range backends {
		if backends[i].URL == "http://clusterip.default.svc.cluster.local:80" {
			sibling = backends[i].Weight
			continue
		}

		assert.Equal(t, int32(1), backends[i].Weight, "endpoints split equally")
		headlessTotal += backends[i].Weight
	}

	assert.Equal(t, int32(3), sibling, "sibling scaled by endpoint count")
	assert.Equal(t, headlessTotal, sibling, "1:1 backendRef ratio preserved (3:3)")
}

// TestExpandHeadlessBackend_GCDReduction proves weights are reduced by their GCD
// after scaling so magnitudes stay small while ratios are preserved.
func TestExpandHeadlessBackend_GCDReduction(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://headless.default.svc.cluster.local:8080", Weight: 2},
					{URL: "http://clusterip.default.svc.cluster.local:80", Weight: 4},
				},
			},
		},
	}

	proxy.ExpandHeadlessBackend(cfg, "cluster.local", "default", "headless", 8080, []proxy.ResolvedEndpoint{
		{Host: "10.0.0.1", Port: 3000},
		{Host: "10.0.0.2", Port: 3000},
	})

	// Pre-GCD: endpoints 2,2 ; sibling 4*2=8 → {2,2,8}, GCD 2 → {1,1,4}.
	backends := cfg.Rules[0].Backends
	require.Len(t, backends, 3)

	var sibling int32
	for i := range backends {
		if backends[i].URL == "http://clusterip.default.svc.cluster.local:80" {
			sibling = backends[i].Weight
			continue
		}

		assert.Equal(t, int32(1), backends[i].Weight)
	}

	assert.Equal(t, int32(4), sibling)
}

// TestExpandHeadlessBackend_MultipleHeadlessCompose proves two sequential
// expansions of distinct headless services in one rule compose to the correct
// aggregate ratio (the controller calls the helper once per Service).
func TestExpandHeadlessBackend_MultipleHeadlessCompose(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://a.default.svc.cluster.local:8080", Weight: 1},
					{URL: "http://b.default.svc.cluster.local:8080", Weight: 1},
					{URL: "http://c.default.svc.cluster.local:80", Weight: 1},
				},
			},
		},
	}

	proxy.ExpandHeadlessBackend(cfg, "cluster.local", "default", "a", 8080, []proxy.ResolvedEndpoint{
		{Host: "10.0.0.1", Port: 3000},
		{Host: "10.0.0.2", Port: 3000},
	})
	proxy.ExpandHeadlessBackend(cfg, "cluster.local", "default", "b", 8080, []proxy.ResolvedEndpoint{
		{Host: "10.0.1.1", Port: 3000},
		{Host: "10.0.1.2", Port: 3000},
		{Host: "10.0.1.3", Port: 3000},
	})

	var aTotal, bTotal, cTotal int32
	aCount, bCount := 0, 0
	for _, b := range cfg.Rules[0].Backends {
		switch {
		case b.URL == "http://c.default.svc.cluster.local:80":
			cTotal += b.Weight
		case hasPrefix(b.URL, "http://10.0.0."):
			aTotal += b.Weight
			aCount++
		case hasPrefix(b.URL, "http://10.0.1."):
			bTotal += b.Weight
			bCount++
		}
	}

	assert.Equal(t, 2, aCount, "service a → 2 endpoints")
	assert.Equal(t, 3, bCount, "service b → 3 endpoints")
	assert.Equal(t, aTotal, bTotal, "a:b aggregate ratio preserved (1:1)")
	assert.Equal(t, aTotal, cTotal, "a:c aggregate ratio preserved (1:1)")
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// TestExpandHeadlessBackend_ZeroEndpoints_NoOp proves a headless Service with no
// resolved endpoints leaves the FQDN backend untouched so the downstream
// zero-endpoint pass can mark it 503.
func TestExpandHeadlessBackend_ZeroEndpoints_NoOp(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://headless.default.svc.cluster.local:8080", Weight: 1},
				},
			},
		},
	}

	proxy.ExpandHeadlessBackend(cfg, "cluster.local", "default", "headless", 8080, nil)

	assert.Equal(t, []string{"http://headless.default.svc.cluster.local:8080"}, backendURLs(cfg.Rules[0].Backends))
}

// TestExpandHeadlessBackend_NonMatchingHost_Untouched proves a backend on a
// different service identity is not expanded.
func TestExpandHeadlessBackend_NonMatchingHost_Untouched(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://other.default.svc.cluster.local:8080", Weight: 1},
				},
			},
		},
	}

	proxy.ExpandHeadlessBackend(cfg, "cluster.local", "default", "headless", 8080, []proxy.ResolvedEndpoint{
		{Host: "10.0.0.1", Port: 3000},
	})

	assert.Equal(t, []string{"http://other.default.svc.cluster.local:8080"}, backendURLs(cfg.Rules[0].Backends))
	assert.Equal(t, int32(1), cfg.Rules[0].Backends[0].Weight, "non-matching weight untouched")
}

// TestExpandHeadlessBackend_SkipsMarkedBackend proves an already-marked backend
// (500/503) matching the host is not expanded — the marking carries its traffic
// fraction and must survive.
func TestExpandHeadlessBackend_SkipsMarkedBackend(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{
						URL:               "http://headless.default.svc.cluster.local:8080",
						Weight:            1,
						UnavailableStatus: http.StatusInternalServerError,
					},
				},
			},
		},
	}

	proxy.ExpandHeadlessBackend(cfg, "cluster.local", "default", "headless", 8080, []proxy.ResolvedEndpoint{
		{Host: "10.0.0.1", Port: 3000},
	})

	require.Len(t, cfg.Rules[0].Backends, 1)
	assert.Equal(t, "http://headless.default.svc.cluster.local:8080", cfg.Rules[0].Backends[0].URL)
	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[0].UnavailableStatus)
}

// TestExpandHeadlessBackend_WeightZeroNotExpanded proves a weight-0 headless
// backendRef is left as the FQDN backend (it carries no traffic, and expanding it
// would needlessly perturb sibling weights).
func TestExpandHeadlessBackend_WeightZeroNotExpanded(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://headless.default.svc.cluster.local:8080", Weight: 0},
					{URL: "http://clusterip.default.svc.cluster.local:80", Weight: 1},
				},
			},
		},
	}

	proxy.ExpandHeadlessBackend(cfg, "cluster.local", "default", "headless", 8080, []proxy.ResolvedEndpoint{
		{Host: "10.0.0.1", Port: 3000},
		{Host: "10.0.0.2", Port: 3000},
	})

	assert.Equal(t, []string{
		"http://headless.default.svc.cluster.local:8080",
		"http://clusterip.default.svc.cluster.local:80",
	}, backendURLs(cfg.Rules[0].Backends), "weight-0 headless untouched; sibling not scaled")
	assert.Equal(t, int32(1), cfg.Rules[0].Backends[1].Weight)
}
