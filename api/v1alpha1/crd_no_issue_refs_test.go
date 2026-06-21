package v1alpha1_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// issueRefPattern matches a GitHub-style issue/PR reference (e.g. #110).
var issueRefPattern = regexp.MustCompile(`#\d+`)

// TestShippedCRDsHaveNoIssueReferences guards the shipped CRDs against issue/PR
// references leaking into user-facing API descriptions. controller-gen copies
// the Go doc comments on the CRD types verbatim into the CRD `description`
// fields, which users read via `kubectl explain` / `kubectl get crd -o yaml`;
// this repo forbids issue references in public/shipped content. The only way to
// keep the descriptions clean is to keep the source comments clean, so scan the
// generated artifact to catch a regression at its visible surface.
func TestShippedCRDsHaveNoIssueReferences(t *testing.T) {
	t.Parallel()

	crdDir := filepath.Join("..", "..", "charts", "cloudflare-tunnel-gateway-controller", "crds")

	entries, err := os.ReadDir(crdDir)
	require.NoError(t, err)

	var scanned int

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}

		raw, err := os.ReadFile(filepath.Join(crdDir, entry.Name()))
		require.NoError(t, err)

		scanned++

		for i, line := range strings.Split(string(raw), "\n") {
			match := issueRefPattern.FindString(line)
			require.Empty(t, match,
				"%s:%d carries an issue/PR reference %q — strip it from the source Go doc comment and re-run controller-gen",
				entry.Name(), i+1, match)
		}
	}

	require.Positive(t, scanned, "no CRD files were scanned")
}
