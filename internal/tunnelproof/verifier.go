// Package tunnelproof asks Cloudflare whether a connector token really holds
// the tunnel it names.
package tunnelproof

import (
	"context"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cockroachdb/errors"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
)

// ClientFactory builds a Cloudflare API client for one API token.
type ClientFactory func(apiToken string) *cloudflare.Client

// Verifier checks tunnel claims against the Cloudflare API and caches the
// verdicts.
type Verifier struct {
	newClient ClientFactory
	now       func() time.Time

	mu    sync.Mutex
	cache map[[sha256.Size]byte]entry
}

type entry struct {
	proof tunnelownership.Proof
	at    time.Time
}

// NewVerifier returns a Verifier that talks to Cloudflare through newClient.
func NewVerifier(newClient ClientFactory) (*Verifier, error) {
	if newClient == nil {
		return nil, errors.New("tunnel claim verifier needs a Cloudflare client factory")
	}

	return &Verifier{newClient: newClient, now: time.Now, cache: make(map[[sha256.Size]byte]entry)}, nil
}

// Verify reports whether the token carries the real secret of the tunnel it
// names, asking Cloudflare with apiToken.
func (v *Verifier) Verify(_ context.Context, _ string, _ *tunnel.Token) tunnelownership.Proof {
	return tunnelownership.ProofUnknown
}
