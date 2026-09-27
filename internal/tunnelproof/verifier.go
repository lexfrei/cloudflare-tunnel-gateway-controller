// Package tunnelproof asks Cloudflare whether a connector token really holds
// the tunnel it names.
//
// A per-Gateway data plane's tunnel identity is parsed from a connector token
// the tenant supplies, and the controller then writes that tunnel's ingress
// document with an API credential the tenant may not own. The token alone
// proves nothing: it is base64 JSON whose tunnel UUID anyone can write. What a
// tenant cannot write is the tunnel's secret, so the proof is fetching the
// tunnel's real token from Cloudflare and comparing the secrets.
package tunnelproof

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/zero_trust"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnel"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/tunnelownership"
)

const (
	// verifiedTTL bounds how long a proof stands before it is asked again,
	// which is how a rotated tunnel secret or a deleted tunnel is noticed.
	verifiedTTL = time.Hour
	// refutedTTL keeps a failing claim from costing an API call on every
	// reconcile, while letting a fix (a corrected token or credential) land
	// within minutes.
	refutedTTL = 5 * time.Minute
	// requestTimeout keeps an unresponsive API from holding a reconcile.
	requestTimeout = 10 * time.Second
)

// ClientFactory builds a Cloudflare API client for one API token.
type ClientFactory func(apiToken string) *cloudflare.Client

// Verifier checks tunnel claims against the Cloudflare API and caches the
// verdicts. One Verifier is shared by every layer that arbitrates tunnel
// ownership, so they read the same verdicts.
type Verifier struct {
	newClient ClientFactory
	now       func() time.Time

	mu sync.Mutex
	// cache is keyed by a digest of the claim, never by the secret itself.
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
// names, asking Cloudflare with apiToken — the credential that will write the
// tunnel's configuration, so the proof covers exactly the access used.
//
// Only a definite answer changes a verdict. An outage returns
// ProofUnknown for a claim never verified, and keeps ProofVerified for one
// that was, however long ago: a Cloudflare outage must not evict a holder.
func (v *Verifier) Verify(ctx context.Context, apiToken string, token *tunnel.Token) tunnelownership.Proof {
	key := cacheKey(token)

	v.mu.Lock()
	cached, found := v.cache[key]
	v.mu.Unlock()

	if found && v.now().Sub(cached.at) < ttl(cached.proof) {
		return cached.proof
	}

	proof := v.ask(ctx, apiToken, token)

	if proof == tunnelownership.ProofUnknown {
		if found && cached.proof == tunnelownership.ProofVerified {
			return tunnelownership.ProofVerified
		}

		return tunnelownership.ProofUnknown
	}

	v.store(key, proof)

	return proof
}

// store records a definite verdict and drops expired refutations, which is
// what bounds the cache: a tenant cycling made-up secrets leaves only entries
// that expire, while verified entries need the real secret to create.
func (v *Verifier) store(key [sha256.Size]byte, proof tunnelownership.Proof) {
	now := v.now()

	v.mu.Lock()
	defer v.mu.Unlock()

	for existing, cached := range v.cache {
		if cached.proof == tunnelownership.ProofRefuted && now.Sub(cached.at) >= refutedTTL {
			delete(v.cache, existing)
		}
	}

	v.cache[key] = entry{proof: proof, at: now}
}

// ask queries Cloudflare for the tunnel's token and compares it with the
// claimant's.
func (v *Verifier) ask(ctx context.Context, apiToken string, token *tunnel.Token) tunnelownership.Proof {
	if apiToken == "" {
		return tunnelownership.ProofUnknown
	}

	logger := log.FromContext(ctx).WithValues("tunnel", token.TunnelID.String())

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	real, err := v.newClient(apiToken).ZeroTrust.Tunnels.Cloudflared.Token.Get(ctx, token.TunnelID.String(),
		zero_trust.TunnelCloudflaredTokenGetParams{AccountID: cloudflare.F(token.AccountTag)})
	if err != nil {
		if isDefiniteRefusal(err) {
			logger.Info("Cloudflare refused the tunnel token lookup for a tunnel claim", "error", err.Error())

			return tunnelownership.ProofRefuted
		}

		logger.Info("could not verify a tunnel claim; Cloudflare API unavailable", "error", err.Error())

		return tunnelownership.ProofUnknown
	}

	parsed, err := tunnel.ParseTunnelToken(*real)
	if err != nil {
		logger.Info("could not verify a tunnel claim; Cloudflare returned an unparsable token")

		return tunnelownership.ProofUnknown
	}

	if parsed.AccountTag != token.AccountTag || parsed.TunnelID != token.TunnelID ||
		subtle.ConstantTimeCompare(parsed.TunnelSecret, token.TunnelSecret) != 1 {
		logger.Info("a tunnel claim's connector token does not match the tunnel's secret")

		return tunnelownership.ProofRefuted
	}

	return tunnelownership.ProofVerified
}

// isDefiniteRefusal reports whether Cloudflare answered the lookup with a
// client error — no such tunnel in that account, or a credential without
// access to it. Throttling and timeouts are not answers.
func isDefiniteRefusal(err error) bool {
	apiErr, ok := errors.AsType[*cloudflare.Error](err)
	if !ok {
		return false
	}

	status := apiErr.StatusCode

	return status >= http.StatusBadRequest && status < http.StatusInternalServerError &&
		status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
}

func ttl(proof tunnelownership.Proof) time.Duration {
	if proof == tunnelownership.ProofVerified {
		return verifiedTTL
	}

	return refutedTTL
}

// cacheKey digests the claim's identity and secret. The secret goes into the
// digest so a rotated token is a new claim, and never into the map itself.
func cacheKey(token *tunnel.Token) [sha256.Size]byte {
	hash := sha256.New()
	hash.Write([]byte(token.AccountTag))
	hash.Write([]byte{0})
	hash.Write(token.TunnelID[:])
	hash.Write(token.TunnelSecret)

	var key [sha256.Size]byte

	copy(key[:], hash.Sum(nil))

	return key
}
