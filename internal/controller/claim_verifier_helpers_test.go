package controller

import (
	"context"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
)

// fixedClaimVerifier answers every tunnel claim with one verdict, standing in
// for the Cloudflare API in tests.
type fixedClaimVerifier tunnelownership.Proof

func (v fixedClaimVerifier) Verify(context.Context, string, *tunnel.Token) tunnelownership.Proof {
	return tunnelownership.Proof(v)
}

// verifiedClaims makes a test Resolver confirm every tunnel claim, so tests
// about something else never reach the Cloudflare API.
func verifiedClaims() config.ResolverOption {
	return config.WithClaimVerifier(fixedClaimVerifier(tunnelownership.ProofVerified))
}
