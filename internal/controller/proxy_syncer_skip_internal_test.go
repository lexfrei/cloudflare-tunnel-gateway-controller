package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestShouldSkipPush pins the skip-key semantics, most importantly that an
// empty hash NEVER matches: "" is simultaneously the hash-failure sentinel,
// the zero value before the first push, and the invalidation value after a
// partial push -- treating "" == "" as a match would skip pushes that must
// happen.
func TestShouldSkipPush(t *testing.T) {
	t.Parallel()

	endpoints := map[string]struct{}{"a": {}, "b": {}}
	resolved := []string{"a", "b"}

	const tok = "tok-1"

	assert.True(t, shouldSkipPush("h1", "h1", tok, tok, endpoints, resolved), "same hash, token, endpoints: skip")
	assert.False(t, shouldSkipPush("h2", "h1", tok, tok, endpoints, resolved), "hash changed: push")
	assert.False(t, shouldSkipPush("h1", "h1", tok, tok, endpoints, []string{"a"}), "endpoint set shrank: push")
	assert.False(t, shouldSkipPush("h1", "h1", tok, tok, endpoints, []string{"a", "c"}), "endpoint replaced: push")
	assert.False(t, shouldSkipPush("h1", "h1", "tok-2", tok, endpoints, resolved),
		"token rotated on unchanged routes/endpoints: push to re-authenticate")
	assert.False(t, shouldSkipPush("", "", tok, tok, nil, nil), "empty hash must never match, even against the zero value")
	assert.False(t, shouldSkipPush("", "", tok, tok, endpoints, resolved), "empty hash after invalidation must not match")
}
