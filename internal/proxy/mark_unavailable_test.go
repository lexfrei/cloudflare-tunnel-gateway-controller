package proxy_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestMarkUnavailableBackends_MarksMatchingBackend verifies that marking an
// invalid backend (matched by service host:port) sets UnavailableStatus on it
// while leaving the valid sibling and all weights untouched, so the proxy
// returns 500 for the invalid backend's traffic fraction instead of dialing
// it (502).
func TestMarkUnavailableBackends_MarksMatchingBackend(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://valid.default.svc.cluster.local:80", Weight: 80},
					{URL: "http://dead.default.svc.cluster.local:8080", Weight: 20},
				},
			},
		},
	}

	proxy.MarkUnavailableBackends(cfg, "cluster.local", "default", "dead", 8080, http.StatusInternalServerError)

	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus, "valid backend must be untouched")
	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[1].UnavailableStatus,
		"the dead backend must be marked")
	assert.Equal(t, int32(80), cfg.Rules[0].Backends[0].Weight, "weights preserved")
	assert.Equal(t, int32(20), cfg.Rules[0].Backends[1].Weight, "weights preserved")
}

// TestMarkUnavailableBackends_IgnoresNonMatching proves a backend on a different
// host or port is not marked.
func TestMarkUnavailableBackends_IgnoresNonMatching(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					// same service name, different port → distinct backend, not marked
					{URL: "http://dead.default.svc.cluster.local:80", Weight: 1},
					// different namespace → not marked
					{URL: "http://dead.other.svc.cluster.local:8080", Weight: 1},
				},
			},
		},
	}

	proxy.MarkUnavailableBackends(cfg, "cluster.local", "default", "dead", 8080, http.StatusInternalServerError)

	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus, "different port must not match")
	assert.Zero(t, cfg.Rules[0].Backends[1].UnavailableStatus, "different namespace must not match")
}

// TestMarkUnavailableBackends_MatchesHTTPSPort confirms host:port matching is
// scheme-agnostic: a port-443 backend (emitted as https://) is still matched.
func TestMarkUnavailableBackends_MatchesHTTPSPort(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "https://dead.default.svc.cluster.local:443", Weight: 1},
				},
			},
		},
	}

	proxy.MarkUnavailableBackends(cfg, "cluster.local", "default", "dead", 443, http.StatusInternalServerError)

	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[0].UnavailableStatus)
}

// TestMarkUnavailableBackends_FirstMarkingWins pins the precedence contract: an
// already-marked backend is not overwritten, so a 500 (invalid ref) applied
// before a 503 (zero ready endpoints) survives.
func TestMarkUnavailableBackends_FirstMarkingWins(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://svc.default.svc.cluster.local:80", Weight: 1},
				},
			},
		},
	}

	proxy.MarkUnavailableBackends(cfg, "cluster.local", "default", "svc", 80, http.StatusInternalServerError)
	proxy.MarkUnavailableBackends(cfg, "cluster.local", "default", "svc", 80, http.StatusServiceUnavailable)

	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[0].UnavailableStatus,
		"the first marking (500) must win over a later 503")
}
