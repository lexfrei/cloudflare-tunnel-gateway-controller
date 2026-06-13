package controller

import (
	"context"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/proxy"
)

var errTunnelGroupFailed = errors.New("tunnel group sync failed")

// TestBuildStatusEntries_PartialFailureIsolatesRoutes pins the per-partition
// status isolation (#479): a route whose tunnel group failed carries the
// group's sync error (→ Pending), while a route on a DIFFERENT, healthy tunnel
// carries no override — one tenant's broken tunnel must never flip another
// tenant's route status.
func TestBuildStatusEntries_PartialFailureIsolatesRoutes(t *testing.T) {
	t.Parallel()

	routes := []gatewayv1.HTTPRoute{
		{ObjectMeta: metav1.ObjectMeta{Name: "broken-route", Namespace: "team-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "healthy-route", Namespace: "team-b"}},
	}

	// Only team-a's tunnel group failed.
	routeSyncErrors := map[string]error{"team-a/broken-route": errTunnelGroupFailed}

	noopUpdate := func(_ context.Context, _ *gatewayv1.HTTPRoute, _ routeBindingInfo,
		_ []ingress.BackendRefError, _ []proxy.RouteDiagnostic, _ error,
	) error {
		return nil
	}

	entries := buildStatusEntries(routes, nil, nil, nil, routeSyncErrors, noopUpdate)
	require.Len(t, entries, 2)

	byName := make(map[string]routeStatusEntry, len(entries))
	for _, entry := range entries {
		byName[entry.name] = entry
	}

	require.ErrorIs(t, byName["broken-route"].syncErrOverride, errTunnelGroupFailed,
		"a route on the failed tunnel must carry the group's sync error")
	assert.NoError(t, byName["healthy-route"].syncErrOverride,
		"a route on a healthy tunnel must NOT inherit another tenant's failure")
}

// TestUpdateRoutesStatus_OverrideWinsOverGlobalSyncErr pins that a per-route
// override replaces the global sync error: with NO global error, only the
// overridden route's update sees an error.
func TestUpdateRoutesStatus_OverrideWinsOverGlobalSyncErr(t *testing.T) {
	t.Parallel()

	seen := map[string]error{}

	entries := []routeStatusEntry{
		{
			name: "broken-route", namespace: "team-a",
			syncErrOverride: errTunnelGroupFailed,
			update: func(_ context.Context, _ routeBindingInfo, _ []ingress.BackendRefError,
				_ []proxy.RouteDiagnostic, se error,
			) error {
				seen["team-a/broken-route"] = se

				return nil
			},
		},
		{
			name: "healthy-route", namespace: "team-b",
			update: func(_ context.Context, _ routeBindingInfo, _ []ingress.BackendRefError,
				_ []proxy.RouteDiagnostic, se error,
			) error {
				seen["team-b/healthy-route"] = se

				return nil
			},
		},
	}

	require.NoError(t, updateRoutesStatus(context.Background(), discardLogger{}, entries, nil))

	assert.ErrorIs(t, seen["team-a/broken-route"], errTunnelGroupFailed)
	assert.NoError(t, seen["team-b/healthy-route"], "no global error and no override → no error for the healthy route")
}

type discardLogger struct{}

func (discardLogger) Error(string, ...any) {}
