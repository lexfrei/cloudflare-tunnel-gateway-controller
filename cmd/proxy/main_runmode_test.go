package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runModeHelperEnv makes the test re-enter itself as a child process. The two
// run modes cannot be called in-process: one exits, the other serves forever.
const runModeHelperEnv = "TEST_PROXY_RUN_MODE_HELPER"

// runModeChildTimeout bounds the child so a mode that neither exits nor logs
// fails the test instead of hanging it.
const runModeChildTimeout = 30 * time.Second

// childEnv returns the parent environment with the proxy variables this suite
// controls removed, then the given ones applied. Stripping first matters:
// PROXY_AUTH_TOKEN inherited from an outer shell would defeat the whole point.
func childEnv(t *testing.T, extra ...string) []string {
	t.Helper()

	out := make([]string, 0, len(os.Environ())+len(extra)+1)

	for _, entry := range os.Environ() {
		switch {
		case strings.HasPrefix(entry, "PROXY_"), strings.HasPrefix(entry, "TUNNEL_TOKEN="):
		default:
			out = append(out, entry)
		}
	}

	return append(append(out, runModeHelperEnv+"=1"), extra...)
}

// TestRunModeWiring_TunnelModeRefusesAnUnauthenticatedConfigAPI pins the call
// site that decides whether the internet-facing mode may run open.
//
// Every other test in this package reaches resolveAuthToken directly, so
// swapping authRequired and authOptional between the two run modes would ship
// exactly the hole this change closes with the whole suite green. Reaching the
// call sites means running the binary, hence the child process; buildDataPlane
// runs before anything touches the network, so the refusal is offline and
// deterministic even though the token is garbage.
func TestRunModeWiring_TunnelModeRefusesAnUnauthenticatedConfigAPI(t *testing.T) {
	if os.Getenv(runModeHelperEnv) == "1" {
		main()

		return
	}

	ctx, cancel := context.WithTimeout(t.Context(), runModeChildTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=TestRunModeWiring_TunnelModeRefusesAnUnauthenticatedConfigAPI")
	cmd.Env = childEnv(t, "TUNNEL_TOKEN=not-a-real-token")

	out, err := cmd.CombinedOutput()

	require.Error(t, err, "tunnel mode must exit non-zero without a config-API token, got output:\n%s", out)
	assert.Contains(t, string(out), "refusing to start with a broken config-API auth configuration",
		"the refusal must name itself, since an operator meets it as a crash loop")
}

// TestRunModeWiring_StandaloneModeStartsWithoutAuth pins the other half. If
// standalone were wired to authRequired instead, a development run with no
// token would refuse to start rather than warn.
func TestRunModeWiring_StandaloneModeStartsWithoutAuth(t *testing.T) {
	if os.Getenv(runModeHelperEnv) == "1" {
		main()

		return
	}

	ctx, cancel := context.WithTimeout(t.Context(), runModeChildTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=TestRunModeWiring_StandaloneModeStartsWithoutAuth")
	// Port 0 lets the kernel assign, so a parallel run cannot collide.
	cmd.Env = childEnv(t, "PROXY_CONFIG_ADDR=127.0.0.1:0", "PROXY_ADDR=127.0.0.1:0")

	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)

	cmd.Stderr = cmd.Stdout

	require.NoError(t, cmd.Start())

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	found := make(chan bool, 1)

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "config API running WITHOUT authentication") {
				found <- true

				return
			}
		}

		// A scan error means the child died; the absence of the line is the
		// answer either way.
		found <- false
	}()

	select {
	case ok := <-found:
		assert.True(t, ok, "standalone mode must start unauthenticated rather than refuse")
	case <-time.After(runModeChildTimeout):
		t.Fatal("standalone mode neither warned nor exited within the timeout")
	}
}
