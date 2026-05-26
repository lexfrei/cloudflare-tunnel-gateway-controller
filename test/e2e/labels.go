//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// ownerLabelKey is the Kubernetes label key e2e helpers stamp on every
// test-owned resource (HTTPRoute, Service, ...) so that per-subtest
// cleanup can filter by it instead of wiping the whole namespace.
//
// Issue #265 documents the regression that motivated this: a blanket
// `client.List(InNamespace(...))` + `Delete` in deleteAllRoutes meant
// that a passing sibling's defer would happily wipe a failing
// subtest's resources, defeating E2E_SKIP_CLEANUP_ON_FAILURE. The
// label-scoped variant only deletes resources whose owner label
// matches the running subtest's hashed name, so siblings can't reach
// each other.
const ownerLabelKey = "e2e.cloudflare-tunnel/owner"

// subtestLabelValue returns the value to stamp under ownerLabelKey for
// a given t.Name() (or any string identifier). The output is the
// hex-encoded first 8 bytes of SHA-256 over the input -- 16 chars,
// always within Kubernetes' 63-char label-value cap, always matching
// the [a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])? regex.
//
// We hash rather than sanitise-and-truncate because:
//
//   - t.Name() contains "/" between subtest path segments, which is
//     illegal in a label value. Stripping `/` to `_` works for short
//     names but the resulting value can exceed 63 chars for the
//     deeper Extended/ subtests (e.g. HTTPRouteRegexQueryParamMatching).
//   - Hashing collapses both problems at once: any input shape, any
//     length, always a valid label.
//   - 8 bytes (64 bits) gives ~2^32 collision resistance over the test
//     suite's < 30 subtests -- many orders of magnitude of headroom.
//
// Determinism matters: deleteAllRoutes runs inside the SAME t whose
// name was used by createHTTPRoute, so re-hashing the same string
// here MUST return the same digest. SHA-256 satisfies that.
func subtestLabelValue(name string) string {
	sum := sha256.Sum256([]byte(name))

	return hex.EncodeToString(sum[:8])
}

// subtestLabels returns the per-subtest owner labels e2e helpers
// (createHTTPRoute, setupWSBackendService, ...) stamp onto resources
// at creation time and that deleteAllRoutes filters by at cleanup
// time. The map is freshly allocated per call so callers can mutate
// it (e.g. add app-specific labels) without poisoning the shared
// reference.
func subtestLabels(t *testing.T) map[string]string {
	t.Helper()

	return map[string]string{ownerLabelKey: subtestLabelValue(t.Name())}
}
