package controller

import (
	"crypto/sha256"
	"slices"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/zero_trust"

	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/config"
	"github.com/lexfrei/cloudflare-tunnel-gateway-controller/internal/ingress"
)

// tunnelDocumentTTL bounds how long a cached tunnel document is trusted
// without reading it back, which is how long an edit made outside the
// controller, such as in the dashboard, can survive a sync.
const tunnelDocumentTTL = 5 * time.Minute

// tunnelDocumentKey names a tunnel together with the credentials its document
// was read or written with. The credentials are compared, not keyed on, so a
// rotated credential misses instead of leaving an entry behind.
type tunnelDocumentKey struct {
	tunnelID    string
	tokenDigest [sha256.Size]byte
	accountID   string
}

// tunnelDocument is one RouteSyncer.documents entry.
type tunnelDocument struct {
	key      tunnelDocumentKey
	rules    []ingress.Rule
	storedAt time.Time
}

func documentKey(resolved *config.ResolvedConfig, accountID string) tunnelDocumentKey {
	return tunnelDocumentKey{
		tunnelID:    resolved.TunnelID,
		tokenDigest: sha256.Sum256([]byte(resolved.APIToken)),
		accountID:   accountID,
	}
}

// documentDeployed reports whether the cache says the tunnel already carries
// rules, as read or written with key's credentials within the TTL.
func (s *RouteSyncer) documentDeployed(
	key tunnelDocumentKey,
	rules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
) bool {
	entry, ok := s.documents[key.tunnelID]

	return ok &&
		entry.key == key &&
		s.clock().Sub(entry.storedAt) < tunnelDocumentTTL &&
		slices.Equal(entry.rules, documentRules(rules))
}

func (s *RouteSyncer) storeDocument(
	key tunnelDocumentKey,
	rules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress,
) {
	if s.documents == nil {
		s.documents = make(map[string]tunnelDocument)
	}

	s.documents[key.tunnelID] = tunnelDocument{key: key, rules: documentRules(rules), storedAt: s.clock()}
}

func (s *RouteSyncer) clock() time.Time {
	if s.now != nil {
		return s.now()
	}

	return time.Now()
}

func documentRules(rules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress) []ingress.Rule {
	converted := make([]ingress.Rule, len(rules))
	for i := range rules {
		converted[i] = ingress.RuleFromUpdate(&rules[i])
	}

	return converted
}
