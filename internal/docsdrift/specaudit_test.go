package docsdrift_test

// Guard against the spec-audit matrices contradicting themselves: when a
// clause's verdict cell is flipped to a TESTED state, the prose in the same
// row must stop claiming the behaviour is untested. This is exactly the
// regression class produced when verdicts get updated and the notes do not.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// untestedClaims are phrases that contradict a *-TESTED verdict when found
// in the same table row.
var untestedClaims = []string{
	"no test",
	"No test",
	"No dedicated test",
	"untested",
	"UNTESTED",
}

func TestSpecAuditTestedVerdictsCarryNoUntestedProse(t *testing.T) {
	t.Parallel()

	repoRoot := findRepoRoot(t)
	auditDir := filepath.Join(repoRoot, "docs", "gateway-api", "_spec-audit")

	entries, err := os.ReadDir(auditDir)
	if err != nil {
		t.Fatalf("read spec-audit dir: %v", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		body, readErr := os.ReadFile(filepath.Join(auditDir, entry.Name()))
		if readErr != nil {
			t.Fatalf("read %s: %v", entry.Name(), readErr)
		}

		for i, line := range strings.Split(string(body), "\n") {
			if !strings.HasPrefix(line, "|") || !strings.Contains(line, "HONOURED-TESTED") {
				continue
			}

			for _, claim := range untestedClaims {
				// The verdict cell itself may legitimately read
				// "(was HONOURED-UNTESTED)" -- only flag claims that assert
				// the present-tense absence of a test.
				if strings.Contains(line, claim) && !strings.Contains(line, "was HONOURED-UNTESTED") {
					t.Errorf("%s:%d row carries a *-TESTED verdict but its prose still claims %q -- update the notes to cite the test:\n  %.180s",
						entry.Name(), i+1, claim, line)

					break
				}
			}
		}
	}
}
