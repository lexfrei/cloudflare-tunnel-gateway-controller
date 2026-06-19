package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestUnmarshalableMatchKey_KeysByContentNotPointer pins that the
// canonicalMatchKey json-failure fallback keys by CONTENT: two spec-identical
// matches with DISTINCT *PathMatch pointers must produce the same key. A bare
// %#v would render the pointer address, so the two would diverge and the shadow
// detector would silently miss their collision.
func TestUnmarshalableMatchKey_KeysByContentNotPointer(t *testing.T) {
	t.Parallel()

	a := RouteMatch{Path: &PathMatch{Type: PathMatchExact, Value: "/v1"}, Method: "GET"}
	bEqual := RouteMatch{Path: &PathMatch{Type: PathMatchExact, Value: "/v1"}, Method: "GET"}

	assert.Equal(t, unmarshalableMatchKey(a), unmarshalableMatchKey(bEqual),
		"equal matches with distinct Path pointers must produce the same fallback key")

	cDiff := RouteMatch{Path: &PathMatch{Type: PathMatchExact, Value: "/v2"}, Method: "GET"}
	assert.NotEqual(t, unmarshalableMatchKey(a), unmarshalableMatchKey(cDiff),
		"matches differing only in Path content must produce distinct fallback keys")

	nilPath := RouteMatch{Method: "GET"}
	assert.NotEqual(t, unmarshalableMatchKey(a), unmarshalableMatchKey(nilPath),
		"a nil Path must not collide with a populated one")
}

// TestCanonicalMatchKey_HeaderValueCaseSensitive pins that header VALUES stay
// case-sensitive in the shadow identity while header NAMES are normalized to
// lowercase (HTTP header names are case-insensitive, values are not). Two
// routes matching the same header name but a different value case are distinct
// matches and must NOT be reported as shadowing each other; the same name in a
// different case IS the same match.
func TestCanonicalMatchKey_HeaderValueCaseSensitive(t *testing.T) {
	t.Parallel()

	lowerValue := RouteMatch{Headers: []HeaderMatch{{Type: HeaderMatchExact, Name: "X-Env", Value: "prod"}}}
	upperValue := RouteMatch{Headers: []HeaderMatch{{Type: HeaderMatchExact, Name: "X-Env", Value: "PROD"}}}

	assert.NotEqual(t, canonicalMatchKey(lowerValue), canonicalMatchKey(upperValue),
		"header values are case-sensitive: matches differing only in value case must not collide")

	lowerName := RouteMatch{Headers: []HeaderMatch{{Type: HeaderMatchExact, Name: "x-env", Value: "prod"}}}

	assert.Equal(t, canonicalMatchKey(lowerValue), canonicalMatchKey(lowerName),
		"header names are case-insensitive: matches differing only in name case must collide")
}
