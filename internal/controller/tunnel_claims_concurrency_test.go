package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
)

// barrierClaimResolver answers a claim lookup only once `want` lookups are in
// flight at the same time, standing in for a Cloudflare API that hangs until
// its request timeout. A collector that looks claims up one after another
// never gets past the first. A Gateway in the unclaimed namespace claims no
// tunnel.
type barrierClaimResolver struct {
	want      int
	unclaimed string
	mu        sync.Mutex
	arrived   int
	all       chan struct{}
}

func (r *barrierClaimResolver) ResolveTunnelClaimForGateway(
	ctx context.Context,
	gateway *gatewayv1.Gateway,
) (*config.TunnelClaim, error) {
	r.mu.Lock()
	r.arrived++

	if r.arrived == r.want {
		close(r.all)
	}
	r.mu.Unlock()

	select {
	case <-r.all:
	case <-ctx.Done():
		return nil, errLookupAbandoned
	}

	if gateway.Namespace == r.unclaimed {
		return &config.TunnelClaim{}, nil
	}

	return &config.TunnelClaim{
		TunnelID: tunnelFor(gateway.Namespace),
		Proof:    tunnelownership.ProofVerified,
	}, nil
}

var errLookupAbandoned = errors.New("claim lookup abandoned before every lookup was in flight")

func tunnelFor(namespace string) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012s", namespace[len(namespace)-1:])
}

// peakClaimResolver holds each lookup for a moment and records the most
// lookups ever in flight at once.
type peakClaimResolver struct {
	inFlight atomic.Int32
	peak     atomic.Int32
}

func (r *peakClaimResolver) ResolveTunnelClaimForGateway(
	_ context.Context,
	gateway *gatewayv1.Gateway,
) (*config.TunnelClaim, error) {
	current := r.inFlight.Add(1)
	defer r.inFlight.Add(-1)

	for {
		seen := r.peak.Load()
		if current <= seen || r.peak.CompareAndSwap(seen, current) {
			break
		}
	}

	time.Sleep(20 * time.Millisecond)

	return &config.TunnelClaim{TunnelID: tunnelFor(gateway.Namespace), Proof: tunnelownership.ProofVerified}, nil
}

// TestCollectTunnelClaims_BoundsConcurrentLookups pins the bound on lookups in
// flight: a cluster with many dedicated planes must not open one Cloudflare
// request per Gateway at once.
func TestCollectTunnelClaims_BoundsConcurrentLookups(t *testing.T) {
	t.Parallel()

	const gatewayCount = 3 * maxConcurrentClaimLookups

	objects := make([]client.Object, 0, gatewayCount+1)
	objects = append(objects, claimsGatewayClass())

	for i := range gatewayCount {
		objects = append(objects, claimsGateway(fmt.Sprintf("team-%d", i), "gw", 0, "token"))
	}

	fakeClient := setupGatewayFakeClient(objects...)
	resolver := &peakClaimResolver{}
	ctx := context.Background()

	gateways, err := managedInfraGateways(ctx, fakeClient, "test-controller")
	require.NoError(t, err)
	require.Len(t, gateways, gatewayCount)

	claims := collectTunnelClaims(ctx, gateways, resolver, claimsClassTunnel)
	require.Len(t, claims, gatewayCount)

	assert.LessOrEqual(t, resolver.peak.Load(), int32(maxConcurrentClaimLookups),
		"no more than the bound may be in flight at once")
	assert.Greater(t, resolver.peak.Load(), int32(1), "lookups must still overlap")
}

// TestCollectTunnelClaims_LooksClaimsUpConcurrently pins that one pass does not
// pay each claim's lookup time in sequence: a Cloudflare API that hangs would
// otherwise cost one request timeout per opted-in Gateway. The claims still
// come back in listing order, without a gap where a Gateway claims nothing.
func TestCollectTunnelClaims_LooksClaimsUpConcurrently(t *testing.T) {
	t.Parallel()

	namespaces := []string{"team-1", "team-2", "team-3", "team-4"}

	objects := make([]client.Object, 0, len(namespaces)+1)
	objects = append(objects, claimsGatewayClass())

	for i, namespace := range namespaces {
		objects = append(objects, claimsGateway(namespace, "gw", i, namespace+"-token"))
	}

	fakeClient := setupGatewayFakeClient(objects...)
	resolver := &barrierClaimResolver{want: len(namespaces), unclaimed: "team-2", all: make(chan struct{})}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	gateways, err := managedInfraGateways(ctx, fakeClient, "test-controller")
	require.NoError(t, err)

	claims := collectTunnelClaims(ctx, gateways, resolver, claimsClassTunnel)
	require.NoError(t, ctx.Err(), "every lookup must be in flight at once; a sequential pass stalls on the first")

	claimants := []string{"team-1", "team-3", "team-4"}
	require.Len(t, claims, len(claimants))

	for i, claim := range claims {
		assert.Equal(t, claimants[i], claim.Namespace)
		assert.Equal(t, tunnelFor(claim.Namespace), claim.TunnelID)
		assert.Equal(t, tunnelownership.ProofVerified, claim.Proof)
	}
}
