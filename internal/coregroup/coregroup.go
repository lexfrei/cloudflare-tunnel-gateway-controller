// Package coregroup holds the one rule for recognising the Kubernetes core API
// group in a reference that tolerates its non-canonical spelling.
//
// Gateway API spells the core group only as the empty string. This project
// also accepts "core" for backendRefs, BackendTLSPolicy CA certificate refs and
// the ReferenceGrant entries authorising them, and every such site asks Is.
// Secret references accept only the empty string and do not use this package;
// docs/gateway-api/limitations.md lists which reference accepts which spelling.
package coregroup

// Is reports whether group names the core API group: the canonical empty
// string, or the tolerated "core".
func Is(group string) bool {
	return group == "" || group == "core"
}
