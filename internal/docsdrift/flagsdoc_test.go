package docsdrift_test

import (
	"os"
	"regexp"
	"testing"
)

// flagDefinition matches every CLI flag registered on the controller's root
// command: rootCmd.Flags().String("name", ...), .Bool, .Float64, .StringSlice…
var flagDefinition = regexp.MustCompile(`rootCmd\.Flags\(\)\.\w+\("([a-z0-9-]+)"`)

// TestControllerFlagsDocumented locks the controller flag surface to the
// configuration reference: every flag registered in cmd/controller/cmd/root.go
// must appear (as --flag-name) in docs/configuration/controller.md. A flag
// shipped without its reference row is how per-Gateway data planes silently
// fail to render on manual installs (--proxy-image) — the operator has no
// discoverable record the flag exists.
func TestControllerFlagsDocumented(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile("../../cmd/controller/cmd/root.go")
	if err != nil {
		t.Fatalf("reading root.go: %v", err)
	}

	doc, err := os.ReadFile("../../docs/configuration/controller.md")
	if err != nil {
		t.Fatalf("reading controller.md: %v", err)
	}

	matches := flagDefinition.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("no flag definitions found in root.go — the extraction regex has drifted from the registration style")
	}

	// Hidden+deprecated flags are deliberately undocumented: they exist only
	// so old manifests keep starting, and documenting them would advertise
	// removal candidates.
	undocumented := map[string]bool{
		"gateway-class-name": true,
	}

	docText := string(doc)

	for _, match := range matches {
		if undocumented[match[1]] {
			continue
		}

		flag := "--" + match[1]
		if !regexp.MustCompile(regexp.QuoteMeta(flag) + `\b`).MatchString(docText) {
			t.Errorf("flag %s is registered in cmd/controller/cmd/root.go but missing from docs/configuration/controller.md", flag)
		}
	}
}
