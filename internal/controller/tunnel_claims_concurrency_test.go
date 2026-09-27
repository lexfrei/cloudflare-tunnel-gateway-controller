package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
// never gets past the first.
type barrierClaimResolver struct {
	want    int
	mu      sync.Mutex
	arrived int
	all     chan struct{}
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

	return &config.TunnelClaim{
		TunnelID: tunnelFor(gateway.Namespace),
		Proof:    tunnelownership.ProofVerified,
	}, nil
}

var errLookupAbandoned = errors.New("claim lookup abandoned before every lookup was in flight")

func tunnelFor(namespace string) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012s", namespace[len(namespace)-1:])
}

// TestCollectTunnelClaims_LooksClaimsUpConcurrently pins that one pass does not
// pay each claim's lookup time in sequence: a Cloudflare API that hangs would
// otherwise cost one request timeout per opted-in Gateway. The claims still
// come back in listing order.
func TestCollectTunnelClaims_LooksClaimsUpConcurrently(t *testing.T) {
	t.Parallel()

	namespaces := []string{"team-1", "team-2", "team-3", "team-4"}

	objects := make([]client.Object, 0, len(namespaces)+1)
	objects = append(objects, claimsGatewayClass())

	for i, namespace := range namespaces {
		objects = append(objects, claimsGateway(namespace, "gw", i, namespace+"-token"))
	}

	fakeClient := setupGatewayFakeClient(objects...)
	resolver := &barrierClaimResolver{want: len(namespaces), all: make(chan struct{})}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	gateways, err := managedInfraGateways(ctx, fakeClient, "test-controller")
	require.NoError(t, err)

	claims := collectTunnelClaims(ctx, gateways, resolver, claimsClassTunnel)
	require.NoError(t, ctx.Err(), "every lookup must be in flight at once; a sequential pass stalls on the first")

	require.Len(t, claims, len(namespaces))

	for i, claim := range claims {
		assert.Equal(t, namespaces[i], claim.Namespace)
		assert.Equal(t, tunnelFor(claim.Namespace), claim.TunnelID)
		assert.Equal(t, tunnelownership.ProofVerified, claim.Proof)
	}
}
