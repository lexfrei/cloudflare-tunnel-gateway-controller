package docsdrift_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// boolValueRow matches a helm-values.md table row that documents a bool value
// with a backtick-quoted default, e.g. `| `proxy.networkPolicy.enabled` | bool | `true` | ... |`.
var boolValueRow = regexp.MustCompile("^\\| `([^`]+)` \\| bool \\| `(true|false)` \\|")

// TestHelmValuesDocBoolDefaultsMatchValues guards the hand-maintained
// docs/configuration/helm-values.md table against values.yaml for every bool
// row it documents. The chart README stays correct automatically via helm-docs,
// but this curated page is hand-written and silently drifted when
// proxy.networkPolicy.enabled flipped to true — the upgrade and metrics guides
// said ON while this page still said OFF. Pin the bool defaults so a future
// default flip must update the doc in the same change.
func TestHelmValuesDocBoolDefaultsMatchValues(t *testing.T) {
	t.Parallel()

	repoRoot := findRepoRoot(t)

	valuesRaw, err := os.ReadFile(filepath.Join(repoRoot,
		"charts", "cloudflare-tunnel-gateway-controller", "values.yaml"))
	if err != nil {
		t.Fatalf("reading values.yaml: %v", err)
	}

	var values map[string]any

	err = yaml.Unmarshal(valuesRaw, &values)
	if err != nil {
		t.Fatalf("parsing values.yaml: %v", err)
	}

	docRaw, err := os.ReadFile(filepath.Join(repoRoot, "docs", "configuration", "helm-values.md"))
	if err != nil {
		t.Fatalf("reading helm-values.md: %v", err)
	}

	checked := 0

	for line := range strings.SplitSeq(string(docRaw), "\n") {
		match := boolValueRow.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		key, documented := match[1], match[2]

		actual, ok := lookupBool(values, strings.Split(key, "."))
		if !ok {
			t.Errorf("helm-values.md documents %q as a bool, but it is absent or non-bool in values.yaml", key)

			continue
		}

		if strconv.FormatBool(actual) != documented {
			t.Errorf("helm-values.md says %q defaults to %q, but values.yaml has %v", key, documented, actual)
		}

		checked++
	}

	if checked == 0 {
		t.Fatal("no bool value rows matched — the guard regex or the doc table format drifted")
	}
}

// lookupBool walks a dotted key path through the parsed values tree and returns
// the bool leaf if the path resolves to one.
func lookupBool(node any, path []string) (bool, bool) {
	for _, segment := range path {
		asMap, ok := node.(map[string]any)
		if !ok {
			return false, false
		}

		node, ok = asMap[segment]
		if !ok {
			return false, false
		}
	}

	value, ok := node.(bool)

	return value, ok
}
