// Package tunnelhost resolves the Cloudflare edge hostname the conformance and
// e2e suites route requests through. Both suites need the same fail-fast
// contract — there is deliberately no built-in default, because a hardcoded
// test hostname silently routed the suites at a stale tunnel — and differ only
// in which env keys they accept. One shared resolver keeps that contract in a
// single place instead of two copies that drift.
package tunnelhost

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// errUnset is the base error returned when none of the candidate env keys is
// set. Resolve wraps it with the specific keys it tried so the message names
// what to export.
var errUnset = errors.New(
	"tunnel hostname not set; see .env.example (hack/conformance-setup.sh sources it from .env via V2_TUNNEL_HOSTNAME)")

// Resolve returns the first non-empty value among the given env keys, checked
// in order. It errors when none is set so callers can fail fast with a clear
// message instead of routing every request at an empty host. There is no
// default by design.
func Resolve(keys ...string) (string, error) {
	return resolveFrom(os.Getenv, keys...)
}

// resolveFrom is Resolve with an injected getenv so the resolution logic is
// unit-testable without mutating process env.
func resolveFrom(getenv func(string) string, keys ...string) (string, error) {
	for _, key := range keys {
		if value := getenv(key); value != "" {
			return value, nil
		}
	}

	return "", fmt.Errorf("%w: set one of %s", errUnset, strings.Join(keys, ", "))
}
