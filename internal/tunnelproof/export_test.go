package tunnelproof

import (
	"encoding/hex"
	"time"
)

// SetClock replaces the verifier's time source.
func (v *Verifier) SetClock(now func() time.Time) {
	v.now = now
}

// CacheKeys returns the cache's keys as hex.
func (v *Verifier) CacheKeys() []string {
	v.mu.Lock()
	defer v.mu.Unlock()

	keys := make([]string, 0, len(v.cache))
	for key := range v.cache {
		keys = append(keys, hex.EncodeToString(key[:]))
	}

	return keys
}
