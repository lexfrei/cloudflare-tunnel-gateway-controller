package config

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/cfmetrics"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelproof"
)

// TestNewResolver_VerifiesClaimsAgainstCloudflare pins that a Resolver can
// only be built with a real verifier: without one a tunnel claim would go
// unchecked, which is the fail-open this whole check exists to close.
func TestNewResolver_VerifiesClaimsAgainstCloudflare(t *testing.T) {
	t.Parallel()

	resolver := NewResolver(nil, "default", cfmetrics.NewNoopCollector())

	assert.IsType(t, &tunnelproof.Verifier{}, resolver.claimVerifier)
}

func TestWithClaimVerifier_NilKeepsTheCloudflareVerifier(t *testing.T) {
	t.Parallel()

	resolver := NewResolver(nil, "default", cfmetrics.NewNoopCollector(), WithClaimVerifier(nil))

	assert.IsType(t, &tunnelproof.Verifier{}, resolver.claimVerifier)
}
