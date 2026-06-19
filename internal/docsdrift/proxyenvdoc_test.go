package docsdrift_test

import (
	"os"
	"regexp"
	"testing"
)

// proxyEnvVar matches every PROXY_* / TUNNEL_TOKEN environment-variable name the
// proxy binary reads, whether via os.Getenv or a helper (envOrDefault,
// parseEnvDuration, isTruthyEnv, …) — they all pass the name as a string
// literal.
var proxyEnvVar = regexp.MustCompile(`"(PROXY_[A-Z0-9_]+|TUNNEL_TOKEN)"`)

// TestProxyEnvVarsDocumented locks the proxy's environment-variable surface to
// its reference: every PROXY_* / TUNNEL_TOKEN name read in cmd/proxy/main.go
// must appear in docs/guides/l7-proxy.md. The proxy is configured entirely
// through env (the chart wires them from values), so an undocumented var is an
// operator-invisible knob — the proxy equivalent of the controller flag drift
// TestControllerFlagsDocumented guards.
func TestProxyEnvVarsDocumented(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile("../../cmd/proxy/main.go")
	if err != nil {
		t.Fatalf("reading cmd/proxy/main.go: %v", err)
	}

	doc, err := os.ReadFile("../../docs/guides/l7-proxy.md")
	if err != nil {
		t.Fatalf("reading l7-proxy.md: %v", err)
	}

	matches := proxyEnvVar.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("no proxy env vars found in cmd/proxy/main.go — the extraction regex has drifted")
	}

	docText := string(doc)
	seen := make(map[string]bool, len(matches))

	for _, match := range matches {
		name := match[1]
		if seen[name] {
			continue
		}

		seen[name] = true

		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(docText) {
			t.Errorf("env var %s is read in cmd/proxy/main.go but missing from docs/guides/l7-proxy.md", name)
		}
	}
}
