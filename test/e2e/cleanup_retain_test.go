//go:build e2e

package e2e

import (
	"os"
	"testing"
)

// skipCleanupEnvVar is the opt-in env knob: when set to any non-empty
// value AND the test failed, cleanup helpers in this package return
// early instead of deleting the test resources. Lets a maintainer keep
// the cluster state around for post-mortem inspection (`kubectl get
// httproute -A`, proxy log inspection, etc.) without manually fishing
// the right `--skip-cleanup` flag into every test.
//
// Scope of retention -- per-subtest predicate, NOT per-subtest
// resource isolation. Each `t.Run` receives a fresh `*testing.T`
// whose `Failed()` reports only that subtest's own failures, never
// a sibling's (pinned by runDeferObservationHelper). So the
// PREDICATE fires per-failing-subtest. But the CLEANUP it gates --
// deleteAllRoutes -- is a blanket namespace wipe: it deletes every
// HTTPRoute in cfg.TestNamespace regardless of which subtest
// created it.
//
// Operational consequence in a full-suite run:
//
//  1. Subtest A fails. A's defer skips (predicate=true). A's
//     routes stay in the namespace.
//  2. Subtest B runs next, creates its own routes, passes.
//  3. B's defer runs (predicate=false because B passed) and
//     deleteAllRoutes wipes the entire namespace -- including
//     A's retained routes.
//
// Only state from the LAST failing subtest after the final passing
// sibling survives the run. In other words, the env var is mostly
// useful when paired with `-run TestName/SubtestName` so the
// failing subtest is the only one that touches the namespace.
// Running the full suite with the env set and expecting to inspect
// an early failure's state is the trap; use `-run` to avoid it.
//
// (Issue #265 tracks the larger refactor: scoping deleteAllRoutes
// to a subtest-owned label set so siblings can't wipe each other's
// state. Documenting the limitation here is the conservative fix.)
//
// Cross-invocation. Retention is scoped to a single `go test`
// process. The next invocation's clean-slate `deleteAllRoutes` at
// the top of `TestHTTPRouteConformance` runs with the parent's
// `t.Failed() == false` and wipes anything left over -- intentional,
// so a follow-up run starts from a known state once the operator has
// finished inspecting. `kubectl` against the retained state must
// happen between the failing run and the next `go test` invocation.
//
// Documented in docs/development/testing.md under "E2E Environment
// Variables"; that doc row is asserted by TestEnvVarDocumented in
// this file so the knob can't silently drop out of the docs.
const skipCleanupEnvVar = "E2E_SKIP_CLEANUP_ON_FAILURE"

// shouldSkipCleanupOnFailure is the pure predicate behind
// skipCleanupOnFailure. Extracted so it can be unit-tested without
// having to artificially fail the surrounding test (which would also
// taint the harness state). Returns true only when BOTH conditions
// hold: the test recorded a failure AND the opt-in env var is set.
//
// Defaulting cleanup to "always run unless explicitly opted out"
// keeps CI from accumulating dangling cluster resources on flaky
// runs while still letting a developer who's actively debugging
// preserve the failed state.
func shouldSkipCleanupOnFailure(failed bool, skipFailEnv string) bool {
	return failed && skipFailEnv != ""
}

// skipCleanupOnFailure inspects the current test's failure status and
// the opt-in env var and reports whether the caller should retain
// resources for post-mortem. Logs the decision when retaining so the
// reason shows up in the test output next to the failure itself.
func skipCleanupOnFailure(t *testing.T) bool {
	t.Helper()

	if !shouldSkipCleanupOnFailure(t.Failed(), os.Getenv(skipCleanupEnvVar)) {
		return false
	}

	t.Logf("test failed and %s is set -- retaining resources for post-mortem inspection", skipCleanupEnvVar)

	return true
}
