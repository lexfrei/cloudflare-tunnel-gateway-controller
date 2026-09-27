package rbacdrift_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRootSecurityPolicyDefersToGuardedDoc keeps the root SECURITY.md from
// growing a second copy of the RBAC scope or the verification procedures.
// docs/reference/security.md carries both and its RBAC block is pinned to the
// shipped role by TestSecurityDocRBAC_MatchesDeployRole. A copy in the root
// file, the one GitHub shows as the repository's security policy, would drift
// with nothing watching it.
func TestRootSecurityPolicyDefersToGuardedDoc(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join(findRepoRoot(t), "SECURITY.md"))
	if err != nil {
		t.Fatalf("read SECURITY.md: %v", err)
	}

	text := string(body)

	for _, forbidden := range []string{"RBAC", "ClusterRole", "cosign verify", "helm verify"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("SECURITY.md mentions %q; describe it in docs/reference/security.md and link there instead", forbidden)
		}
	}

	const guardedDoc = "https://cf.k8s.lex.la/latest/reference/security/"
	if !strings.Contains(text, guardedDoc) {
		t.Errorf("SECURITY.md no longer links %s, the page that carries the hardening guidance", guardedDoc)
	}
}
