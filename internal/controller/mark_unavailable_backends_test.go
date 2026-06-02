package controller

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

// TestMarkUnavailableBackends_MarksInvalidKeepsValid pins the per-fraction 500
// behaviour: in a rule with a valid high-weight backend and an invalid
// (nonexistent Service) low-weight backend, the invalid one is marked 500 and
// kept in the weighted pool while the valid one is untouched — so the invalid
// backend's traffic fraction returns 500 (not 502) and the valid fraction
// keeps serving.
func TestMarkUnavailableBackends_MarksInvalidKeepsValid(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://valid-svc.default.svc.cluster.local:80", Weight: 80},
					{URL: "http://missing-svc.default.svc.cluster.local:8080", Weight: 20},
				},
			},
		},
	}

	failedRefs := []ingress.BackendRefError{
		{RouteNamespace: "default", RouteName: "r", BackendName: "missing-svc", BackendNS: "default", Port: 8080},
	}

	markUnavailableBackends(cfg, "cluster.local", failedRefs)

	backends := cfg.Rules[0].Backends
	require.Len(t, backends, 2, "both backends must remain in the pool")
	assert.Zero(t, backends[0].UnavailableStatus, "valid backend untouched")
	assert.Equal(t, http.StatusInternalServerError, backends[1].UnavailableStatus, "invalid backend marked 500")
	assert.Equal(t, int32(80), backends[0].Weight)
	assert.Equal(t, int32(20), backends[1].Weight, "weight preserved for the invalid backend's fraction")
}

// TestMarkUnavailableBackends_AllInvalidAllMarked proves that when every backend
// in a rule is invalid, all are marked 500 — so 100% of the rule's traffic
// returns 500, matching the all-invalid spec contract.
func TestMarkUnavailableBackends_AllInvalidAllMarked(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://a.default.svc.cluster.local:80", Weight: 1},
					{URL: "http://b.default.svc.cluster.local:80", Weight: 1},
				},
			},
		},
	}

	failedRefs := []ingress.BackendRefError{
		{RouteNamespace: "default", RouteName: "r", BackendName: "a", BackendNS: "default", Port: 80},
		{RouteNamespace: "default", RouteName: "r", BackendName: "b", BackendNS: "default", Port: 80},
	}

	markUnavailableBackends(cfg, "cluster.local", failedRefs)

	for i := range cfg.Rules[0].Backends {
		assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[i].UnavailableStatus)
	}
}

// TestMarkUnavailableBackends_ServiceImportDomain proves a ServiceImport
// failed-ref is matched against the clusterset host (carried on the ref's
// Domain), not the local cluster domain — without it the host would not match
// the converter-synthesized clusterset.local URL and the 500 would be lost.
func TestMarkUnavailableBackends_ServiceImportDomain(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{
				Backends: []proxy.BackendRef{
					{URL: "http://imported.default.svc.clusterset.local:80", Weight: 1},
				},
			},
		},
	}

	failedRefs := []ingress.BackendRefError{
		{
			RouteNamespace: "default", RouteName: "r",
			BackendName: "imported", BackendNS: "default", Port: 80,
			Reason: "BackendNotFound", Domain: "clusterset.local",
		},
	}

	markUnavailableBackends(cfg, "cluster.local", failedRefs)

	assert.Equal(t, http.StatusInternalServerError, cfg.Rules[0].Backends[0].UnavailableStatus,
		"a ServiceImport backend must be marked 500 via its clusterset host")
}

// TestMarkUnavailableBackends_NoFailedRefsNoop proves a clean config is left
// untouched when there are no failed refs.
func TestMarkUnavailableBackends_NoFailedRefsNoop(t *testing.T) {
	t.Parallel()

	cfg := &proxy.Config{
		Rules: []proxy.RouteRule{
			{Backends: []proxy.BackendRef{{URL: "http://svc.default.svc.cluster.local:80", Weight: 1}}},
		},
	}

	markUnavailableBackends(cfg, "cluster.local", nil)

	assert.Zero(t, cfg.Rules[0].Backends[0].UnavailableStatus)
}
