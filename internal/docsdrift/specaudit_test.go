package docsdrift_test

// Guard against the spec-audit matrices contradicting themselves: when a
// clause's verdict cell is flipped to a TESTED state, the prose in the same
// row must stop claiming the behaviour is untested. This is exactly the
// regression class produced when verdicts get updated and the notes do not.

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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

// auditRowID matches a clause row in the inventory and the verdict files, the
// same shape the matrix's Method section greps for.
var auditRowID = regexp.MustCompile(`^\| ([A-Z]+-[0-9]+) \|`)

// dashboardRow matches a status row of the matrix dashboard table.
var dashboardRow = regexp.MustCompile(`^\| (MET|PARTIAL|GAP|N/A)\b[^|]* \| ([0-9]+) \|$`)

// dashboardTotal matches the clause total in the dashboard heading.
var dashboardTotal = regexp.MustCompile(`(?m)^## Dashboard \(([0-9]+) clauses`)

// TestSpecAuditDashboardMatchesRows recomputes the matrix dashboard from the
// verdict rows it summarises, and checks that the verdict rows cover exactly
// the inventory plus the cross-cutting GEP rows, which only the verdict files
// carry.
func TestSpecAuditDashboardMatchesRows(t *testing.T) {
	t.Parallel()

	auditDir := filepath.Join(findRepoRoot(t), "docs", "gateway-api", "_spec-audit")

	verdicts := map[string]int{}
	rowIDs := map[string]string{}

	rowFiles, err := filepath.Glob(filepath.Join(auditDir, "rows-*.md"))
	if err != nil || len(rowFiles) == 0 {
		t.Fatalf("no rows-*.md under %s (err %v)", auditDir, err)
	}
	for _, file := range rowFiles {
		for _, line := range readLines(t, file) {
			match := auditRowID.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			if previous, dup := rowIDs[match[1]]; dup {
				t.Errorf("%s appears in both %s and %s", match[1], previous, filepath.Base(file))
			}
			rowIDs[match[1]] = filepath.Base(file)

			cells := strings.Split(line, "|")
			if len(cells) < 5 {
				t.Fatalf("%s: row %s has no status column", filepath.Base(file), match[1])
			}
			verdicts[strings.TrimSpace(cells[4])]++
		}
	}

	inventory := map[string]bool{}
	for _, line := range readLines(t, filepath.Join(auditDir, "01-clause-inventory.md")) {
		if match := auditRowID.FindStringSubmatch(line); match != nil {
			inventory[match[1]] = true
		}
	}

	for _, id := range slices.Sorted(maps.Keys(rowIDs)) {
		if !inventory[id] && !strings.HasPrefix(id, "GEP-") {
			t.Errorf("%s (%s) has a verdict but no row in 01-clause-inventory.md", id, rowIDs[id])
		}
	}
	for _, id := range slices.Sorted(maps.Keys(inventory)) {
		if rowIDs[id] == "" {
			t.Errorf("%s is in 01-clause-inventory.md but no rows-*.md file classifies it", id)
		}
	}

	matrixPath := filepath.Join(auditDir, "00-compliance-matrix.md")
	body, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatalf("reading %s: %v", matrixPath, err)
	}

	total := dashboardTotal.FindStringSubmatch(string(body))
	if total == nil {
		t.Fatal("00-compliance-matrix.md has no '## Dashboard (N clauses' heading")
	}
	if total[1] != strconv.Itoa(len(rowIDs)) {
		t.Errorf("dashboard heading states %s clauses, rows-*.md classify %d", total[1], len(rowIDs))
	}

	stated := map[string]int{}
	for _, line := range strings.Split(string(body), "\n") {
		match := dashboardRow.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		count, convErr := strconv.Atoi(match[2])
		if convErr != nil {
			t.Fatalf("dashboard count %q: %v", match[2], convErr)
		}
		stated[strings.ReplaceAll(match[1], "/", "")] = count
	}
	if len(stated) == 0 {
		t.Fatal("found no dashboard status rows in 00-compliance-matrix.md, so nothing was compared")
	}
	for _, status := range slices.Sorted(maps.Keys(verdicts)) {
		if stated[status] != verdicts[status] {
			t.Errorf("dashboard states %d %s, rows-*.md hold %d", stated[status], status, verdicts[status])
		}
	}
	for _, status := range slices.Sorted(maps.Keys(stated)) {
		if _, ok := verdicts[status]; !ok {
			t.Errorf("dashboard states %d %s, rows-*.md hold none", stated[status], status)
		}
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	return strings.Split(string(body), "\n")
}
